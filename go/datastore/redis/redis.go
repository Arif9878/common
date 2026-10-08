// Package redis creates Redis clients (github.com/redis/go-redis/v9) with
// tracing, metrics, health checks, graceful shutdown and optional credential
// rotation.
//
//	rdb, err := redis.New(ctx, cfg.Redis)
//	if err != nil {
//		return err
//	}
//	shutdown.Register(graceful.CloseDeps, "redis", rdb.Stop)
//	checks.AddReadiness("redis", rdb.HealthCheck)
//
//	err = rdb.Set(ctx, "k", "v", time.Minute).Err()
//
// The lifecycle method is [Client.Stop], not Shutdown or Close: go-redis
// already uses those names (Shutdown sends the server's SHUTDOWN command).
//
// The returned [Client] embeds goredis.UniversalClient: every go-redis
// command is available, and one Addrs entry gives a single-node client,
// several give a cluster client, and MasterName gives a Sentinel failover
// client. The package adds no business-level helpers.
//
// # Retries
//
// go-redis retries failed commands three times by default, including
// non-idempotent ones such as INCR, which can then apply twice. Here
// retries are off unless Config.MaxRetries is set.
//
// # Credential rotation
//
// With [WithCredentials], every new connection authenticates with the
// current credential, refreshed through secret/rotation before it expires
// (validated by connecting with it). Existing connections keep the
// credential they authenticated with until ConnMaxLifetime recycles them,
// so a replaced credential is revoked only after ConnMaxLifetime has
// passed (or at Stop). Keep ConnMaxLifetime well below the credential
// TTL.
//
// # Telemetry
//
// Commands are traced with redisotel, without their arguments: values and
// keys can hold personal data. Connection pool metrics come from redisotel.
//
// Distributed locks and distributed rate limiting are not provided here
// because their correctness guarantees need careful documentation; see the
// lock package.
package redis

import (
	"context"
	"crypto/tls"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
)

// Config configures the client. Environment variable names are relative;
// the service chooses the prefix, for example REDIS_.
type Config struct {
	// Addrs lists host:port addresses: one for a single node, several for
	// a cluster or for Sentinel nodes (with MasterName).
	Addrs      []string      `env:"ADDRS" envSeparator:"," envDefault:"localhost:6379"`
	MasterName string        `env:"MASTER_NAME"`
	Username   string        `env:"USERNAME"`
	Password   config.Secret `env:"PASSWORD"`
	DB         int           `env:"DB"`
	TLS        bool          `env:"TLS"`
	// TLSServerName overrides the name checked in the server certificate.
	TLSServerName string `env:"TLS_SERVER_NAME"`

	DialTimeout     time.Duration `env:"DIAL_TIMEOUT" envDefault:"5s"`
	ReadTimeout     time.Duration `env:"READ_TIMEOUT" envDefault:"3s"`
	WriteTimeout    time.Duration `env:"WRITE_TIMEOUT" envDefault:"3s"`
	PoolTimeout     time.Duration `env:"POOL_TIMEOUT" envDefault:"4s"`
	PoolSize        int           `env:"POOL_SIZE"` // 0: go-redis default, 10 per CPU
	MinIdleConns    int           `env:"MIN_IDLE_CONNS"`
	ConnMaxLifetime time.Duration `env:"CONN_MAX_LIFETIME" envDefault:"30m"`
	ConnMaxIdleTime time.Duration `env:"CONN_MAX_IDLE_TIME" envDefault:"5m"`
	// MaxRetries retries commands after network errors. 0 disables
	// retries; see the package documentation before enabling them.
	MaxRetries int `env:"MAX_RETRIES"`
}

func (c Config) withDefaults() Config {
	if len(c.Addrs) == 0 {
		c.Addrs = []string{"localhost:6379"}
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.DialTimeout, 5*time.Second)
	def(&c.ReadTimeout, 3*time.Second)
	def(&c.WriteTimeout, 3*time.Second)
	def(&c.PoolTimeout, 4*time.Second)
	def(&c.ConnMaxLifetime, 30*time.Minute)
	def(&c.ConnMaxIdleTime, 5*time.Minute)
	return c
}

func (c Config) options() *goredis.UniversalOptions {
	retries := c.MaxRetries
	if retries == 0 {
		retries = -1 // go-redis: -1 disables, 0 means "default (3)"
	}
	o := &goredis.UniversalOptions{
		Addrs:           c.Addrs,
		MasterName:      c.MasterName,
		Username:        c.Username,
		Password:        c.Password.Reveal(),
		DB:              c.DB,
		DialTimeout:     c.DialTimeout,
		ReadTimeout:     c.ReadTimeout,
		WriteTimeout:    c.WriteTimeout,
		PoolTimeout:     c.PoolTimeout,
		PoolSize:        c.PoolSize,
		MinIdleConns:    c.MinIdleConns,
		ConnMaxLifetime: c.ConnMaxLifetime,
		ConnMaxIdleTime: c.ConnMaxIdleTime,
		MaxRetries:      retries,
	}
	if c.TLS {
		o.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.TLSServerName}
	}
	return o
}

// Option configures [New].
type Option func(*options)

type options struct {
	credentials func(ctx context.Context) (secret.Secret, error)
	revoke      func(ctx context.Context, s secret.Secret) error
	rotationOpt []rotation.Option
	configure   func(*goredis.UniversalOptions)
	logger      *slog.Logger
	meterProv   metric.MeterProvider
	tracerProv  trace.TracerProvider
}

// WithCredentials takes the user name and password from the "username" and
// "password" fields of the secrets fetch returns, rotating them before they
// expire. revoke, if not nil, is called for a replaced credential once no
// connection can still be using it.
func WithCredentials(fetch func(ctx context.Context) (secret.Secret, error), revoke func(ctx context.Context, s secret.Secret) error, opts ...rotation.Option) Option {
	return func(o *options) { o.credentials, o.revoke, o.rotationOpt = fetch, revoke, opts }
}

// WithConfigure adjusts the go-redis options before the client is created.
func WithConfigure(fn func(*goredis.UniversalOptions)) Option {
	return func(o *options) { o.configure = fn }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.meterProv = mp } }

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// Client is a go-redis client with lifecycle management.
type Client struct {
	goredis.UniversalClient

	cfg     Config
	o       options
	rotator *rotation.Rotator[secret.Secret]
	metrics chan struct{} // closed on Shutdown to stop redisotel metrics

	mu      sync.Mutex
	pending map[*time.Timer]secret.Secret // revocations waiting for ConnMaxLifetime

	once        sync.Once
	shutdownErr error
}

// New creates a client and pings the server, so a service does not start
// with an unusable Redis.
func New(ctx context.Context, cfg Config, opts ...Option) (*Client, error) {
	cfg = cfg.withDefaults()
	o := options{
		logger:     slog.Default(),
		meterProv:  otel.GetMeterProvider(),
		tracerProv: otel.GetTracerProvider(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	c := &Client{cfg: cfg, o: o, metrics: make(chan struct{}), pending: map[*time.Timer]secret.Secret{}}

	ro := cfg.options()
	if o.credentials != nil {
		r, err := rotation.New(ctx, "redis", rotation.Spec[secret.Secret]{
			Fetch: o.credentials,
			Build: func(_ context.Context, s secret.Secret) (secret.Secret, error) {
				return s, s.Require("username", "password")
			},
			Validate: func(ctx context.Context, s secret.Secret) error { return c.tryCredentials(ctx, s) },
			Close: func(_ context.Context, _ secret.Secret, s secret.Secret) error {
				c.scheduleRevoke(s)
				return nil
			},
		}, append([]rotation.Option{rotation.WithLogger(o.logger), rotation.WithMeterProvider(o.meterProv)}, o.rotationOpt...)...)
		if err != nil {
			return nil, err
		}
		c.rotator = r
		ro.Username, ro.Password = "", ""
		ro.CredentialsProviderContext = func(context.Context) (string, string, error) {
			s := r.Current()
			return s.Field("username"), s.Field("password"), nil
		}
	}
	if o.configure != nil {
		o.configure(ro)
	}
	c.UniversalClient = goredis.NewUniversalClient(ro)

	if err := redisotel.InstrumentTracing(c.UniversalClient,
		redisotel.WithTracerProvider(o.tracerProv), redisotel.WithDBStatement(false)); err != nil {
		_ = c.closeAll(ctx)
		return nil, errors.Internal.Wrap(err, "redis: tracing")
	}
	if err := redisotel.InstrumentMetrics(c.UniversalClient,
		redisotel.WithMeterProvider(o.meterProv), redisotel.WithCloseChan(c.metrics)); err != nil {
		_ = c.closeAll(ctx)
		return nil, errors.Internal.Wrap(err, "redis: metrics")
	}

	if err := c.HealthCheck(ctx); err != nil {
		_ = c.closeAll(ctx)
		return nil, err
	}
	return c, nil
}

// tryCredentials validates a credential with a one-off connection, built
// with the same options (including WithConfigure) as the real client.
func (c *Client) tryCredentials(ctx context.Context, s secret.Secret) error {
	ro := c.cfg.options()
	if c.o.configure != nil {
		c.o.configure(ro)
	}
	ro.CredentialsProviderContext = nil
	ro.Username, ro.Password = s.Field("username"), s.Field("password")
	ro.PoolSize, ro.MinIdleConns = 1, 0
	probe := goredis.NewUniversalClient(ro)
	defer func() { _ = probe.Close() }()
	return Classify(probe.Ping(ctx).Err())
}

func (c *Client) scheduleRevoke(s secret.Secret) {
	if c.o.revoke == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var t *time.Timer
	t = time.AfterFunc(c.cfg.ConnMaxLifetime, func() {
		c.mu.Lock()
		_, ok := c.pending[t]
		delete(c.pending, t)
		c.mu.Unlock()
		if ok {
			c.revoke(s)
		}
	})
	c.pending[t] = s
}

func (c *Client) revoke(s secret.Secret) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.o.revoke(ctx, s); err != nil {
		c.o.logger.Warn("revoking replaced redis credential failed", logging.Err(err))
	}
}

// HealthCheck pings the server; it matches health.Check.
func (c *Client) HealthCheck(ctx context.Context) error {
	if err := c.Ping(ctx).Err(); err != nil {
		return errors.Join(errors.New("redis: ping"), Classify(err))
	}
	return nil
}

// Rotate replaces the credential now. It returns an error of kind
// InvalidArgument without credential rotation.
func (c *Client) Rotate(ctx context.Context) error {
	if c.rotator == nil {
		return errors.InvalidArgument.New("redis: credential rotation not configured")
	}
	return c.rotator.Rotate(ctx)
}

// Stop closes the client, stops rotation and revokes replaced
// credentials still waiting for ConnMaxLifetime. Commands in flight fail,
// so call it after work is drained (graceful.CloseDeps). Its signature
// matches graceful.Hook; repeated calls return the first result.
func (c *Client) Stop(ctx context.Context) error {
	c.once.Do(func() { c.shutdownErr = c.closeAll(ctx) })
	return c.shutdownErr
}

func (c *Client) closeAll(ctx context.Context) error {
	var errs []error
	if c.rotator != nil {
		// The current credential is revoked too: no connection will use it.
		errs = append(errs, c.rotator.Close(ctx))
	}
	c.mu.Lock()
	pending := c.pending
	c.pending = map[*time.Timer]secret.Secret{}
	c.mu.Unlock()
	for t, s := range pending {
		if t.Stop() {
			c.revoke(s)
		}
	}
	select {
	case <-c.metrics:
	default:
		close(c.metrics)
	}
	if c.UniversalClient != nil {
		errs = append(errs, c.UniversalClient.Close()) //nolint:staticcheck // explicit: the go-redis client, not Stop
	}
	return errors.Join(errs...)
}

// Classify returns err with an errors.Kind: redis.Nil (missing key)
// NotFound, authentication errors Unauthorized or Forbidden, LOADING, BUSY,
// TRYAGAIN, CLUSTERDOWN and MASTERDOWN Unavailable, network failures and
// pool timeouts Unavailable or Timeout, other server errors Internal.
// Context errors keep their kind. nil stays nil.
func Classify(err error) error {
	if err == nil || errors.KindOf(err) != errors.Unknown {
		return err
	}
	if errors.Is(err, goredis.Nil) {
		return errors.NotFound.Wrap(err, "redis")
	}
	if errors.Is(err, goredis.ErrPoolTimeout) {
		return errors.Unavailable.Wrap(err, "redis: pool exhausted")
	}
	if re, ok := errors.AsType[goredis.Error](err); ok {
		msg := re.Error()
		switch prefix, _, _ := strings.Cut(msg, " "); prefix {
		case "NOAUTH", "WRONGPASS":
			return errors.Unauthorized.Wrap(err, "redis")
		case "NOPERM":
			return errors.Forbidden.Wrap(err, "redis")
		case "LOADING", "BUSY", "TRYAGAIN", "CLUSTERDOWN", "MASTERDOWN":
			return errors.Unavailable.Wrap(err, "redis")
		}
		if strings.Contains(msg, "invalid username-password") || strings.Contains(msg, "invalid password") {
			return errors.Unauthorized.Wrap(err, "redis")
		}
		return errors.Internal.Wrap(err, "redis")
	}
	return errors.Unavailable.Wrap(err, "redis")
}
