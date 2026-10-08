// Package pgstore is a PostgreSQL idempotency.Store, plus [DoTx], which
// makes an operation's database writes and its idempotency record commit
// together.
//
// Create the table once, for example in a migration:
//
//	_, err := db.Exec(ctx, pgstore.Schema)
//
// Expired rows are not deleted automatically; run [Store.DeleteExpired]
// periodically.
package pgstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
)

// Schema creates the default table. Use [WithTable] for another name and
// adapt this statement accordingly.
const Schema = `CREATE TABLE IF NOT EXISTS idempotency_keys (
	key        text        PRIMARY KEY,
	state      text        NOT NULL,
	token      text        NOT NULL,
	result     bytea,
	expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at ON idempotency_keys (expires_at);`

// Querier is satisfied by *pgxpool.Pool, *postgres.DB and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store implements idempotency.Store in a PostgreSQL table. Expiry uses the
// database clock only.
type Store struct {
	db    Querier
	table string
}

// Option configures [New].
type Option func(*Store)

// WithTable uses table instead of "idempotency_keys". table is placed in
// SQL as is; it must be a trusted identifier, never user input.
func WithTable(table string) Option { return func(s *Store) { s.table = table } }

// New returns a store using db.
func New(db Querier, opts ...Option) *Store {
	s := &Store{db: db, table: "idempotency_keys"}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Store) sql(q string) string { return strings.ReplaceAll(q, "{t}", s.table) }

const claimSQL = `INSERT INTO {t} AS t (key, state, token, result, expires_at)
VALUES ($1, 'in_progress', $2, NULL, now() + make_interval(secs => $3))
ON CONFLICT (key) DO UPDATE
	SET state = 'in_progress', token = EXCLUDED.token, result = NULL, expires_at = EXCLUDED.expires_at
	WHERE t.expires_at < now()
RETURNING token`

const selectSQL = `SELECT state, result FROM {t} WHERE key = $1 AND expires_at >= now()`

// Begin implements idempotency.Store.
func (s *Store) Begin(ctx context.Context, key string, lease time.Duration) (string, bool, idempotency.Record, error) {
	return begin(ctx, s.db, s, key, lease)
}

func begin(ctx context.Context, q Querier, s *Store, key string, lease time.Duration) (string, bool, idempotency.Record, error) {
	for range 2 { // the row may expire between the two statements; retry once
		tok := idempotency.NewToken()
		var got string
		err := q.QueryRow(ctx, s.sql(claimSQL), key, tok, lease.Seconds()).Scan(&got)
		if err == nil {
			return got, true, idempotency.Record{}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", false, idempotency.Record{}, postgres.Classify(err)
		}
		var rec idempotency.Record
		var state string
		err = q.QueryRow(ctx, s.sql(selectSQL), key).Scan(&state, &rec.Result)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, idempotency.Record{}, postgres.Classify(err)
		}
		rec.State = idempotency.State(state)
		return "", false, rec, nil
	}
	return "", false, idempotency.Record{}, errors.Unavailable.New("idempotency: key changing too fast")
}

const completeSQL = `UPDATE {t} SET state = 'completed', result = $3, expires_at = now() + make_interval(secs => $4)
WHERE key = $1 AND token = $2 AND expires_at >= now()`

// Complete implements idempotency.Store.
func (s *Store) Complete(ctx context.Context, key, token string, result []byte, ttl time.Duration) error {
	return complete(ctx, s.db, s, key, token, result, ttl)
}

func complete(ctx context.Context, q Querier, s *Store, key, token string, result []byte, ttl time.Duration) error {
	tag, err := q.Exec(ctx, s.sql(completeSQL), key, token, result, ttl.Seconds())
	if err != nil {
		return postgres.Classify(err)
	}
	if tag.RowsAffected() == 0 {
		return idempotency.ErrLeaseLost
	}
	return nil
}

// Release implements idempotency.Store.
func (s *Store) Release(ctx context.Context, key, token string) error {
	tag, err := s.db.Exec(ctx, s.sql(`DELETE FROM {t} WHERE key = $1 AND token = $2`), key, token)
	if err != nil {
		return postgres.Classify(err)
	}
	if tag.RowsAffected() == 0 {
		return idempotency.ErrLeaseLost
	}
	return nil
}

// DeleteExpired removes expired rows and returns how many.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, s.sql(`DELETE FROM {t} WHERE expires_at < now()`))
	if err != nil {
		return 0, postgres.Classify(err)
	}
	return tag.RowsAffected(), nil
}

// TxBeginner is satisfied by *pgxpool.Pool and *postgres.DB.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DoTx runs fn at most once per key, inside one transaction with the
// idempotency record: fn's writes through tx and the "completed" record
// commit together or not at all. For effects within this database that is
// exactly once, unlike idempotency.Do; effects outside it (calls to other
// services, messages) are still at least once.
//
// A concurrent call with the same key waits for the first transaction's
// row lock and then returns its result. If fn fails, the transaction rolls
// back and the key stays unclaimed. ttl is how long the result is kept.
func DoTx[T any](ctx context.Context, db TxBeginner, s *Store, key string, ttl time.Duration, fn func(ctx context.Context, tx pgx.Tx) (T, error)) (T, idempotency.Outcome, error) {
	var zero T
	tx, err := db.Begin(ctx)
	if err != nil {
		return zero, idempotency.Executed, postgres.Classify(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The lease only matters if this transaction somehow outlives it; the
	// row lock is what excludes concurrent calls.
	token, claimed, existing, err := begin(ctx, tx, s, key, ttl)
	if err != nil {
		return zero, idempotency.Executed, err
	}
	if !claimed {
		if existing.State != idempotency.Completed {
			return zero, idempotency.Executed, idempotency.ErrInProgress
		}
		var v T
		if err := json.Unmarshal(existing.Result, &v); err != nil {
			return zero, idempotency.Duplicate, errors.Internal.Wrap(err, "idempotency: decode stored result")
		}
		return v, idempotency.Duplicate, nil
	}

	v, err := fn(ctx, tx)
	if err != nil {
		return zero, idempotency.Executed, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return zero, idempotency.Executed, errors.Internal.Wrap(err, "idempotency: encode result")
	}
	if err := complete(ctx, tx, s, key, token, b, ttl); err != nil {
		return zero, idempotency.Executed, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, idempotency.Executed, postgres.Classify(err)
	}
	return v, idempotency.Executed, nil
}
