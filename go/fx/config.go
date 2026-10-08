package commonfx

import (
	"context"
	"log/slog"
	"reflect"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/tracing"
)

// Config provides T loaded with config.Load. Loading fails app
// construction with every configuration problem listed.
func Config[T any](opts ...config.Option) fx.Option {
	return fx.Provide(func() (T, error) { return config.Load[T](opts...) })
}

// ConfigFields provides every exported struct-typed field of T as its own
// type, so modules can depend on logging.Config, postgres.Config and so on
// directly. T itself must be provided, for example by [Config]. Two fields
// of the same type are an error (fx reports it at startup); provide such
// configurations yourself instead.
func ConfigFields[T any]() fx.Option {
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		return fx.Error(errors.InvalidArgument.Errorf("commonfx.ConfigFields: %s is not a struct", t))
	}
	var provides []any
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || f.Type.Kind() != reflect.Struct || f.Anonymous {
			continue
		}
		fnType := reflect.FuncOf([]reflect.Type{t}, []reflect.Type{f.Type}, false)
		fn := reflect.MakeFunc(fnType, func(args []reflect.Value) []reflect.Value {
			return []reflect.Value{args[0].Field(i)}
		})
		provides = append(provides, fn.Interface())
	}
	return fx.Provide(provides...)
}

// Observability provides *slog.Logger (logging.New), *tracing.Provider and
// *metrics.Provider (both registered as OpenTelemetry globals, so
// instrumentation libraries use them), and the trace.TracerProvider and
// metric.MeterProvider interfaces. It needs logging.Config, tracing.Config
// and metrics.Config in the graph. fx's own events are logged through the
// same logger at debug level, and the providers are flushed and stopped in
// graceful.Telemetry, after everything else.
func Observability() fx.Option {
	return fx.Options(
		fx.WithLogger(func(l *slog.Logger) fxevent.Logger {
			fl := &fxevent.SlogLogger{Logger: l.With("component", "fx")}
			fl.UseLogLevel(slog.LevelDebug) // provide/invoke events are noise at info; errors stay at error
			return fl
		}),
		fx.Module("commonfx.observability",
			fx.Provide(
				func(cfg logging.Config) (*slog.Logger, error) { return logging.New(cfg) },
				func(cfg tracing.Config) (*tracing.Provider, error) {
					return tracing.Init(context.Background(), cfg, tracing.WithGlobal())
				},
				func(cfg metrics.Config) (*metrics.Provider, error) {
					return metrics.Init(context.Background(), cfg, metrics.WithGlobal())
				},
				func(p *tracing.Provider) trace.TracerProvider { return p.TracerProvider() },
				func(p *metrics.Provider) metric.MeterProvider { return p.MeterProvider() },
			),
			fx.Invoke(func(g *graceful.Manager, tp *tracing.Provider, mp *metrics.Provider) error {
				if err := g.Register(graceful.Telemetry, "tracing", tp.Shutdown); err != nil {
					return err
				}
				return g.Register(graceful.Telemetry, "metrics", mp.Shutdown)
			}),
		),
	)
}
