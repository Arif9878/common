// Package vault is a HashiCorp Vault client that implements
// secret.Provider and manages its own authentication.
//
//	auth, _ := kubernetes.NewKubernetesAuth("orders") // github.com/hashicorp/vault/api/auth/kubernetes
//	v, err := vault.New(ctx, cfg.Vault, vault.WithAuth(auth))
//	if err != nil {
//		return err
//	}
//	shutdown.Register(graceful.CloseDeps, "vault", v.Close)
//
//	s, err := v.Get(ctx, "secret/data/orders/stripe") // KV v2
//	key := s.Field("api_key")
//
// # Authentication
//
// [New] logs in before returning, so a service never starts without access.
// Any api.AuthMethod works (Kubernetes, AppRole, AWS, ... from
// github.com/hashicorp/vault/api/auth/*); without one, Config.Token is used.
// The client renews its token in the background and, when the token cannot
// be renewed further (it reached its max TTL or renewal failed), logs in
// again with exponential backoff (1s to 1m, jittered) until it succeeds or
// the client is closed. Watch vault.token.ttl.
//
// # Reading secrets
//
// [Client.Get] reads any logical path. KV v2 responses ("secret/data/x")
// are recognized and unwrapped: fields come from data.data and Version from
// metadata.version; a deleted or destroyed version is NotFound. Other
// responses (dynamic secrets such as "database/creds/orders") use the
// response data as fields and carry the lease: LeaseID and ExpiresAt.
// Non-string values are JSON-encoded.
//
// Dynamic secrets plug into secret/rotation: use [Client.Fetcher] as
// Spec.Fetch and [Client.Revoke] in Spec.Close, so rotation obtains new
// credentials before the lease expires and revokes the old lease once
// drained. [Client.RenewLease] extends a lease instead, where that suits
// better.
//
// # Errors
//
// Errors are classified with the errors package: 400 InvalidArgument,
// 401/403 Forbidden (Vault answers 403 for bad tokens and missing policy),
// 404 or no data NotFound, 429 RateLimited, 503 (sealed, standby)
// Unavailable, network failures Unavailable, other 5xx Internal. Messages
// contain the operation, path, status and Vault's error strings, never
// response bodies, request data or secret values.
//
// The underlying *api.Client is available from [Client.API] for anything
// this package does not cover.
package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/tracing"
	"github.com/Arif9878/common/go/secret"
)

// Config configures the client. Empty fields fall back to Vault's standard
// environment variables (VAULT_ADDR, VAULT_NAMESPACE, VAULT_CACERT, ...).
// Variable names are relative; the service chooses the prefix.
type Config struct {
	Address   string `env:"ADDR"`
	Namespace string `env:"NAMESPACE"`
	// Token authenticates when no auth method is given.
	Token config.Secret `env:"TOKEN"`
	// CACert is a PEM file with the CA that signed Vault's certificate.
	CACert        string `env:"CACERT"`
	TLSServerName string `env:"TLS_SERVER_NAME"`
	// Timeout bounds each HTTP request to Vault.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10s"`
	// MaxRetries is how often the Vault library retries 5xx and 412
	// responses, with its own backoff. The default is 2.
	MaxRetries int `env:"MAX_RETRIES" envDefault:"2"`
}

// Option configures [New].
type Option func(*options)

type options struct {
	auth          api.AuthMethod
	logger        *slog.Logger
	meterProvider metric.MeterProvider
	tracer        trace.Tracer
	minBackoff    time.Duration
	maxBackoff    time.Duration
}

// WithAuth logs in with m, for example Kubernetes or AppRole auth.
func WithAuth(m api.AuthMethod) Option { return func(o *options) { o.auth = m } }

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProvider = mp }
}

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracer = tp.Tracer("github.com/Arif9878/common/go/secret/vault") }
}

// WithReloginBackoff sets the delays between failed login attempts after
// the token expired. The default is 1s doubling to 1m.
func WithReloginBackoff(minDelay, maxDelay time.Duration) Option {
	return func(o *options) { o.minBackoff, o.maxBackoff = minDelay, max(maxDelay, minDelay) }
}

// Client is a Vault client. It is safe for concurrent use.
type Client struct {
	api *api.Client
	o   options

	mu       sync.Mutex
	tokenExp time.Time // zero if unknown or non-expiring

	stop     context.CancelFunc
	done     chan struct{}
	closeErr error
	once     sync.Once

	requests metric.Int64Counter
	duration metric.Float64Histogram
	reg      metric.Registration
}

// New creates a client and logs in. It fails if the configuration is
// invalid or login fails.
func New(ctx context.Context, cfg Config, opts ...Option) (*Client, error) {
	o := options{
		logger:        slog.Default(),
		meterProvider: otel.GetMeterProvider(),
		tracer:        otel.GetTracerProvider().Tracer("github.com/Arif9878/common/go/secret/vault"),
		minBackoff:    time.Second,
		maxBackoff:    time.Minute,
	}
	for _, opt := range opts {
		opt(&o)
	}

	apiCfg := api.DefaultConfig() // reads VAULT_* environment variables
	if apiCfg.Error != nil {
		return nil, errors.InvalidArgument.Wrap(apiCfg.Error, "vault: config")
	}
	if cfg.Address != "" {
		apiCfg.Address = cfg.Address
	}
	if cfg.Timeout > 0 {
		apiCfg.Timeout = cfg.Timeout
	}
	apiCfg.MaxRetries = cfg.MaxRetries
	if cfg.CACert != "" || cfg.TLSServerName != "" {
		if err := apiCfg.ConfigureTLS(&api.TLSConfig{CACert: cfg.CACert, TLSServerName: cfg.TLSServerName}); err != nil {
			return nil, errors.InvalidArgument.Wrap(err, "vault: TLS config")
		}
	}
	client, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "vault: client")
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}
	if tok := cfg.Token.Reveal(); tok != "" {
		client.SetToken(tok)
	}

	c := &Client{api: client, o: o, done: make(chan struct{})}
	c.initMetrics()

	authSecret, err := c.login(ctx)
	if err != nil {
		c.unregister()
		return nil, err
	}

	loopCtx, stop := context.WithCancel(context.Background())
	c.stop = stop
	go c.keepAlive(loopCtx, authSecret)
	return c, nil
}

func (c *Client) initMetrics() {
	meter := c.o.meterProvider.Meter("github.com/Arif9878/common/go/secret/vault")
	c.requests, _ = meter.Int64Counter("vault.requests",
		metric.WithDescription("Vault operations by operation and outcome (ok or the error kind)."))
	c.duration, _ = meter.Float64Histogram("vault.request.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of Vault operations."))
	ttl, _ := meter.Float64ObservableGauge("vault.token.ttl", metric.WithUnit("s"),
		metric.WithDescription("Seconds until the client token expires. Absent for non-expiring tokens."))
	c.reg, _ = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		c.mu.Lock()
		exp := c.tokenExp
		c.mu.Unlock()
		if !exp.IsZero() {
			obs.ObserveFloat64(ttl, time.Until(exp).Seconds())
		}
		return nil
	}, ttl)
}

func (c *Client) unregister() {
	if c.reg != nil {
		_ = c.reg.Unregister()
	}
}

// API returns the underlying Vault client.
func (c *Client) API() *api.Client { return c.api }

// observe runs op with a span, metrics and error classification.
func (c *Client) observe(ctx context.Context, operation, path string, op func(ctx context.Context) error) (err error) {
	ctx, span := c.o.tracer.Start(ctx, "vault."+operation, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("vault.path", path)))
	defer tracing.End(span, &err)
	start := time.Now()
	err = op(ctx)
	if err != nil {
		err = classify(ctx, operation, path, err)
	}
	outcome := "ok"
	if err != nil {
		outcome = errors.KindOf(err).String()
	}
	attrs := metric.WithAttributes(attribute.String("operation", operation), attribute.String("outcome", outcome))
	c.requests.Add(ctx, 1, attrs)
	c.duration.Record(ctx, time.Since(start).Seconds(), attrs)
	return err
}

// classify converts a Vault error into a classified error without response
// bodies.
func classify(ctx context.Context, operation, path string, err error) error {
	if errors.KindOf(err) != errors.Unknown {
		return err // already classified, or a context error
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The Vault library does not always wrap context errors.
		return errors.KindOf(ctxErr).Errorf("vault %s %s: %v", operation, path, ctxErr)
	}
	re, ok := errors.AsType[*api.ResponseError](err)
	if !ok {
		return errors.Unavailable.Wrap(err, "vault "+operation+" "+path)
	}

	msg := fmt.Sprintf("vault %s %s: status %d", operation, path, re.StatusCode)
	if !re.RawError && len(re.Errors) > 0 {
		msg += ": " + strings.Join(re.Errors, "; ")
	}
	kind := errors.Internal
	switch re.StatusCode {
	case http.StatusBadRequest:
		kind = errors.InvalidArgument
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = errors.Forbidden
	case http.StatusNotFound:
		kind = errors.NotFound
	case http.StatusTooManyRequests:
		kind = errors.RateLimited
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		kind = errors.Unavailable
	}
	return kind.New(msg)
}

// Get reads the secret at path. It implements secret.Provider.
func (c *Client) Get(ctx context.Context, path string) (secret.Secret, error) {
	var out secret.Secret
	err := c.observe(ctx, "read", path, func(ctx context.Context) error {
		s, err := c.api.Logical().ReadWithContext(ctx, path)
		if err != nil {
			return err
		}
		out, err = convert(path, s)
		return err
	})
	return out, err
}

// Fetcher returns a function reading path, for rotation.Spec.Fetch.
func (c *Client) Fetcher(path string) func(ctx context.Context) (secret.Secret, error) {
	return func(ctx context.Context) (secret.Secret, error) { return c.Get(ctx, path) }
}

// RenewLease extends the lease of s by increment (the server may grant
// less) and returns s with the new expiry. It returns an error of kind
// InvalidArgument if s has no lease.
func (c *Client) RenewLease(ctx context.Context, s secret.Secret, increment time.Duration) (secret.Secret, error) {
	if s.LeaseID == "" {
		return s, errors.InvalidArgument.New("vault: secret has no lease")
	}
	err := c.observe(ctx, "renew", leasePrefix(s.LeaseID), func(ctx context.Context) error {
		r, err := c.api.Sys().RenewWithContext(ctx, s.LeaseID, int(increment.Seconds()))
		if err != nil {
			return err
		}
		if r == nil {
			return errors.Internal.New("vault: empty renewal response")
		}
		s.ExpiresAt = time.Now().Add(time.Duration(r.LeaseDuration) * time.Second)
		return nil
	})
	return s, err
}

// Revoke revokes the lease of s, if it has one. Its signature fits
// rotation.Spec.Close together with closing the resource.
func (c *Client) Revoke(ctx context.Context, s secret.Secret) error {
	if s.LeaseID == "" {
		return nil
	}
	return c.observe(ctx, "revoke", leasePrefix(s.LeaseID), func(ctx context.Context) error {
		return c.api.Sys().RevokeWithContext(ctx, s.LeaseID)
	})
}

// leasePrefix drops the unique last segment of a lease ID, leaving the
// path it was issued for, which is safe to record.
func leasePrefix(leaseID string) string {
	if i := strings.LastIndexByte(leaseID, '/'); i > 0 {
		return leaseID[:i]
	}
	return "lease"
}

// convert turns a Vault response into a secret.Secret.
func convert(path string, s *api.Secret) (secret.Secret, error) {
	if s == nil || s.Data == nil {
		return secret.Secret{}, errors.NotFound.Errorf("vault read %s: no secret", path)
	}
	data := s.Data
	var version string

	// KV v2 wraps the secret in data.data with metadata alongside.
	if inner, isKV := data["data"]; isKV {
		if meta, ok := data["metadata"].(map[string]any); ok {
			if inner == nil {
				return secret.Secret{}, errors.NotFound.Errorf("vault read %s: version deleted or destroyed", path)
			}
			m, ok := inner.(map[string]any)
			if !ok {
				return secret.Secret{}, errors.InvalidArgument.Errorf("vault read %s: malformed KV v2 data", path)
			}
			data = m
			switch v := meta["version"].(type) {
			case json.Number:
				version = v.String()
			case float64:
				version = strconv.FormatInt(int64(v), 10)
			}
		}
	}

	fields := make(map[string]string, len(data))
	for k, v := range data {
		switch v := v.(type) {
		case string:
			fields[k] = v
		case nil:
			fields[k] = ""
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return secret.Secret{}, errors.InvalidArgument.Errorf("vault read %s: field %q is not encodable", path, k)
			}
			fields[k] = string(b)
		}
	}
	out := secret.New(fields)
	out.Version = version
	out.LeaseID = s.LeaseID
	if s.LeaseDuration > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(s.LeaseDuration) * time.Second)
	}
	return out, nil
}

// login authenticates and records the token expiry. It returns the auth
// secret for renewal, or nil for a token that cannot be renewed.
func (c *Client) login(ctx context.Context) (*api.Secret, error) {
	var authSecret *api.Secret
	err := c.observe(ctx, "login", "auth", func(ctx context.Context) error {
		var err error
		if c.o.auth != nil {
			authSecret, err = c.api.Auth().Login(ctx, c.o.auth)
			if err == nil && (authSecret == nil || authSecret.Auth == nil) {
				err = errors.Forbidden.New("vault login: no auth info returned")
			}
			return err
		}
		if c.api.Token() == "" {
			return errors.InvalidArgument.New("vault: no auth method and no token configured")
		}
		authSecret, err = c.api.Auth().Token().LookupSelfWithContext(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}

	ttl, _ := authSecret.TokenTTL()
	renewable, _ := authSecret.TokenIsRenewable()
	c.mu.Lock()
	c.tokenExp = time.Time{}
	if ttl > 0 {
		c.tokenExp = time.Now().Add(ttl)
	}
	c.mu.Unlock()

	if !renewable || ttl == 0 {
		return nil, nil // nothing to renew: non-expiring, or valid until its TTL ends
	}
	if authSecret.Auth == nil {
		// Token lookup returns data, not auth info; renewal needs the latter.
		authSecret.Auth = &api.SecretAuth{ClientToken: c.api.Token(), Renewable: true, LeaseDuration: int(ttl.Seconds())}
	}
	return authSecret, nil
}

// keepAlive renews the token and logs in again when it can no longer be
// renewed.
func (c *Client) keepAlive(ctx context.Context, authSecret *api.Secret) {
	defer close(c.done)
	for {
		if authSecret != nil {
			if !c.watch(ctx, authSecret) {
				return
			}
		} else {
			// Not renewable: log in again shortly before it expires, if it does.
			c.mu.Lock()
			exp := c.tokenExp
			c.mu.Unlock()
			if exp.IsZero() || c.o.auth == nil {
				<-ctx.Done()
				return
			}
			if !sleep(ctx, time.Until(exp)*9/10) {
				return
			}
		}

		var ok bool
		authSecret, ok = c.relogin(ctx)
		if !ok {
			return
		}
	}
}

// watch renews the token until renewal stops being possible. It returns
// false if ctx ended.
func (c *Client) watch(ctx context.Context, authSecret *api.Secret) bool {
	w, err := c.api.NewLifetimeWatcher(&api.LifetimeWatcherInput{Secret: authSecret})
	if err != nil {
		c.o.logger.Warn("vault token renewal unavailable", logging.Err(err))
		return true
	}
	go w.Start()
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case err := <-w.DoneCh():
			if err != nil {
				c.o.logger.Warn("vault token renewal failed; logging in again", logging.Err(classify(ctx, "renew", "auth/token/renew-self", err)))
			} else {
				c.o.logger.Info("vault token reached its maximum TTL; logging in again")
			}
			return true
		case r := <-w.RenewCh():
			if r != nil && r.Secret != nil && r.Secret.Auth != nil {
				c.mu.Lock()
				c.tokenExp = time.Now().Add(time.Duration(r.Secret.Auth.LeaseDuration) * time.Second)
				c.mu.Unlock()
			}
		}
	}
}

func (c *Client) relogin(ctx context.Context) (*api.Secret, bool) {
	if c.o.auth == nil {
		c.o.logger.Error("vault token can no longer be renewed and no auth method is configured to log in again")
		<-ctx.Done()
		return nil, false
	}
	delay := c.o.minBackoff
	for {
		authSecret, err := c.login(ctx)
		if err == nil {
			c.o.logger.Info("vault login succeeded")
			return authSecret, true
		}
		c.mu.Lock()
		expired := !c.tokenExp.IsZero() && time.Now().After(c.tokenExp)
		c.mu.Unlock()
		level := slog.LevelWarn
		if expired {
			level = slog.LevelError
		}
		c.o.logger.Log(ctx, level, "vault login failed", "token_expired", expired, logging.Err(err))
		if !sleep(ctx, delay/2+rand.N(delay/2+1)) { //nolint:gosec // jitter
			return nil, false
		}
		delay = min(delay*2, c.o.maxBackoff)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(max(d, 0))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close stops token renewal and re-login, waiting for the background
// goroutine within ctx. It does not revoke the client token: tokens own the
// leases they created, and revoking it would revoke credentials that other
// resources may still be draining. Its signature matches graceful.Hook.
func (c *Client) Close(ctx context.Context) error {
	c.once.Do(func() {
		c.stop()
		select {
		case <-c.done:
		case <-ctx.Done():
			c.closeErr = errors.Timeout.Wrap(ctx.Err(), "vault: close")
		}
		c.unregister()
	})
	return c.closeErr
}
