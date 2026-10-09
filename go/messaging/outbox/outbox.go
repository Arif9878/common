// Package outbox publishes Kafka records written in a PostgreSQL
// transaction, after that transaction commits: the transactional outbox
// pattern. A service that saves an order and publishes "order created"
// cannot otherwise do both atomically: publishing first can announce an
// order that then fails to save, and saving first can lose the event in a
// crash.
//
// Write the records in the same transaction as the business data:
//
//	box := outbox.New()
//	tx, err := db.Begin(ctx)
//	...
//	_, err = tx.Exec(ctx, `INSERT INTO orders ...`, ...)
//	r, err := kafka.Encode("orders.created", key, event, serde)
//	err = box.Write(ctx, tx, r)
//	err = tx.Commit(ctx)
//
// and run one [Relay] per service (more replicas are fine; one relays at a
// time):
//
//	relay := box.NewRelay(db, producer, outbox.WithLogger(logger))
//	shutdown.Go("outbox relay", func() error { return relay.Run(ctx) })
//	shutdown.Register(graceful.StopIntake, "outbox relay", relay.Stop)
//
// # Guarantees
//
// A record is published if and only if its transaction commits, at least
// once: the relay deletes records after Kafka acknowledges them, so a crash
// in between publishes them again. Write gives every record an "event-id"
// header ([EventIDHeader]) unless it has one, so consumers can drop the
// repeats with kafka.WithIdempotency and kafka.HeaderKey(outbox.EventIDHeader).
//
// Records are published in commit-independent insertion order (by a
// sequence), one batch at a time, by a single relay: the relay holds a
// PostgreSQL advisory lock while it runs, and the other replicas wait for
// it. Records of one Kafka partition therefore keep their order, except
// when a relay loses its database session in the middle of a batch and
// another takes over before the first notices.
//
// The relay wakes up on LISTEN/NOTIFY as soon as a transaction with outbox
// records commits, and polls every WithPollInterval in case a notification
// is missed.
//
// Create the table once, for example in a migration, with [Schema].
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
)

// Schema creates the default table. Use [WithTable] for another name and
// adapt this statement accordingly.
const Schema = `CREATE TABLE IF NOT EXISTS kafka_outbox (
	id         bigserial   PRIMARY KEY,
	topic      text        NOT NULL,
	key        bytea,
	value      bytea,
	headers    jsonb       NOT NULL DEFAULT '[]',
	created_at timestamptz NOT NULL DEFAULT now()
);`

// EventIDHeader is the record header Write sets to a random ID, so
// consumers can recognize a record published twice.
const EventIDHeader = "event-id"

// Execer is satisfied by pgx.Tx, *pgxpool.Pool and *postgres.DB. Pass the
// transaction that writes the business data.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Outbox writes records to an outbox table. It is safe for concurrent use.
type Outbox struct {
	table string
}

// Option configures [New].
type Option func(*Outbox)

// WithTable uses table instead of "kafka_outbox". It is placed in SQL as
// is; it must be a trusted identifier, never user input. It also names the
// notification channel, so relays of different tables do not wake each
// other.
func WithTable(table string) Option { return func(o *Outbox) { o.table = table } }

// New returns an Outbox.
func New(opts ...Option) *Outbox {
	o := &Outbox{table: "kafka_outbox"}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

type header struct {
	Key   string `json:"k"`
	Value []byte `json:"v"`
}

// Write inserts records into the outbox through tx. They are published
// after tx commits, and never if it rolls back. Each record without an
// [EventIDHeader] header gets one. Only Topic, Key, Value and Headers are
// kept; the partition is chosen by the producer when the record is
// published.
func (o *Outbox) Write(ctx context.Context, tx Execer, records ...*kgo.Record) error {
	if len(records) == 0 {
		return nil
	}
	topics := make([]string, len(records))
	keys := make([][]byte, len(records))
	values := make([][]byte, len(records))
	headers := make([]string, len(records))
	for i, r := range records {
		if r.Topic == "" {
			return errors.InvalidArgument.New("outbox: record without a topic")
		}
		hs := make([]header, 0, len(r.Headers)+1)
		hasID := false
		for _, h := range r.Headers {
			hs = append(hs, header{h.Key, h.Value})
			hasID = hasID || h.Key == EventIDHeader
		}
		if !hasID {
			hs = append(hs, header{EventIDHeader, []byte(rand.Text())})
		}
		b, err := json.Marshal(hs)
		if err != nil {
			return errors.InvalidArgument.Wrap(err, "outbox: encode headers")
		}
		topics[i], keys[i], values[i], headers[i] = r.Topic, r.Key, r.Value, string(b)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+o.table+` (topic, key, value, headers)
SELECT * FROM unnest($1::text[], $2::bytea[], $3::bytea[], $4::jsonb[])`, topics, keys, values, headers); err != nil {
		return errors.Join(errors.New("outbox: write"), postgres.Classify(err))
	}
	// Delivered when tx commits, which wakes the relay.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, '')`, o.table); err != nil {
		return errors.Join(errors.New("outbox: notify"), postgres.Classify(err))
	}
	return nil
}
