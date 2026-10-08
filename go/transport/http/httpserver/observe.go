package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/observability/logging"
)

// Option configures [Handler] and [Observe].
type Option func(*options)

type options struct {
	logger      *slog.Logger
	meterProv   metric.MeterProvider
	tracerProv  trace.TracerProvider
	propagators propagation.TextMapPropagator
	route       func(*http.Request) string
	filter      func(*http.Request) bool
	maxBytes    int64
	timeout     time.Duration
}

func newOptions(opts []Option) options {
	o := options{
		logger:      slog.Default(),
		meterProv:   otel.GetMeterProvider(),
		tracerProv:  otel.GetTracerProvider(),
		propagators: otel.GetTextMapPropagator(),
		maxBytes:    1 << 20,
		timeout:     30 * time.Second,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProv = mp }
}

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// WithPropagators sets how trace context is read from requests. The default
// is the global propagator (tracing.WithGlobal installs W3C trace context).
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *options) { o.propagators = p }
}

// WithRoute sets how the route template of a request is found, for routers
// other than *http.ServeMux. fn must return a template such as
// "/orders/{id}", never the raw path.
func WithRoute(fn func(*http.Request) string) Option {
	return func(o *options) { o.route = fn }
}

// WithFilter skips the access log and trace span for requests for which fn
// returns false, such as health probes. Metrics are still recorded.
func WithFilter(fn func(*http.Request) bool) Option {
	return func(o *options) { o.filter = fn }
}

// WithMaxBodyBytes sets the request body limit used by [Handler]. The
// default is 1 MiB; 0 disables the limit.
func WithMaxBodyBytes(n int64) Option { return func(o *options) { o.maxBytes = n } }

// WithTimeout sets the request deadline used by [Handler]. The default is
// 30 seconds; 0 disables it (for streaming endpoints).
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// Handler wraps next in the standard middleware chain described in the
// package documentation. If next is an *http.ServeMux and WithRoute is not
// given, routes are resolved from the mux.
func Handler(next http.Handler, opts ...Option) http.Handler {
	o := newOptions(opts)
	if mux, ok := next.(*http.ServeMux); ok && o.route == nil {
		o.route = ServeMuxRoute(mux)
	}
	mw := []Middleware{RequestID(), observe(o), Recover(o.logger)}
	if o.maxBytes > 0 {
		mw = append(mw, MaxBytes(o.maxBytes))
	}
	if o.timeout > 0 {
		mw = append(mw, Timeout(o.timeout))
	}
	return Chain(next, mw...)
}

// ServeMuxRoute returns a route function for mux: the path part of the
// pattern that matches the request, such as "/orders/{id}" for the
// pattern "GET /orders/{id}".
func ServeMuxRoute(mux *http.ServeMux) func(*http.Request) string {
	return func(r *http.Request) string {
		_, pattern := mux.Handler(r)
		if i := strings.IndexByte(pattern, ' '); i >= 0 {
			pattern = pattern[i+1:]
		}
		return pattern
	}
}

// Observe traces, measures and logs each request; see the package
// documentation. It records the metrics http.server.request.duration and
// http.server.active_requests with OpenTelemetry semantic-convention
// attributes.
func Observe(opts ...Option) Middleware {
	return observe(newOptions(opts))
}

type observer struct {
	o        options
	duration metric.Float64Histogram
	active   metric.Int64UpDownCounter
}

func observe(o options) Middleware {
	meter := o.meterProv.Meter("github.com/Arif9878/common/go/transport/http/httpserver")
	ob := &observer{o: o}
	ob.duration, _ = meter.Float64Histogram("http.server.request.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests."))
	ob.active, _ = meter.Int64UpDownCounter("http.server.active_requests", metric.WithUnit("{request}"),
		metric.WithDescription("Number of in-flight HTTP server requests."))

	return func(next http.Handler) http.Handler {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ob.serve(w, r, next)
		})
		otelOpts := []otelhttp.Option{
			otelhttp.WithTracerProvider(o.tracerProv),
			otelhttp.WithPropagators(o.propagators),
			otelhttp.WithMeterProvider(noop.NewMeterProvider()), // metrics are recorded below, with routes
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method }),
		}
		if o.filter != nil {
			otelOpts = append(otelOpts, otelhttp.WithFilter(o.filter))
		}
		return otelhttp.NewHandler(inner, "http.server", otelOpts...)
	}
}

func (ob *observer) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	start := time.Now()
	route := "unmatched"
	if ob.o.route != nil {
		if rt := ob.o.route(r); rt != "" {
			route = rt
		}
	}
	method := normalizeMethod(r.Method)

	span := trace.SpanFromContext(r.Context())
	span.SetName(method + " " + route)
	span.SetAttributes(semconv.HTTPRoute(route))

	st := &requestState{}
	ctx := context.WithValue(r.Context(), stateKey{}, st)
	rw := &responseWriter{ResponseWriter: w}

	methodAttr := metric.WithAttributes(semconv.HTTPRequestMethodKey.String(method))
	ob.active.Add(ctx, 1, methodAttr)
	defer func() {
		ob.active.Add(ctx, -1, methodAttr)
		status := rw.statusCode()
		elapsed := time.Since(start)

		attrs := []attribute.KeyValue{
			semconv.HTTPRequestMethodKey.String(method),
			semconv.HTTPRoute(route),
			semconv.HTTPResponseStatusCode(status),
		}
		if status >= 500 {
			attrs = append(attrs, semconv.ErrorTypeKey.String(strconv.Itoa(status)))
		}
		ob.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(attrs...))

		if ob.o.filter != nil && !ob.o.filter(r) {
			return
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}
		ob.o.logger.LogAttrs(ctx, level, "http request",
			slog.String("method", method),
			slog.String("route", route),
			slog.Int("status", status),
			logging.Duration(elapsed),
			slog.Int64("bytes", rw.bytes),
			logging.Err(st.err),
		)
	}()
	next.ServeHTTP(rw, r.WithContext(ctx))
}

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodConnect: true,
	http.MethodOptions: true, http.MethodTrace: true,
}

// normalizeMethod bounds the method attribute, as the semantic conventions
// require: unknown methods become "_OTHER".
func normalizeMethod(m string) string {
	if knownMethods[m] {
		return m
	}
	return "_OTHER"
}

type stateKey struct{}

// requestState carries per-request data from inner handlers to Observe.
// Only the handler goroutine writes it, before Observe reads it.
type requestState struct {
	err error
}

func recordError(ctx context.Context, err error) {
	if st, ok := ctx.Value(stateKey{}).(*requestState); ok && st.err == nil {
		st.err = err
	}
}

// responseWriter records the status and size of a response. It supports
// http.ResponseController (Unwrap) and http.Flusher.
type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *responseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK // handler wrote nothing: net/http sends 200
	}
	return w.status
}
