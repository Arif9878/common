// Package tracing sets up OpenTelemetry tracing for a service.
//
// Call [Init] once in main, pass [WithGlobal] so that instrumentation
// libraries (otelhttp, otelgrpc, kotel, otelpgx) pick up the provider and
// the W3C trace-context propagator, and shut it down last so buffered spans
// are flushed:
//
//	tp, err := tracing.Init(ctx, cfg.Tracing, tracing.WithGlobal())
//	if err != nil {
//		return err
//	}
//	defer tp.Shutdown(context.WithoutCancel(ctx))
//
// Packages should not import this package to create spans. They take a
// trace.TracerProvider option (defaulting to otel.GetTracerProvider()) and
// use the OpenTelemetry API directly; [End] is the only span helper here.
//
// # Configuration
//
// Explicit [Config] fields win. Fields left empty fall back to the standard
// OpenTelemetry environment variables, which the SDK reads itself:
// OTEL_EXPORTER_OTLP_(TRACES_)ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS,
// OTEL_TRACES_SAMPLER(_ARG), OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES.
//
// # Failure behavior
//
// Exporting is asynchronous and never blocks request handling. Spans are
// buffered in a bounded queue (2048 by default) and dropped when it is full,
// for example while the collector is unreachable. The OTLP exporters connect
// lazily, so Init succeeds even if the collector is down.
//
// With Exporter "none", spans are still created and sampled, so logs carry
// trace IDs and context is propagated downstream, but nothing is exported.
package tracing

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/internal/otelres"
	"github.com/Arif9878/common/go/observability/internal/otlpconf"
	"github.com/Arif9878/common/go/observability/otlp"
)

// Exporter names accepted in [Config].
const (
	ExporterNone = "none"
	// ExporterOTLP exports over OTLP with the protocol of the shared
	// otlp.Config ([WithOTLP]), else OTEL_EXPORTER_OTLP_PROTOCOL, else gRPC.
	ExporterOTLP     = "otlp"
	ExporterOTLPGRPC = "otlp-grpc"
	ExporterOTLPHTTP = "otlp-http"
)

// Config configures tracing. The zero value creates spans without exporting
// them. Environment variable names are relative; the service chooses the
// prefix, for example TRACING_.
type Config struct {
	// Exporter is "none", "otlp" (protocol from the shared otlp.Config),
	// "otlp-grpc" or "otlp-http".
	Exporter string `env:"EXPORTER" envDefault:"none"`
	// Endpoint overrides the shared otlp.Config endpoint for traces, as
	// host:port or a URL. Empty uses the shared endpoint, then the
	// OTEL_EXPORTER_OTLP_* variables, then the exporter's default
	// (localhost:4317 for gRPC, localhost:4318 for HTTP).
	Endpoint string `env:"ENDPOINT"`
	// Insecure disables TLS to the collector.
	Insecure bool `env:"INSECURE"`
	// SampleRatio, if set, samples this fraction (0 to 1) of new traces and
	// follows the parent's decision for propagated traces. Nil uses
	// OTEL_TRACES_SAMPLER or, if unset, samples everything.
	SampleRatio *float64 `env:"SAMPLE_RATIO"`

	// Service, Environment and Version describe the service in every span.
	Service     string `env:"SERVICE"`
	Environment string `env:"ENVIRONMENT"`
	Version     string `env:"VERSION"`
}

var errUnknownExporter = stderrors.New("unknown exporter (want none, otlp, otlp-grpc or otlp-http)")

// Validate reports whether cfg is valid. [Init] calls it.
func (cfg Config) Validate() error {
	switch strings.ToLower(cfg.Exporter) {
	case "", ExporterNone, ExporterOTLP, ExporterOTLPGRPC, ExporterOTLPHTTP:
	default:
		return errUnknownExporter
	}
	if r := cfg.SampleRatio; r != nil && (*r < 0 || *r > 1) {
		return stderrors.New("sample ratio outside [0, 1]")
	}
	return nil
}

// Option configures [Init].
type Option func(*options)

type options struct {
	exporter sdktrace.SpanExporter
	global   bool
	otlp     otlp.Config
}

// WithOTLP sets the shared OTLP connection settings: endpoint, headers
// (such as an API key), protocol, TLS, compression and timeout. Config's
// Endpoint and Insecure override them for traces.
func WithOTLP(cfg otlp.Config) Option {
	return func(o *options) { o.otlp = cfg }
}

// WithExporter uses exp instead of the exporter named in Config, for
// example an in-memory exporter in tests.
func WithExporter(exp sdktrace.SpanExporter) Option {
	return func(o *options) { o.exporter = exp }
}

// WithGlobal registers the provider and [Propagator] as the OpenTelemetry
// globals. Instrumentation libraries use the globals by default.
func WithGlobal() Option {
	return func(o *options) { o.global = true }
}

// Provider owns the tracer provider created by [Init].
type Provider struct {
	tp *sdktrace.TracerProvider

	shutdownOnce sync.Once
	shutdownErr  error
}

// Init creates a tracer provider from cfg. It returns an error for an
// unknown exporter, a sample ratio outside [0, 1] or an invalid endpoint.
func Init(ctx context.Context, cfg Config, opts ...Option) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	res, err := otelres.New(ctx, cfg.Service, cfg.Environment, cfg.Version)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}

	if r := cfg.SampleRatio; r != nil {
		tpOpts = append(tpOpts, sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*r))))
	}

	exp := o.exporter
	if exp == nil {
		if exp, err = newExporter(ctx, cfg, o.otlp); err != nil {
			return nil, fmt.Errorf("tracing: %w", err)
		}
	}
	if exp != nil {
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	if o.global {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(Propagator())
	}
	return &Provider{tp: tp}, nil
}

func newExporter(ctx context.Context, cfg Config, shared otlp.Config) (sdktrace.SpanExporter, error) {
	exporter := strings.ToLower(cfg.Exporter)
	if exporter == "" || exporter == ExporterNone {
		return nil, nil
	}
	set, err := otlpconf.Resolve("TRACES", exporter, shared, cfg.Endpoint, cfg.Insecure)
	if err != nil {
		return nil, err
	}
	if set.Protocol == otlp.ProtocolGRPC {
		var opts []otlptracegrpc.Option
		switch {
		case set.EndpointURL:
			opts = append(opts, otlptracegrpc.WithEndpointURL(set.Endpoint))
		case set.Endpoint != "":
			opts = append(opts, otlptracegrpc.WithEndpoint(set.Endpoint))
		}
		if set.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if set.Headers != nil {
			opts = append(opts, otlptracegrpc.WithHeaders(set.Headers))
		}
		if set.Compression == "gzip" { // gRPC exports are uncompressed by default
			opts = append(opts, otlptracegrpc.WithCompressor("gzip"))
		}
		if set.Timeout > 0 {
			opts = append(opts, otlptracegrpc.WithTimeout(set.Timeout))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	var opts []otlptracehttp.Option
	switch {
	case set.EndpointURL:
		opts = append(opts, otlptracehttp.WithEndpointURL(set.Endpoint))
	case set.Endpoint != "":
		opts = append(opts, otlptracehttp.WithEndpoint(set.Endpoint))
	}
	if set.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if set.Headers != nil {
		opts = append(opts, otlptracehttp.WithHeaders(set.Headers))
	}
	switch set.Compression {
	case "gzip":
		opts = append(opts, otlptracehttp.WithCompression(otlptracehttp.GzipCompression))
	case "none":
		opts = append(opts, otlptracehttp.WithCompression(otlptracehttp.NoCompression))
	}
	if set.Timeout > 0 {
		opts = append(opts, otlptracehttp.WithTimeout(set.Timeout))
	}
	return otlptracehttp.New(ctx, opts...)
}

// TracerProvider returns the provider for passing to instrumentation.
func (p *Provider) TracerProvider() trace.TracerProvider { return p.tp }

// ForceFlush exports all buffered spans.
func (p *Provider) ForceFlush(ctx context.Context) error { return p.tp.ForceFlush(ctx) }

// Shutdown flushes buffered spans and stops the provider. It returns when
// the flush completes or ctx is done. Only the first call has an effect;
// later calls return the first result.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.shutdownOnce.Do(func() { p.shutdownErr = p.tp.Shutdown(ctx) })
	return p.shutdownErr
}

// Propagator returns the standard propagator: W3C trace context and baggage.
func Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

// End ends span, first recording *errp if it is non-nil: the span status is
// set to Error and error.type to the [errors.Kind] of the error. Use it with
// a named error result:
//
//	func (s *Store) Load(ctx context.Context, id string) (_ *Order, err error) {
//		ctx, span := s.tracer.Start(ctx, "Store.Load")
//		defer tracing.End(span, &err)
//		...
//	}
//
// The error text is recorded as a span event, just as it would be logged;
// see the logging package for what must not appear in error strings.
func End(span trace.Span, errp *error) {
	if errp != nil && *errp != nil {
		err := *errp
		kind := errors.KindOf(err).String()
		span.SetAttributes(semconv.ErrorTypeKey.String(kind))
		span.RecordError(err)
		span.SetStatus(codes.Error, kind)
	}
	span.End()
}
