// Package migrate applies SQL schema migrations to PostgreSQL with goose
// (github.com/pressly/goose/v3), safely from every replica of a service:
// a session advisory lock lets one replica migrate while the others wait,
// then find nothing left to do.
//
//	//go:embed migrations/*.sql
//	var migrations embed.FS
//
//	sub, _ := fs.Sub(migrations, "migrations")
//	applied, err := migrate.Up(ctx, db, sub)
//
// Migrations are goose SQL files: 00001_create_orders.sql with
// "-- +goose Up" and "-- +goose Down" sections, each run in a transaction
// unless marked "-- +goose NO TRANSACTION" (for CREATE INDEX
// CONCURRENTLY). With commonfx, PostgresMigrations runs Up while the
// application starts, before it reports ready.
//
// Run migrations that rewrite large tables or take long locks as a
// separate job instead: replicas wait for the lock while starting.
package migrate

import (
	"context"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
)

// Option configures [Up] and [Pending].
type Option func(*options)

type options struct {
	table       string
	logger      *slog.Logger
	outOfOrder  bool
	lockTimeout time.Duration
}

// WithLockTimeout bounds how long a replica waits for another one to finish
// migrating. The default is 10 minutes.
func WithLockTimeout(d time.Duration) Option { return func(o *options) { o.lockTimeout = d } }

// WithTableName records applied versions in table instead of
// "goose_db_version". Services sharing a database need different tables.
// The table also names the advisory lock, so they do not wait for each
// other.
func WithTableName(table string) Option { return func(o *options) { o.table = table } }

// WithLogger sets the logger for applied migrations. The default is
// slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithAllowOutOfOrder applies migrations with versions below the current
// one that were never applied, such as from a branch merged late. By
// default they are an error.
func WithAllowOutOfOrder() Option { return func(o *options) { o.outOfOrder = true } }

// Applied is one migration that Up applied.
type Applied struct {
	Version  int64
	Name     string
	Duration time.Duration
}

// Up applies every pending migration in fsys, in version order, holding
// the advisory lock. It returns the migrations it applied: none when the
// database is up to date or another replica just migrated it. A failed
// migration stops Up with an error; the ones before it stay applied.
func Up(ctx context.Context, db *postgres.DB, fsys fs.FS, opts ...Option) ([]Applied, error) {
	p, o, err := provider(db, fsys, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = p.Close() }()
	results, err := p.Up(ctx)
	if pe, ok := errors.AsType[*goose.PartialError](err); ok {
		results = pe.Applied
	}
	var applied []Applied
	for _, r := range results {
		if r.Error != nil || r.Empty {
			continue
		}
		a := Applied{Version: r.Source.Version, Name: r.Source.Path, Duration: r.Duration}
		applied = append(applied, a)
		o.logger.InfoContext(ctx, "migration applied", "version", a.Version, "name", a.Name, "duration", a.Duration)
	}
	if err != nil {
		return applied, errors.Join(errors.New("migrate: up"), classify(err))
	}
	return applied, nil
}

// Pending reports whether fsys has migrations the database lacks, for
// example for a readiness check in a service that does not migrate itself.
func Pending(ctx context.Context, db *postgres.DB, fsys fs.FS, opts ...Option) (bool, error) {
	p, _, err := provider(db, fsys, opts)
	if err != nil {
		return false, err
	}
	defer func() { _ = p.Close() }()
	pending, err := p.HasPending(ctx)
	return pending, classify(err)
}

func provider(db *postgres.DB, fsys fs.FS, opts []Option) (*goose.Provider, options, error) {
	o := options{table: "goose_db_version", logger: slog.Default(), lockTimeout: 10 * time.Minute}
	for _, opt := range opts {
		opt(&o)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("common/migrate:" + o.table))
	// Waiting replicas retry the lock every second.
	attempts := uint64(1)
	if secs := int64(o.lockTimeout / time.Second); secs > 1 {
		attempts = uint64(secs)
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(int64(h.Sum64()>>1)), lock.WithLockTimeout(1, attempts))
	if err != nil {
		return nil, o, errors.Internal.Wrap(err, "migrate: locker")
	}
	p, err := goose.NewProvider(goose.DialectPostgres, stdlib.OpenDBFromPool(db.Pool()), fsys,
		goose.WithSessionLocker(locker),
		goose.WithTableName(o.table),
		goose.WithAllowOutofOrder(o.outOfOrder),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return nil, o, errors.InvalidArgument.Wrap(err, "migrate: migrations")
	}
	return p, o, nil
}

// classify gives errors a kind: PostgreSQL errors keep theirs (a failed
// statement), connection failures are Unavailable, context errors keep
// theirs, and the rest (a broken migration file, a missing version) are
// InvalidArgument.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.KindOf(err) != errors.Unknown:
		return err
	}
	if _, ok := errors.AsType[*pgconn.PgError](err); ok {
		return postgres.Classify(err)
	}
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return errors.Unavailable.Wrap(err, "migrate")
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return errors.Unavailable.Wrap(err, "migrate")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return postgres.Classify(err)
	}
	return errors.InvalidArgument.Wrap(err, "migrate")
}
