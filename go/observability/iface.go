package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	ObservabilityTelemetry = "opentelemetry"
)

type Protocol string
type Datastore string

const (
	GRPC Protocol = "GRPC"
	HTTP Protocol = "HTTP"
)

const (
	DatastoreMySQL    Datastore = "MySQL"
	DatastorePostgres Datastore = "PostgresSQL"
)

type Telemetry interface {
	Shutdown(ctx context.Context) error
	Tracer() Tracer
}

// SpanOption is used to configure how should the new Span behaves.
type SpanOption func(*SpanOptions)

// SpanOptions holds the configuration to create a new Span.
type SpanOptions struct {
	DatastoreOperation *DatastoreOperation
}

type DatastoreOperation struct {
	Datastore    Datastore
	DatabaseName string
}

type Tracer interface {
	SpanFromContext(ctx context.Context) Span
	Start(ctx context.Context, spanName string, opts ...SpanOptions) (context.Context, Span)
}

type Span interface {
	trace.Span
}

type config struct {
	AppName        string
	AppVersion     string
	Implementation string
	OtelConf       OtelConf
}

// Option is a functional option to be used for creating new Telemetry.
type Option func(*config)

type OtelConf struct {
	OTLPEndpoint string
	Protocol     Protocol
	sdktrace.SpanExporter
}

func WithOTLPExporter(ctx context.Context) Option {
	return func(c *config) {
		switch c.OtelConf.Protocol {
		case HTTP:
			// Change default HTTPS -> HTTP
			insecureOpt := otlptracehttp.WithInsecure()

			// Update default OTLP reciver endpoint
			endpointOpt := otlptracehttp.WithEndpoint(c.OtelConf.OTLPEndpoint)

			exporter, err := otlptracehttp.New(ctx, insecureOpt, endpointOpt)
			if err != nil {
				panic(err)
			}

			c.OtelConf.SpanExporter = exporter
		case GRPC:
			insecureOpt := otlptracegrpc.WithInsecure()

			// Update default OTLP reciver endpoint
			endpointOpt := otlptracegrpc.WithEndpoint(c.OtelConf.OTLPEndpoint)

			exporter, err := otlptracegrpc.New(ctx, insecureOpt, endpointOpt)
			if err != nil {
				panic(err)
			}
			c.OtelConf.SpanExporter = exporter
		default:
			panic("otel conf protocol is required")
		}
	}
}

// New creates new Telemetry implementation.
func New(ctx context.Context, appName string, opts ...Option) Telemetry {
	cfg := config{
		AppName: appName,
	}

	for _, f := range opts {
		f(&cfg)
	}

	switch cfg.Implementation {
	case ObservabilityTelemetry:
		WithOTLPExporter(ctx)
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(cfg.OtelConf.SpanExporter),
			sdktrace.WithResource(resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceName(cfg.AppName),
				semconv.ServiceVersion(cfg.AppVersion),
			)),
		)
		otel.SetTracerProvider(tp)

		otel.SetTextMapPropagator(
			propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{},
				propagation.Baggage{},
			),
		)
		impl := observeOtelImpl{
			traceApp: tp,
		}
		return impl
	default:
		panic("implementation is required")
	}
}
