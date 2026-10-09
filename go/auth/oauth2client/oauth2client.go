// Package oauth2client obtains access tokens with the OAuth 2.0 client
// credentials grant, for calls from one service to another, and attaches
// them to HTTP and gRPC requests. Tokens are cached and replaced shortly
// before they expire, so callers pay for a token request about once per
// token lifetime.
//
//	src := oauth2client.New(cfg.PaymentsAuth) // TOKEN_URL, CLIENT_ID, CLIENT_SECRET, SCOPES
//	client := httpclient.New(cfg.Payments,
//		httpclient.WithBaseTransport(src.Transport(httpclient.NewTransport(cfg.Payments))))
//	conn, err := grpcclient.New(target, grpcclient.WithDialOptions(grpc.WithPerRPCCredentials(src)))
//
// The receiving service verifies the tokens with jwtauth.
package oauth2client

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/transport/http/httpclient"
)

// Config configures a client. Environment variable names are relative; the
// service chooses the prefix, for example PAYMENTS_AUTH_.
type Config struct {
	// TokenURL is the identity provider's token endpoint, such as
	// https://keycloak.example.com/realms/main/protocol/openid-connect/token.
	TokenURL string `env:"TOKEN_URL,required"`
	// ClientID and ClientSecret identify this service to the provider.
	ClientID     string        `env:"CLIENT_ID,required"`
	ClientSecret config.Secret `env:"CLIENT_SECRET,required"`
	// Scopes requested for the token.
	Scopes []string `env:"SCOPES" envSeparator:","`
	// Audience, if set, is sent as the "audience" parameter, which Auth0,
	// Okta and others use to choose the API the token is for.
	Audience string `env:"AUDIENCE"`
	// EarlyExpiry replaces a token this long before it expires, so a
	// request never leaves with a token about to expire.
	EarlyExpiry time.Duration `env:"EARLY_EXPIRY" envDefault:"30s"`
	// Timeout bounds each token request.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10s"`
}

// Option configures [New].
type Option func(*options)

type options struct {
	client    *http.Client
	logger    *slog.Logger
	meterProv metric.MeterProvider
	insecure  bool
	name      string
}

// WithHTTPClient sends token requests with c. The default is an httpclient
// with retries.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProv = mp }
}

// WithName labels the source's metrics, such as the downstream service. It
// must be a fixed, low-cardinality value. The default is the client ID.
func WithName(name string) Option { return func(o *options) { o.name = name } }

// WithInsecureTransport lets gRPC send the token over a connection without
// TLS, for a local network. Never use it across untrusted networks.
func WithInsecureTransport() Option { return func(o *options) { o.insecure = true } }

// Source provides access tokens. It is safe for concurrent use.
type Source struct {
	ts       oauth2.TokenSource
	insecure bool
}

// New returns a Source for cfg. It requests no token until one is needed.
func New(cfg Config, opts ...Option) *Source {
	o := options{logger: slog.Default(), meterProv: otel.GetMeterProvider(), name: cfg.ClientID}
	for _, opt := range opts {
		opt(&o)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if o.client == nil {
		o.client = httpclient.New(httpclient.Config{Timeout: timeout},
			httpclient.WithName("oauth2-token"), httpclient.WithRetry(), httpclient.WithLogger(o.logger),
			httpclient.WithMeterProvider(o.meterProv))
	}
	cc := clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret.Reveal(),
		TokenURL:     cfg.TokenURL,
		Scopes:       cfg.Scopes,
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
	if cfg.Audience != "" {
		cc.EndpointParams = url.Values{"audience": {cfg.Audience}}
	}
	early := cfg.EarlyExpiry
	if early <= 0 {
		early = 30 * time.Second
	}
	fetches, _ := o.meterProv.Meter("github.com/Arif9878/common/go/auth/oauth2client").Int64Counter("oauth2.token.requests",
		metric.WithDescription("Token requests to the identity provider, by client and outcome: ok or the error kind."))
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, o.client)
	inner := &fetcher{ts: cc.TokenSource(ctx), name: o.name, counter: fetches, logger: o.logger}
	return &Source{ts: oauth2.ReuseTokenSourceWithExpiry(nil, inner, early), insecure: o.insecure}
}

// fetcher requests a token from the provider, for the cache in front of it.
type fetcher struct {
	ts      oauth2.TokenSource
	name    string
	counter metric.Int64Counter
	logger  *slog.Logger
}

func (f *fetcher) Token() (*oauth2.Token, error) {
	t, err := f.ts.Token()
	err = classify(err)
	outcome := "ok"
	if err != nil {
		outcome = errors.KindOf(err).String()
		f.logger.Warn("oauth2 token request failed", "client", f.name, logging.Err(err))
	}
	f.counter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("client", f.name), attribute.String("outcome", outcome)))
	return t, err
}

// classify gives token errors a kind: a rejected client or request
// (invalid_client, invalid_scope) is Unauthorized or InvalidArgument and
// not worth retrying; anything else is Unavailable.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok && re.Response != nil {
		switch code := re.Response.StatusCode; {
		case code == http.StatusUnauthorized || re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client":
			return errors.Unauthorized.Wrap(err, "oauth2client: client rejected")
		case code >= 400 && code < 500:
			return errors.InvalidArgument.Wrap(err, "oauth2client: token request rejected")
		}
	}
	return errors.Unavailable.Wrap(err, "oauth2client: token request")
}

// Token returns a valid access token, from the cache when possible.
func (s *Source) Token() (*oauth2.Token, error) { return s.ts.Token() }

// Transport returns a RoundTripper that adds "Authorization: Bearer
// <token>" to every request and sends it with base (http.DefaultTransport
// if nil). A request that cannot get a token fails without being sent.
func (s *Source) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{src: s, base: base}
}

type transport struct {
	src  *Source
	base http.RoundTripper
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, err := t.src.Token()
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	return t.base.RoundTrip(r2)
}

// GetRequestMetadata adds the token to a gRPC call. With RequireTransportSecurity,
// Source implements credentials.PerRPCCredentials for grpc.WithPerRPCCredentials.
func (s *Source) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	tok, err := s.Token()
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + tok.AccessToken}, nil
}

// RequireTransportSecurity reports whether gRPC must use TLS to send the
// token: true unless WithInsecureTransport was given.
func (s *Source) RequireTransportSecurity() bool { return !s.insecure }
