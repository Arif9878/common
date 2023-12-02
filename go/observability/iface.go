package observability

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	ObservabilityTelemetry = "opentelemetry"
)

type ProtocolType string

const (
	GRPC ProtocolType = "GRPC"
	HTTP ProtocolType = "HTTP"
)

type Telemetry interface {
	Shutdown(ctx context.Context) error
}

type Tracer interface {
}

// Span is a general form of process Span.
type Span interface {
}

type config struct {
	AppName                   string
	AppVersion                string
	Disabled                  bool
	ShouldWaitForConnection   bool
	WaitForConnectionDuration time.Duration
	OtelConf                  OtelConf
}

// Option is a functional option to be used for creating new Telemetry.
type Option func(*config)

type OtelConf struct {
	trace.TracerProvider
}

// Initialize OpenTelemetry once during the application startup
func WithTracerOpenTelemetry() Option {
	return func(c *config) {
		exporter, err := stdouttrace.New()
		if err != nil {
			panic("failed to initialize stdout exporter")
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceName(c.AppName),
				semconv.ServiceVersion(c.AppVersion),
			)),
		)
		otel.SetTracerProvider(tp)

		otel.SetTextMapPropagator(
			propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{},
				propagation.Baggage{},
			),
		)

		c.OtelConf.TracerProvider = tp
	}
}
