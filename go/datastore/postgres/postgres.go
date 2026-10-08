// Package postgres creates and manages PostgreSQL connection pools
// (github.com/jackc/pgx/v5/pgxpool) with tracing, pool metrics, health
// checks, graceful shutdown and optional credential rotation.
//
//	db, err := postgres.New(ctx, cfg.Postgres)
//	if err != nil {
//		return err
//	}
//	shutdown.Register(graceful.CloseDeps, "postgres", db.Close)
//	checks.AddReadiness("postgres", db.Ping)
//
//	rows, err := db.Query(ctx, "SELECT id FROM orders WHERE customer_id = $1", id)
//
// This package manages connections only; it has no repositories or query
// helpers. Exec, Query, QueryRow, Begin and SendBatch delegate to the
// current pool, and [DB.Pool] exposes it for everything else.
//
// # Credential rotation
//
// With [WithCredentials], the user name and password come from a function
// (typically vault.Client.Fetcher for dynamic database credentials) and
// the pool is replaced through secret/rotation before they expire: a new
// pool is created and pinged, swapped in atomically, and the old pool is
// closed once its acquired connections are returned (pgxpool.Pool.Close
// waits for them). Always go through DB, or call [DB.Pool] per operation,
// so new work uses the new pool; never keep a *pgxpool.Pool.
//
// # Timeouts
//
// Use context deadlines on every query (the httpserver.Timeout middleware
// sets one per request). Config.StatementTimeout additionally makes the
// server cancel statements that run too long, which also protects against
// callers without deadlines.
//
// # Telemetry
//
// Queries are traced with otelpgx as children of the span in the query's
// context (the request span, for example); queries without a recording
// span in their context are not traced, so start a span in background jobs.
// The SQL text (with placeholders) is recorded, query parameters never are. Pool statistics are exported as
// db.client.connection.* metrics for the current pool.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
)

// Config configures the pool. Environment variable names are relative; the
// service chooses the prefix, for example PG_.
type Config struct {
	Host     string        `env:"HOST" envDefault:"localhost"`
	Port     int           `env:"PORT" envDefault:"5432" validate:"min=1,max=65535"`
	Database string        `env:"DATABASE,required"`
	User     string        `env:"USER"`
	Password config.Secret `env:"PASSWORD"`
	// SSLMode is a libpq sslmode: disable, require, verify-ca or
	// verify-full. The default, verify-full, checks the server certificate
	// and host name; use disable only for local development.
	SSLMode         string `env:"SSLMODE" envDefault:"verify-full" validate:"oneof=disable allow prefer require verify-ca verify-full"`
	SSLRootCert     string `env:"SSLROOTCERT"`
	ApplicationName string `env:"APPLICATION_NAME"`

	MaxConns          int32         `env:"MAX_CONNS" envDefault:"10" validate:"min=1"`
	MinConns          int32         `env:"MIN_CONNS" envDefault:"0" validate:"min=0"`
	MaxConnLifetime   time.Duration `env:"MAX_CONN_LIFETIME" envDefault:"30m"`
	MaxConnIdleTime   time.Duration `env:"MAX_CONN_IDLE_TIME" envDefault:"5m"`
	HealthCheckPeriod time.Duration `env:"HEALTH_CHECK_PERIOD" envDefault:"1m"`
	ConnectTimeout    time.Duration `env:"CONNECT_TIMEOUT" envDefault:"5s"`
	// StatementTimeout makes the server cancel statements running longer;
	// 0 leaves the server default.
	StatementTimeout time.Duration `env:"STATEMENT_TIMEOUT"`
}

func (c Config) withDefaults() Config {
	if c.Host == "" {
		c.Host = "localhost"
	}
	if c.Port == 0 {
		c.Port = 5432
	}
	if c.SSLMode == "" {
		c.SSLMode = "verify-full"
	}
	if c.MaxConns == 0 {
		c.MaxConns = 10
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.MaxConnLifetime, 30*time.Minute)
	def(&c.MaxConnIdleTime, 5*time.Minute)
	def(&c.HealthCheckPeriod, time.Minute)
	def(&c.ConnectTimeout, 5*time.Second)
	return c
}

// poolConfig builds a pgxpool configuration. The password is set on the
// parsed configuration, never placed in a connection string, so it cannot
// appear in parse errors.
func (c Config) poolConfig(user, password string) (*pgxpool.Config, error) {
	q := url.Values{}
	q.Set("sslmode", c.SSLMode)
	if c.SSLRootCert != "" {
		q.Set("sslrootcert", c.SSLRootCert)
	}
	if c.ApplicationName != "" {
		q.Set("application_name", c.ApplicationName)
	}
	q.Set("connect_timeout", strconv.Itoa(max(int(c.ConnectTimeout.Seconds()), 1)))
	if c.StatementTimeout > 0 {
		q.Set("statement_timeout", strconv.FormatInt(c.StatementTimeout.Milliseconds(), 10))
	}
	u := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:     "/" + c.Database,
		RawQuery: q.Encode(),
	}
	pc, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "postgres: config")
	}
	pc.ConnConfig.User = user
	pc.ConnConfig.Password = password
	pc.MaxConns = c.MaxConns
	pc.MinConns = c.MinConns
	pc.MaxConnLifetime = c.MaxConnLifetime
	pc.MaxConnLifetimeJitter = c.MaxConnLifetime / 10
	pc.MaxConnIdleTime = c.MaxConnIdleTime
	pc.HealthCheckPeriod = c.HealthCheckPeriod
	return pc, nil
}

// Option configures [New].
type Option func(*options)

type options struct {
	credentials func(ctx context.Context) (secret.Secret, error)
	revoke      func(ctx context.Context, s secret.Secret) error
	rotationOpt []rotation.Option
	logger      *slog.Logger
	meterProv   metric.MeterProvider
	tracerProv  trace.TracerProvider
	configure   func(*pgxpool.Config)
}

// WithCredentials takes user name and password from the "username" and
// "password" fields of the secrets fetch returns, rotating the pool before
// they expire. Config.User and Config.Password are then ignored. revoke, if
// not nil, is called for a credential once its pool is closed (for Vault:
// vault.Client.Revoke).
func WithCredentials(fetch func(ctx context.Context) (secret.Secret, error), revoke func(ctx context.Context, s secret.Secret) error, opts ...rotation.Option) Option {
	return func(o *options) { o.credentials, o.revoke, o.rotationOpt = fetch, revoke, opts }
}

// WithConfigure lets callers adjust the pgxpool configuration before each
// pool is created, for example to register types in AfterConnect.
func WithConfigure(fn func(*pgxpool.Config)) Option { return func(o *options) { o.configure = fn } }

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.meterProv = mp } }

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// DB is a PostgreSQL pool, possibly replaced over time by credential
// rotation. It is safe for concurrent use.
type DB struct {
	cfg     Config
	o       options
	static  *pgxpool.Pool
	rotator *rotation.Rotator[*pgxpool.Pool]
	reg     metric.Registration
}

// New creates the pool and checks connectivity with a ping, so a service
// does not start with an unusable database.
func New(ctx context.Context, cfg Config, opts ...Option) (*DB, error) {
	cfg = cfg.withDefaults()
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	o := options{
		logger:     slog.Default(),
		meterProv:  otel.GetMeterProvider(),
		tracerProv: otel.GetTracerProvider(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	db := &DB{cfg: cfg, o: o}

	if o.credentials == nil {
		pool, err := db.connect(ctx, cfg.User, cfg.Password.Reveal())
		if err != nil {
			return nil, err
		}
		db.static = pool
	} else {
		r, err := rotation.New(ctx, "postgres "+cfg.Database, rotation.Spec[*pgxpool.Pool]{
			Fetch: o.credentials,
			Build: func(ctx context.Context, s secret.Secret) (*pgxpool.Pool, error) {
				if err := s.Require("username", "password"); err != nil {
					return nil, err
				}
				return db.connect(ctx, s.Field("username"), s.Field("password"))
			},
			Close: func(ctx context.Context, p *pgxpool.Pool, s secret.Secret) error {
				err := closePool(ctx, p)
				if o.revoke != nil {
					err = errors.Join(err, o.revoke(ctx, s))
				}
				return err
			},
		}, append([]rotation.Option{rotation.WithLogger(o.logger), rotation.WithMeterProvider(o.meterProv)}, o.rotationOpt...)...)
		if err != nil {
			return nil, err
		}
		db.rotator = r
	}
	db.initMetrics()
	return db, nil
}

// connect creates a pool and pings it. The ping doubles as validation for
// rotation: a pool that cannot connect is never swapped in.
func (db *DB) connect(ctx context.Context, user, password string) (*pgxpool.Pool, error) {
	pc, err := db.cfg.poolConfig(user, password)
	if err != nil {
		return nil, err
	}
	pc.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithTracerProvider(db.o.tracerProv),
		otelpgx.WithMeterProvider(db.o.meterProv),
	)
	if db.o.configure != nil {
		db.o.configure(pc)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, classify(err, "postgres: create pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, classify(err, "postgres: connect")
	}
	return pool, nil
}

func (db *DB) initMetrics() {
	meter := db.o.meterProv.Meter("github.com/Arif9878/common/go/datastore/postgres")
	poolAttr := attribute.String("db.client.connection.pool.name", db.cfg.Database)
	attrs := metric.WithAttributeSet(attribute.NewSet(poolAttr))
	idleAttrs := metric.WithAttributeSet(attribute.NewSet(poolAttr, attribute.String("db.client.connection.state", "idle")))
	usedAttrs := metric.WithAttributeSet(attribute.NewSet(poolAttr, attribute.String("db.client.connection.state", "used")))

	count, _ := meter.Int64ObservableUpDownCounter("db.client.connection.count", metric.WithUnit("{connection}"),
		metric.WithDescription("Connections by state (idle or used)."))
	maxConns, _ := meter.Int64ObservableUpDownCounter("db.client.connection.max", metric.WithUnit("{connection}"),
		metric.WithDescription("Maximum open connections."))
	pending, _ := meter.Int64ObservableUpDownCounter("db.client.connection.pending_requests", metric.WithUnit("{request}"),
		metric.WithDescription("Acquires waiting for a connection (pgxpool: constructing connections)."))
	waits, _ := meter.Int64ObservableCounter("db.client.connection.wait_count", metric.WithUnit("{acquire}"),
		metric.WithDescription("Acquires that had to wait for a connection; a rising rate means the pool is too small."))
	waitTime, _ := meter.Float64ObservableCounter("db.client.connection.wait_time", metric.WithUnit("s"),
		metric.WithDescription("Total time spent waiting for connections."))

	db.reg, _ = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		st := db.Pool().Stat()
		obs.ObserveInt64(count, int64(st.IdleConns()), idleAttrs)
		obs.ObserveInt64(count, int64(st.AcquiredConns()), usedAttrs)
		obs.ObserveInt64(maxConns, int64(st.MaxConns()), attrs)
		obs.ObserveInt64(pending, int64(st.ConstructingConns()), attrs)
		obs.ObserveInt64(waits, st.EmptyAcquireCount(), attrs)
		obs.ObserveFloat64(waitTime, st.EmptyAcquireWaitTime().Seconds(), attrs)
		return nil
	}, count, maxConns, pending, waits, waitTime)
}

// Pool returns the current pool. Call it per operation; with rotation it
// changes over time, and an operation started on a pool that is being
// replaced can fail with puddle.ErrClosedPool or a *pgconn.ConnectError
// before sending anything (retry it). The DB methods retry automatically,
// so prefer them.
func (db *DB) Pool() *pgxpool.Pool {
	if db.rotator != nil {
		return db.rotator.Current()
	}
	return db.static
}

// withPool calls f with the current pool. With rotation, a caller can read
// the pool just before it is replaced and closed, and its acquisition then
// fails without any statement having been sent; see acquisitionFailed. Such
// failures are retried on the new pool.
func withPool[T any](db *DB, f func(p *pgxpool.Pool) (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		p := db.Pool()
		v, err := f(p)
		if err != nil && db.rotator != nil && attempt < 3 && acquisitionFailed(err) && db.Pool() != p {
			continue
		}
		return v, err
	}
}

// acquisitionFailed reports whether err means no connection was obtained,
// so nothing was sent to the server and retrying cannot repeat a write:
// either the pool was already closed (puddle.ErrClosedPool), or a new
// connection was being established when the pool was closed, which cancels
// it (*pgconn.ConnectError).
func acquisitionFailed(err error) bool {
	if errors.Is(err, puddle.ErrClosedPool) {
		return true
	}
	_, ok := errors.AsType[*pgconn.ConnectError](err)
	return ok
}

// Acquire returns a connection from the current pool. Release it when done.
func (db *DB) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	return withPool(db, func(p *pgxpool.Pool) (*pgxpool.Conn, error) { return p.Acquire(ctx) })
}

// Exec runs sql on the current pool.
func (db *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return withPool(db, func(p *pgxpool.Pool) (pgconn.CommandTag, error) { return p.Exec(ctx, sql, args...) })
}

// Query runs sql on the current pool.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return withPool(db, func(p *pgxpool.Pool) (pgx.Rows, error) { return p.Query(ctx, sql, args...) })
}

// QueryRow runs sql on the current pool. Errors are returned by Scan, as
// with pgx.
func (db *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := db.Query(ctx, sql, args...)
	return &row{rows: rows, err: err}
}

// row implements pgx.Row over Query, so that acquisition errors are
// detected (and retried) up front rather than at Scan.
type row struct {
	rows pgx.Rows
	err  error
}

func (r *row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	r.rows.Close()
	return r.rows.Err()
}

// Begin starts a transaction on the current pool. The transaction keeps
// its connection, and so its pool, until it ends.
func (db *DB) Begin(ctx context.Context) (pgx.Tx, error) {
	return withPool(db, func(p *pgxpool.Pool) (pgx.Tx, error) { return p.Begin(ctx) })
}

// SendBatch sends b on a connection from the current pool, which is
// released when the results are closed.
func (db *DB) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return errBatch{err}
	}
	return &connBatch{BatchResults: conn.SendBatch(ctx, b), conn: conn}
}

type connBatch struct {
	pgx.BatchResults
	conn *pgxpool.Conn
}

func (b *connBatch) Close() error {
	err := b.BatchResults.Close()
	b.conn.Release()
	return err
}

type errBatch struct{ err error }

func (e errBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, e.err }
func (e errBatch) Query() (pgx.Rows, error)         { return nil, e.err }
func (e errBatch) QueryRow() pgx.Row                { return &row{err: e.err} }
func (e errBatch) Close() error                     { return e.err }

// Ping checks connectivity; it matches health.Check.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.Pool().Ping(ctx); err != nil {
		return classify(err, "postgres: ping")
	}
	return nil
}

// Rotate replaces the pool with one using freshly fetched credentials. It
// returns an error of kind InvalidArgument without credential rotation.
func (db *DB) Rotate(ctx context.Context) error {
	if db.rotator == nil {
		return errors.InvalidArgument.New("postgres: credential rotation not configured")
	}
	return db.rotator.Rotate(ctx)
}

// Close closes the pool, waiting within ctx for acquired connections to be
// returned. With rotation, it also closes replaced pools still draining.
// Its signature matches graceful.Hook.
func (db *DB) Close(ctx context.Context) error {
	if db.reg != nil {
		_ = db.reg.Unregister()
	}
	if db.rotator != nil {
		return db.rotator.Close(ctx)
	}
	return closePool(ctx, db.static)
}

// closePool closes p within ctx. pgxpool.Close waits for every acquired
// connection, so a leaked connection would block forever; this returns at
// ctx's deadline and lets the close finish in the background.
func closePool(ctx context.Context, p *pgxpool.Pool) error {
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.Timeout.Errorf("postgres: close: %d connections still acquired: %w", p.Stat().AcquiredConns(), ctx.Err())
	}
}

// Classify returns err with an errors.Kind derived from PostgreSQL error
// codes: pgx.ErrNoRows and unknown database NotFound, unique violations
// Conflict, foreign key, check and not-null violations InvalidArgument,
// serialization failures and deadlocks Unavailable (so the retry package
// retries the transaction), authentication failures Unauthorized, too many
// connections Unavailable, statement timeout Timeout, network failures
// Unavailable, other server errors Internal. Context errors keep
// their kind. Use it at repository boundaries; nil stays nil.
func Classify(err error) error {
	if err == nil || errors.KindOf(err) != errors.Unknown {
		return err
	}
	return classify(err, "postgres")
}

// classify gives pgx errors a kind. Messages come from pgx, which does not
// include passwords.
func classify(err error, msg string) error {
	if err == nil {
		return nil
	}
	if errors.KindOf(err) != errors.Unknown {
		return fmt.Errorf("%s: %w", msg, err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.NotFound.Wrap(err, msg)
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "23505": // unique_violation
			return errors.Conflict.Wrap(err, msg)
		case "23503", "23514", "23502": // foreign key, check, not null
			return errors.InvalidArgument.Wrap(err, msg)
		case "40001", "40P01": // serialization failure, deadlock: safe to retry
			return errors.Unavailable.Wrap(err, msg)
		case "28P01", "28000": // invalid password / authorization
			return errors.Unauthorized.Wrap(err, msg)
		case "3D000": // database does not exist
			return errors.NotFound.Wrap(err, msg)
		case "53300", "57P03": // too many connections, cannot connect now
			return errors.Unavailable.Wrap(err, msg)
		case "57014": // statement canceled (statement_timeout)
			return errors.Timeout.Wrap(err, msg)
		}
		return errors.Internal.Wrap(err, msg)
	}
	return errors.Unavailable.Wrap(err, msg)
}
