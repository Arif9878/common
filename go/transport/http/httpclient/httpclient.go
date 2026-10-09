// Package httpclient builds *http.Client values with the organization's
// defaults: pooled connections with bounded timeouts, tracing and metrics,
// request-ID propagation, and optional retry and circuit breaking.
//
//	client := httpclient.New(cfg.PaymentsHTTP,
//		httpclient.WithName("payments"),
//		httpclient.WithRetry(retry.WithMaxAttempts(3)),
//		httpclient.WithCircuitBreaker(circuitbreaker.New("payments")),
//	)
//	defer client.CloseIdleConnections()
//
// The result is a plain *http.Client; use it with http.NewRequestWithContext
// so cancellation and deadlines propagate.
//
// # Per attempt
//
// Every attempt is a separate client span (otelhttp, with W3C trace context
// injected into the request headers) and is measured as
// http.client.request.duration. The X-Request-ID header is set from the
// context when the request does not carry one.
//
// # Retries
//
// Retry is off unless [WithRetry] is given. A request is retried only when
// repeating it is safe: its method is idempotent (GET, HEAD, OPTIONS, TRACE,
// PUT, DELETE) or it carries an Idempotency-Key header, and its body can be
// re-sent (it has none, or http.Request.GetBody is set, as it is for
// bodies from bytes, strings and bytes.Buffer readers).
//
// Retried outcomes are network errors, 429, 502, 503 and 504; a Retry-After
// header sets the minimum delay. When attempts run out on a retryable
// status, the last response is returned as is, with a nil error, exactly as
// without retries: callers always handle status codes themselves. Error
// results are classified (kinds Unavailable, Timeout, Canceled) so they map
// cleanly through the errors package.
//
// # Circuit breaking
//
// With [WithCircuitBreaker], each attempt goes through the breaker. Network
// errors and 500, 502, 503, 504 and 429 responses count as failures; other
// statuses count as successes. While the breaker is open, attempts fail
// with circuitbreaker.ErrOpen without sending anything.
package httpclient

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/resilience/circuitbreaker"
	"github.com/Arif9878/common/go/resilience/retry"
)

// Config configures connection handling. Zero fields use the defaults
// shown. Environment variable names are relative.
type Config struct {
	// Timeout bounds a whole call, including retries and reading the body.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"30s"`
	// DialTimeout bounds opening one TCP connection.
	DialTimeout time.Duration `env:"DIAL_TIMEOUT" envDefault:"5s"`
	// KeepAlive is the TCP keep-alive interval.
	KeepAlive time.Duration `env:"KEEP_ALIVE" envDefault:"30s"`
	// TLSHandshakeTimeout bounds the TLS handshake.
	TLSHandshakeTimeout time.Duration `env:"TLS_HANDSHAKE_TIMEOUT" envDefault:"5s"`
	// ResponseHeaderTimeout bounds waiting for response headers per
	// attempt. Zero means no per-attempt limit beyond Timeout.
	ResponseHeaderTimeout time.Duration `env:"RESPONSE_HEADER_TIMEOUT"`
	// IdleConnTimeout closes idle connections after this long.
	IdleConnTimeout time.Duration `env:"IDLE_CONN_TIMEOUT" envDefault:"90s"`
	// MaxIdleConns bounds idle connections across all hosts.
	MaxIdleConns int `env:"MAX_IDLE_CONNS" envDefault:"100"`
	// MaxIdleConnsPerHost defaults to 20, not net/http's 2, which causes
	// connection churn under load to a single host.
	MaxIdleConnsPerHost int `env:"MAX_IDLE_CONNS_PER_HOST" envDefault:"20"`
	// MaxConnsPerHost limits all connections to one host; 0 means no limit.
	MaxConnsPerHost int `env:"MAX_CONNS_PER_HOST"`
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.Timeout, 30*time.Second)
	def(&c.DialTimeout, 5*time.Second)
	def(&c.KeepAlive, 30*time.Second)
	def(&c.TLSHandshakeTimeout, 5*time.Second)
	def(&c.IdleConnTimeout, 90*time.Second)
	if c.MaxIdleConns == 0 {
		c.MaxIdleConns = 100
	}
	if c.MaxIdleConnsPerHost == 0 {
		c.MaxIdleConnsPerHost = 20
	}
	return c
}

// NewTransport returns the tuned *http.Transport used by [New], for callers
// that need to adjust it further and pass it back with [WithBaseTransport].
func NewTransport(cfg Config) *http.Transport {
	cfg = cfg.withDefaults()
	dialer := &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: cfg.KeepAlive}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// Option configures [New].
type Option func(*options)

type options struct {
	name        string
	base        http.RoundTripper
	tlsConfig   *tls.Config
	retry       []retry.Option
	breaker     *circuitbreaker.Breaker
	logger      *slog.Logger
	meterProv   metric.MeterProvider
	tracerProv  trace.TracerProvider
	propagators propagation.TextMapPropagator
}

// WithName names the client in logs and retry metrics, typically the
// dependency ("payments"). The default is "http".
func WithName(name string) Option { return func(o *options) { o.name = name } }

// WithBaseTransport replaces the transport that sends requests, for example
// a customized [NewTransport]. Connection settings in Config are then
// ignored.
func WithBaseTransport(rt http.RoundTripper) Option { return func(o *options) { o.base = rt } }

// WithTLSConfig sets the TLS configuration of the default transport, for
// example client certificates for mTLS or a private CA.
func WithTLSConfig(c *tls.Config) Option { return func(o *options) { o.tlsConfig = c } }

// WithRetry enables retries with a policy built from opts; see the package
// documentation for what is retried.
func WithRetry(opts ...retry.Option) Option {
	return func(o *options) { o.retry = append([]retry.Option{}, opts...) }
}

// WithCircuitBreaker sends every attempt through b.
func WithCircuitBreaker(b *circuitbreaker.Breaker) Option { return func(o *options) { o.breaker = b } }

// WithLogger sets the logger for retries. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.meterProv = mp } }

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// WithPropagators sets how trace context is written to requests. The
// default is the global propagator.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *options) { o.propagators = p }
}

// New returns an *http.Client configured by cfg and opts.
func New(cfg Config, opts ...Option) *http.Client {
	cfg = cfg.withDefaults()
	o := options{
		name:        "http",
		logger:      slog.Default(),
		meterProv:   otel.GetMeterProvider(),
		tracerProv:  otel.GetTracerProvider(),
		propagators: otel.GetTextMapPropagator(),
	}
	for _, opt := range opts {
		opt(&o)
	}

	base := o.base
	if base == nil {
		t := NewTransport(cfg)
		if o.tlsConfig != nil {
			t.TLSClientConfig = o.tlsConfig
		}
		base = t
	}
	traced := otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(o.tracerProv),
		otelhttp.WithMeterProvider(o.meterProv),
		otelhttp.WithPropagators(o.propagators),
	)

	rt := &roundTripper{name: o.name, next: traced, base: base, breaker: o.breaker, logger: o.logger}
	if o.retry != nil {
		logger, name := o.logger, o.name
		rt.policy = retry.New(append([]retry.Option{
			retry.WithName(name),
			retry.WithMeterProvider(o.meterProv),
			retry.WithOnRetry(func(attempt int, err error, delay time.Duration) {
				logger.Warn("http request failed, retrying", "client", name,
					"attempt", attempt, "delay", delay.String(), logging.Err(err))
			}),
		}, o.retry...)...)
	}
	return &http.Client{Transport: rt, Timeout: cfg.Timeout}
}

// roundTripper adds request IDs, circuit breaking and retries around next.
type roundTripper struct {
	name    string
	next    http.RoundTripper
	base    http.RoundTripper
	policy  *retry.Policy
	breaker *circuitbreaker.Breaker
	logger  *slog.Logger
}

// CloseIdleConnections closes idle connections of the base transport, so
// http.Client.CloseIdleConnections works through the wrappers.
func (rt *roundTripper) CloseIdleConnections() {
	if c, ok := rt.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if id, ok := requestid.FromContext(req.Context()); ok && req.Header.Get(requestid.Header) == "" {
		req = req.Clone(req.Context()) // RoundTrip must not modify the caller's request
		req.Header.Set(requestid.Header, id)
	}
	if rt.policy == nil || !replayable(req) {
		return rt.attempt(req)
	}

	var last *http.Response
	err := rt.policy.Do(req.Context(), func(ctx context.Context) error {
		attemptReq := req
		if last != nil {
			discard(last)
			last = nil
			var err error
			if attemptReq, err = rewind(req); err != nil {
				return retry.Permanent(err)
			}
		}
		resp, err := rt.attempt(attemptReq) //nolint:bodyclose // closed by discard or returned to the caller
		if err != nil {
			return err
		}
		last = resp
		return statusError(resp)
	})

	if last != nil {
		if err == nil || isStatusError(err) && req.Context().Err() == nil {
			return last, nil // retries exhausted on a status: return it as is
		}
		discard(last)
	}
	return nil, err
}

// attempt sends one request through the breaker, if any.
func (rt *roundTripper) attempt(req *http.Request) (*http.Response, error) {
	if rt.breaker == nil {
		resp, err := rt.next.RoundTrip(req)
		return resp, classify(err)
	}
	var resp *http.Response
	err := rt.breaker.Execute(req.Context(), func(context.Context) error {
		var err error
		resp, err = rt.next.RoundTrip(req) //nolint:bodyclose // returned to the caller, who closes it
		if err != nil {
			return classify(err)
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return &httpStatusError{code: resp.StatusCode} // breaker failure; response still returned
		}
		return nil
	})
	if resp != nil {
		return resp, nil
	}
	return nil, err
}

// replayable reports whether req may safely be sent more than once.
func replayable(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
	default:
		if req.Header.Get("Idempotency-Key") == "" {
			return false
		}
	}
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

func rewind(req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Body = body
	return r, nil
}

// discard drains a little of the body, so the connection can be reused,
// and closes it.
func discard(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
}

// httpStatusError is a retryable or breaker-relevant status.
type httpStatusError struct {
	code       int
	retryAfter time.Duration
}

func (e *httpStatusError) Error() string             { return "http status " + strconv.Itoa(e.code) }
func (e *httpStatusError) RetryAfter() time.Duration { return e.retryAfter }

func isStatusError(err error) bool {
	_, ok := errors.AsType[*httpStatusError](err)
	return ok
}

// statusError returns a classified error for statuses worth retrying.
func statusError(resp *http.Response) error {
	e := &httpStatusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return errors.RateLimited.Wrap(e, "")
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return errors.Unavailable.Wrap(e, "")
	case http.StatusGatewayTimeout:
		return errors.Timeout.Wrap(e, "")
	default:
		return nil
	}
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// classify gives transport errors a kind: caller cancellation and deadlines
// keep their context kinds; other network failures are Unavailable.
func classify(err error) error {
	if err == nil || errors.KindOf(err) != errors.Unknown {
		return err
	}
	return errors.Unavailable.Wrap(err, "")
}
