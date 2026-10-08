package commonfx

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/secret/vault"
	"github.com/Arif9878/common/go/transport/grpc/grpcclient"
	"github.com/Arif9878/common/go/transport/grpc/grpcserver"
	"github.com/Arif9878/common/go/transport/http/httpclient"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

// telemetryIn collects whatever telemetry the graph has. [Observability]
// provides all of it; without it, modules fall back to each package's
// defaults (slog.Default and the OpenTelemetry globals).
type telemetryIn struct {
	fx.In
	Logger         *slog.Logger                  `optional:"true"`
	TracerProvider trace.TracerProvider          `optional:"true"`
	MeterProvider  metric.MeterProvider          `optional:"true"`
	Propagator     propagation.TextMapPropagator `optional:"true"`
}

// telemetry is passed explicitly to every module's constructor, so wiring
// never depends on OpenTelemetry global state or on option order.
type telemetry struct {
	logger *slog.Logger
	tp     trace.TracerProvider
	mp     metric.MeterProvider
	prop   propagation.TextMapPropagator
}

func newTelemetry(in telemetryIn) *telemetry {
	l := in.Logger
	if l == nil {
		l = slog.Default()
	}
	return &telemetry{logger: l, tp: in.TracerProvider, mp: in.MeterProvider, prop: in.Propagator}
}

// optionsFor builds a package's telemetry options, skipping what the graph
// does not have so the package keeps its default for it.
func optionsFor[O any](t *telemetry, logger func(*slog.Logger) O, tracer func(trace.TracerProvider) O,
	meter func(metric.MeterProvider) O, prop func(propagation.TextMapPropagator) O,
) []O {
	var opts []O
	if logger != nil {
		opts = append(opts, logger(t.logger))
	}
	if tracer != nil && t.tp != nil {
		opts = append(opts, tracer(t.tp))
	}
	if meter != nil && t.mp != nil {
		opts = append(opts, meter(t.mp))
	}
	if prop != nil && t.prop != nil {
		opts = append(opts, prop(t.prop))
	}
	return opts
}

func (t *telemetry) httpServer() []httpserver.Option {
	return optionsFor(t, httpserver.WithLogger, httpserver.WithTracerProvider, httpserver.WithMeterProvider, httpserver.WithPropagators)
}

func (t *telemetry) httpClient() []httpclient.Option {
	return optionsFor(t, httpclient.WithLogger, httpclient.WithTracerProvider, httpclient.WithMeterProvider, httpclient.WithPropagators)
}

func (t *telemetry) grpcServer() []grpcserver.Option {
	return optionsFor(t, grpcserver.WithLogger, grpcserver.WithTracerProvider, grpcserver.WithMeterProvider, grpcserver.WithPropagators)
}

func (t *telemetry) grpcClient() []grpcclient.Option {
	return optionsFor(t, nil, grpcclient.WithTracerProvider, grpcclient.WithMeterProvider, grpcclient.WithPropagators)
}

func (t *telemetry) postgres() []postgres.Option {
	return optionsFor(t, postgres.WithLogger, postgres.WithTracerProvider, postgres.WithMeterProvider, nil)
}

func (t *telemetry) redis() []redis.Option {
	return optionsFor(t, redis.WithLogger, redis.WithTracerProvider, redis.WithMeterProvider, nil)
}

func (t *telemetry) vault() []vault.Option {
	return optionsFor(t, vault.WithLogger, vault.WithTracerProvider, vault.WithMeterProvider, nil)
}

func (t *telemetry) kafka() []kafka.Option {
	return optionsFor(t, kafka.WithLogger, kafka.WithTracerProvider, kafka.WithMeterProvider, kafka.WithPropagators)
}

func (t *telemetry) health() []health.Option {
	return optionsFor(t, health.WithLogger, nil, health.WithMeterProvider, nil)
}
