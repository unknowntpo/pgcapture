package source

import (
	"context"
	"errors"
	"github.com/jackc/pgconn"
	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgproto3/v2"
	"github.com/jackc/pgx/v4"
	"github.com/rueian/pgcapture/pkg/decode"
	"github.com/rueian/pgcapture/pkg/sql"
	"log"
	"sync/atomic"
	"time"
)

type PGXSource struct {
	BaseSource

	SetupConnStr string
	ReplConnStr  string
	ReplSlot     string
	CreateSlot   bool

	setupConn *pgx.Conn
	replConn  *pgconn.PgConn

	schema  *decode.PGXSchemaLoader
	decoder *decode.PGLogicalDecoder

	nextReportTime time.Time

	ackLsn uint64
}

func (p *PGXSource) Setup() (err error) {
	ctx := context.Background()
	p.setupConn, err = pgx.Connect(ctx, p.SetupConnStr)
	if err != nil {
		return err
	}
	p.schema = decode.NewPGXSchemaLoader(p.setupConn)
	if err = p.schema.RefreshType(); err != nil {
		return err
	}

	p.decoder = decode.NewPGLogicalDecoder(p.schema)

	if _, err = p.setupConn.Exec(ctx, sql.InstallExtension); err != nil {
		return nil
	}

	if p.CreateSlot {
		_, err = p.setupConn.Exec(ctx, sql.CreateLogicalSlot, p.ReplSlot, OutputPlugin)
	}

	return err
}

func (p *PGXSource) Capture(cp Checkpoint) (changes chan Change, err error) {
	defer func() {
		if err != nil {
			p.cleanup()
		}
	}()

	p.replConn, err = pgconn.Connect(context.Background(), p.ReplConnStr)
	if err != nil {
		return nil, err
	}

	ident, err := pglogrepl.IdentifySystem(context.Background(), p.replConn)
	if err != nil {
		return nil, err
	}
	log.Println("SystemID:", ident.SystemID, "Timeline:", ident.Timeline, "XLogPos:", ident.XLogPos, "DBName:", ident.DBName)

	var requestLSN pglogrepl.LSN
	if cp.LSN != 0 {
		requestLSN = pglogrepl.LSN(cp.LSN)
		log.Println("start logical replication on slot with requested position", p.ReplSlot, requestLSN)
	} else {
		requestLSN = ident.XLogPos
		log.Println("start logical replication on slot with previous position", p.ReplSlot, requestLSN)
	}
	if err = pglogrepl.StartReplication(context.Background(), p.replConn, p.ReplSlot, requestLSN, pglogrepl.StartReplicationOptions{PluginArgs: pgLogicalParam}); err != nil {
		return nil, err
	}
	p.ackLsn = uint64(requestLSN)

	return p.BaseSource.capture(p.fetching, p.cleanup)
}

func (p *PGXSource) fetching(ctx context.Context) (change Change, err error) {
	if time.Now().After(p.nextReportTime) {
		if err = pglogrepl.SendStandbyStatusUpdate(ctx, p.replConn, pglogrepl.StandbyStatusUpdate{WALWritePosition: p.committedLSN()}); err != nil {
			return change, err
		}
		p.nextReportTime = time.Now().Add(5 * time.Second)
	}
	msg, err := p.replConn.ReceiveMessage(ctx)
	if err != nil {
		return change, err
	}
	switch msg := msg.(type) {
	case *pgproto3.CopyData:
		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			var pkm pglogrepl.PrimaryKeepaliveMessage
			if pkm, err = pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:]); err == nil && pkm.ReplyRequested {
				p.nextReportTime = time.Time{}
			}
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				return change, err
			}
			m, err := p.decoder.Decode(xld.WALData)
			if m == nil || err != nil {
				return change, err
			}
			if msg := m.GetChange(); msg != nil {
				if decode.Ignore(msg) {
					return change, nil
				} else if decode.IsDDL(msg) {
					if err = p.schema.RefreshType(); err != nil {
						return change, err
					}
				}
			}
			change = Change{
				Checkpoint: Checkpoint{LSN: uint64(xld.WALStart) + uint64(len(xld.WALData))},
				Message:    m,
			}
		}
	default:
		err = errors.New("unexpected message")
	}
	return change, err
}

func (p *PGXSource) Commit(cp Checkpoint) {
	atomic.StoreUint64(&p.ackLsn, cp.LSN)
}

func (p *PGXSource) committedLSN() (lsn pglogrepl.LSN) {
	return pglogrepl.LSN(atomic.LoadUint64(&p.ackLsn))
}

func (p *PGXSource) cleanup() {
	if p.setupConn != nil {
		p.setupConn.Close(context.Background())
	}
	if p.replConn != nil {
		p.replConn.Close(context.Background())
	}
}

const OutputPlugin = "pglogical_output"

var pgLogicalParam = []string{
	"min_proto_version '1'",
	"max_proto_version '1'",
	"startup_params_format '1'",
	"\"binary.want_binary_basetypes\" '1'",
	"\"binary.basetypes_major_version\" '906'",
	"\"binary.bigendian\" '1'",
}

func PGTime2Time(ts uint64) time.Time {
	micro := microsecFromUnixEpochToY2K + int64(ts)
	return time.Unix(micro/microInSecond, (micro%microInSecond)*nsInSecond)
}

const microInSecond = int64(1e6)
const nsInSecond = int64(1e3)
const microsecFromUnixEpochToY2K = int64(946684800 * 1000000)
