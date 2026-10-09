// Package jwtauth verifies JSON Web Tokens (OAuth 2.0 access tokens and
// OpenID Connect ID tokens) signed by an identity provider that publishes
// its keys as a JWKS: Keycloak, Auth0, Okta, Azure AD / Entra ID, Google,
// Cognito, Dex, ZITADEL.
//
//	v, err := jwtauth.New(ctx, cfg.Auth) // AUTH_ISSUER, AUTH_AUDIENCE
//	...
//	srv := httpserver.NewServer(cfg.HTTP, httpserver.Chain(mux, httpserver.Auth(jwtauth.HTTP(v))), logger)
//	e.Use(echo.WrapMiddleware(httpserver.Auth(jwtauth.HTTP(v))))                    // Echo
//	grpcserver.New(grpcserver.WithAuth(grpcserver.Bearer(v.Authenticate), "/grpc.health.v1.Health/Check"))
//
// Handlers read the caller from the context:
//
//	claims, _ := jwtauth.FromContext(ctx)
//	if !claims.HasScope("orders:write") { return errors.Forbidden.New("…") }
//
// or require scopes on routes with [RequireScope].
//
// # What is verified
//
// The signature, with a key from the issuer's JWKS selected by the token's
// key ID; the algorithm, which must be one of Config.Algorithms and match
// the key (so a token cannot pick "none" or an HMAC algorithm keyed with a
// public key); the issuer; the audience, which must name one of
// Config.Audience; and the expiry and not-before times, with Config.Leeway
// for clock skew. Tokens without an expiry are rejected.
//
// Every failure is an error of kind Unauthorized, which httpserver and
// grpcserver turn into 401 and UNAUTHENTICATED without revealing why; the
// reason is in the error for logs and in the auth.tokens metric.
//
// # Keys and rotation
//
// New fetches the keys once, so a service with a wrong issuer or an
// unreachable provider fails at startup. Afterwards the keys are refreshed
// in the background every Config.RefreshInterval, and at once when a token
// names a key ID not in the cache (a key rotation), at most every
// Config.MinRefreshInterval so tokens with made-up key IDs cannot flood the
// provider. If a refresh fails, the cached keys stay in use.
package jwtauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/transport/http/httpclient"
)

// Config configures token verification. Environment variable names are
// relative; the service chooses the prefix, for example AUTH_.
type Config struct {
	// Issuer is the expected "iss" claim, such as
	// https://keycloak.example.com/realms/main.
	Issuer string `env:"ISSUER,required"`
	// Audience lists the accepted "aud" values (this API's identifiers); a
	// token must name at least one.
	Audience []string `env:"AUDIENCE,required" envSeparator:"," validate:"min=1"`
	// JWKSURL is where the issuer publishes its keys. Empty discovers it
	// from Issuer + /.well-known/openid-configuration.
	JWKSURL string `env:"JWKS_URL"`
	// Algorithms are the accepted signing algorithms.
	Algorithms []string `env:"ALGORITHMS" envSeparator:"," envDefault:"RS256,RS384,RS512,PS256,PS384,PS512,ES256,ES384,ES512,EdDSA"`
	// Leeway tolerates this much clock skew in exp, nbf and iat.
	Leeway time.Duration `env:"LEEWAY" envDefault:"30s"`
	// RefreshInterval is how often the keys are refreshed in the
	// background.
	RefreshInterval time.Duration `env:"JWKS_REFRESH_INTERVAL" envDefault:"15m"`
	// MinRefreshInterval is the least time between refreshes triggered by
	// unknown key IDs.
	MinRefreshInterval time.Duration `env:"JWKS_MIN_REFRESH_INTERVAL" envDefault:"30s"`
}

// Option configures [New].
type Option func(*options)

type options struct {
	client    *http.Client
	logger    *slog.Logger
	meterProv metric.MeterProvider
	now       func() time.Time
	noSubject bool
}

// WithoutSubjectInTelemetry keeps the token's subject out of logs and
// spans, for subjects that are personal data (an e-mail address, say)
// rather than opaque IDs.
func WithoutSubjectInTelemetry() Option { return func(o *options) { o.noSubject = true } }

// WithHTTPClient fetches the OpenID configuration and keys with c. The
// default is an httpclient with a 10s timeout and retries.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithLogger sets the logger for key refreshes. The default is
// slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProv = mp }
}

// WithClock replaces the clock used for expiry and refreshes, for tests.
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// Verifier verifies tokens. It is safe for concurrent use.
type Verifier struct {
	cfg     Config
	o       options
	jwksURL string
	parser  *jwt.Parser

	mu          sync.Mutex
	keys        map[string]key
	fetchedAt   time.Time
	lastAttempt time.Time
	refreshing  bool

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	tokens    metric.Int64Counter
	refreshes metric.Int64Counter
}

// New returns a Verifier for cfg, after fetching the issuer's keys. Stop it
// with [Verifier.Close].
func New(ctx context.Context, cfg Config, opts ...Option) (*Verifier, error) {
	if cfg.Issuer == "" || len(cfg.Audience) == 0 {
		return nil, errors.InvalidArgument.New("jwtauth: Config needs an Issuer and at least one Audience")
	}
	if len(cfg.Algorithms) == 0 {
		cfg.Algorithms = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	}
	for _, alg := range cfg.Algorithms {
		if m := jwt.GetSigningMethod(alg); m == nil || strings.HasPrefix(alg, "HS") || alg == "none" {
			return nil, errors.InvalidArgument.Errorf("jwtauth: algorithm %q is not an asymmetric JWS algorithm", alg)
		}
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 15 * time.Minute
	}
	if cfg.MinRefreshInterval <= 0 {
		cfg.MinRefreshInterval = 30 * time.Second
	}
	o := options{logger: slog.Default(), meterProv: otel.GetMeterProvider(), now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	if o.client == nil {
		o.client = httpclient.New(httpclient.Config{Timeout: 10 * time.Second},
			httpclient.WithName("jwks"), httpclient.WithRetry(), httpclient.WithLogger(o.logger),
			httpclient.WithMeterProvider(o.meterProv))
	}

	v := &Verifier{cfg: cfg, o: o, jwksURL: cfg.JWKSURL, stop: make(chan struct{}), done: make(chan struct{})}
	v.parser = jwt.NewParser(
		jwt.WithValidMethods(cfg.Algorithms),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience...),
		jwt.WithLeeway(cfg.Leeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(o.now),
	)
	meter := o.meterProv.Meter("github.com/Arif9878/common/go/auth/jwtauth")
	v.tokens, _ = meter.Int64Counter("auth.tokens",
		metric.WithDescription("Tokens verified, by outcome: ok, expired, invalid, unknown_key, keys_unavailable."))
	v.refreshes, _ = meter.Int64Counter("auth.jwks.refreshes",
		metric.WithDescription("JWKS fetches, by outcome: ok or failed."))

	if v.jwksURL == "" {
		u, err := discover(ctx, o.client, cfg.Issuer)
		if err != nil {
			return nil, err
		}
		v.jwksURL = u
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	go v.background() //nolint:gosec // the refresh outlives New's ctx; Close stops it
	return v, nil
}

// Close stops the background refresh. Its signature matches graceful.Hook.
func (v *Verifier) Close(context.Context) error {
	v.stopOnce.Do(func() { close(v.stop) })
	<-v.done
	return nil
}

func (v *Verifier) background() {
	defer close(v.done)
	t := time.NewTicker(v.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := v.refresh(ctx); err != nil {
				v.o.logger.Warn("jwtauth: key refresh failed; keeping the cached keys", logging.Err(err))
			}
			cancel()
		}
	}
}

// refresh fetches the keys and replaces the cache.
func (v *Verifier) refresh(ctx context.Context) error {
	v.mu.Lock()
	v.lastAttempt = v.o.now()
	v.mu.Unlock()
	b, err := fetch(ctx, v.o.client, v.jwksURL)
	var keys map[string]key
	if err == nil {
		if keys, err = parseJWKS(b); err != nil {
			err = errors.Unavailable.Wrap(err, "jwtauth: "+v.jwksURL)
		}
	}
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	v.refreshes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys, v.fetchedAt = keys, v.o.now()
	v.mu.Unlock()
	return nil
}

// keyFor returns the key with id, refreshing the cache once if it is
// unknown and the last attempt is old enough.
func (v *Verifier) keyFor(ctx context.Context, id string) (key, error) {
	v.mu.Lock()
	k, ok := v.lookup(id)
	canRefresh := !ok && v.o.now().Sub(v.lastAttempt) >= v.cfg.MinRefreshInterval && !v.refreshing
	if canRefresh {
		v.refreshing = true
	}
	v.mu.Unlock()
	if ok {
		return k, nil
	}
	if !canRefresh {
		return key{}, errUnknownKey
	}
	err := v.refresh(ctx)
	v.mu.Lock()
	v.refreshing = false
	k, ok = v.lookup(id)
	v.mu.Unlock()
	switch {
	case ok:
		return k, nil
	case err != nil:
		return key{}, errors.Join(errKeysUnavailable, err)
	}
	return key{}, errUnknownKey
}

// lookup finds id; a token without a key ID matches the only key, if the
// JWKS has one. v.mu is held.
func (v *Verifier) lookup(id string) (key, bool) {
	if id == "" && len(v.keys) == 1 {
		for _, k := range v.keys {
			return k, true
		}
	}
	k, ok := v.keys[id]
	return k, ok && id != ""
}

var (
	errUnknownKey      = errors.Unauthorized.New("jwtauth: token signed with an unknown key")
	errKeysUnavailable = errors.Unauthorized.New("jwtauth: token key not cached and the JWKS is unreachable")
)

// Verify checks token (without a "Bearer " prefix) and returns its claims.
// Errors have kind Unauthorized.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	claims := &Claims{}
	_, err := v.parser.ParseWithClaims(token, &claims.raw, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, err := v.keyFor(ctx, kid)
		if err != nil {
			return nil, err
		}
		if k.alg != "" && k.alg != t.Method.Alg() {
			return nil, fmt.Errorf("token algorithm %s does not match key algorithm %s", t.Method.Alg(), k.alg)
		}
		return publicKeyFor(t.Method, k.pub)
	})
	outcome := "ok"
	switch {
	case err == nil:
	case errors.Is(err, jwt.ErrTokenExpired):
		outcome = "expired"
	case errors.Is(err, errUnknownKey):
		outcome = "unknown_key"
	case errors.Is(err, errKeysUnavailable):
		outcome = "keys_unavailable"
	default:
		outcome = "invalid"
	}
	v.tokens.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	if err != nil {
		if errors.KindOf(err) != errors.Unauthorized {
			err = errors.Unauthorized.Wrap(err, "jwtauth: invalid token")
		}
		return nil, errors.WithPublicMessage(err, "invalid or expired token")
	}
	claims.fill()
	return claims, nil
}

// publicKeyFor checks that pub suits method's family, so an RSA key is
// never used to verify an EC signature and the reverse.
func publicKeyFor(m jwt.SigningMethod, pub crypto.PublicKey) (crypto.PublicKey, error) {
	ok := false
	switch m.(type) {
	case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
		_, ok = pub.(*rsa.PublicKey)
	case *jwt.SigningMethodECDSA:
		_, ok = pub.(*ecdsa.PublicKey)
	case *jwt.SigningMethodEd25519:
		_, ok = pub.(ed25519.PublicKey)
	}
	if !ok {
		return nil, fmt.Errorf("key type does not match algorithm %s", m.Alg())
	}
	return pub, nil
}

// Authenticate verifies token and returns ctx carrying its claims, for
// grpcserver.Bearer and other transports. The subject is added to the
// context's logs as user_id and to the current span as user.id, unless
// WithoutSubjectInTelemetry is set.
func (v *Verifier) Authenticate(ctx context.Context, token string) (context.Context, error) {
	c, err := v.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	ctx = NewContext(ctx, c)
	if !v.o.noSubject && c.Subject != "" {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("user.id", c.Subject))
		ctx = logging.ContextWithAttrs(ctx, slog.String(logging.KeyUserID, c.Subject))
	}
	return ctx, nil
}
