// Package pglock implements lock.Locker in PostgreSQL. Leases are rows in
// a table; expiry uses the database clock; fencing tokens come from a
// sequence, so they increase across all keys.
//
// Create the table and sequence once, for example in a migration:
//
//	_, err := db.Exec(ctx, pglock.Schema)
package pglock

import (
	"context"
	"crypto/rand"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lock"
)

// Schema creates the default table and fence sequence.
const Schema = `CREATE TABLE IF NOT EXISTS locks (
	key        text        PRIMARY KEY,
	token      text        NOT NULL,
	fence      bigint      NOT NULL,
	expires_at timestamptz NOT NULL
);
CREATE SEQUENCE IF NOT EXISTS locks_fence;`

// Querier is satisfied by *pgxpool.Pool and *postgres.DB.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Locker implements lock.Locker.
type Locker struct {
	db       Querier
	table    string
	sequence string
}

// Option configures [New].
type Option func(*Locker)

// WithTable uses table and sequence instead of "locks" and "locks_fence".
// They are placed in SQL as is and must be trusted identifiers.
func WithTable(table, sequence string) Option {
	return func(l *Locker) { l.table, l.sequence = table, sequence }
}

// New returns a Locker using db.
func New(db Querier, opts ...Option) *Locker {
	l := &Locker{db: db, table: "locks", sequence: "locks_fence"}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

func (l *Locker) sql(q string) string {
	return strings.NewReplacer("{t}", l.table, "{seq}", l.sequence).Replace(q)
}

const acquireSQL = `INSERT INTO {t} AS t (key, token, fence, expires_at)
VALUES ($1, $2, nextval('{seq}'), now() + make_interval(secs => $3))
ON CONFLICT (key) DO UPDATE
	SET token = EXCLUDED.token, fence = EXCLUDED.fence, expires_at = EXCLUDED.expires_at
	WHERE t.expires_at < now()
RETURNING fence`

// TryAcquire implements lock.Locker.
func (l *Locker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (*lock.Lease, error) {
	token := rand.Text()
	var fence int64
	err := l.db.QueryRow(ctx, l.sql(acquireSQL), key, token, ttl.Seconds()).Scan(&fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, lock.ErrNotAcquired
	}
	if err != nil {
		return nil, postgres.Classify(err)
	}
	return lock.NewLease(key, token, uint64(fence), ttl, //nolint:gosec // sequences are positive
		func(ctx context.Context) error {
			return l.affect(ctx, `DELETE FROM {t} WHERE key = $1 AND token = $2 AND expires_at >= now()`, key, token)
		},
		func(ctx context.Context, ttl time.Duration) error {
			return l.affect(ctx, `UPDATE {t} SET expires_at = now() + make_interval(secs => $3)
				WHERE key = $1 AND token = $2 AND expires_at >= now()`, key, token, ttl.Seconds())
		}), nil
}

func (l *Locker) affect(ctx context.Context, q string, args ...any) error {
	tag, err := l.db.Exec(ctx, l.sql(q), args...)
	if err != nil {
		return postgres.Classify(err)
	}
	if tag.RowsAffected() == 0 {
		return lock.ErrNotHeld
	}
	return nil
}
