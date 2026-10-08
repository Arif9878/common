// Package commonfx wires the common platform packages into Uber fx
// (go.uber.org/fx) applications. It is a separate module, so services that
// do not use fx do not depend on it:
//
//	import commonfx "github.com/Arif9878/common/go/fx"
//
//	fx.New(
//		commonfx.Config[AppConfig](config.WithPrefix("ORDERS_")),
//		commonfx.ConfigFields[AppConfig](), // provides logging.Config, postgres.Config, ...
//		commonfx.Observability(),
//		commonfx.Lifecycle(),
//		commonfx.AdminServer(),             // /live /ready /startup /metrics
//		commonfx.HTTPServer(),
//		commonfx.Postgres(),
//		fx.Provide(NewOrderService, NewRouter),
//		commonfx.Ready(),                   // always last
//		fx.StopTimeout(45*time.Second),     // longer than the graceful timeout
//	).Run()
//
// Every module only calls the constructors of the core packages; nothing
// here changes their behavior.
//
// # Startup and shutdown
//
// fx handles signals (App.Run) and starts components. Shutdown order is
// owned by lifecycle/graceful, not by fx: fx stops hooks in reverse
// dependency order, which cannot express "stop intake everywhere, then
// drain, then close dependencies, then flush telemetry" across components
// that do not depend on each other. So:
//
//   - The modules here register every stop function in a graceful phase.
//     Register your own shutdown work the same way (inject
//     *graceful.Manager), not with fx.Lifecycle OnStop.
//   - [Ready] must be the last option. Its OnStart runs after every other
//     OnStart and marks the service ready (health.Checker.MarkStarted); its
//     OnStop runs before every other OnStop and runs the graceful phases.
//     Lifecycle fails app start if Ready is missing.
//   - The graceful timeout (30s by default, see [WithShutdownTimeout]) must
//     fit in fx.StopTimeout (15s by default): set fx.StopTimeout above it.
//     Ready logs a warning if fx's stop deadline is shorter.
//
// Constructors that connect (Postgres, Redis, Vault, Kafka) do so while the
// fx graph is built, bounded by [ConnectTimeout], so a service with an
// unreachable dependency fails at startup.
package commonfx

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"go.uber.org/fx"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/messaging/kafka"
)

// ConnectTimeout bounds connecting to a dependency in a constructor.
const ConnectTimeout = 30 * time.Second

// LifecycleOption configures [Lifecycle].
type LifecycleOption func(*lifecycleOptions)

type lifecycleOptions struct {
	timeout  time.Duration
	graceful []graceful.Option
	health   []health.Option
}

// WithShutdownTimeout sets the total graceful shutdown time (default 30s).
func WithShutdownTimeout(d time.Duration) LifecycleOption {
	return func(o *lifecycleOptions) { o.timeout = d }
}

// WithUnreadyDelay waits d after marking the service unready before
// stopping intake; see graceful.WithUnreadyDelay.
func WithUnreadyDelay(d time.Duration) LifecycleOption {
	return func(o *lifecycleOptions) { o.graceful = append(o.graceful, graceful.WithUnreadyDelay(d)) }
}

// WithHealthOptions configures the health.Checker.
func WithHealthOptions(opts ...health.Option) LifecycleOption {
	return func(o *lifecycleOptions) { o.health = append(o.health, opts...) }
}

// lifecycleState is shared by Lifecycle and Ready. The type is unexported,
// so only this package can request it from the graph.
type lifecycleState struct {
	ready   atomic.Bool // Ready was registered
	timeout time.Duration
}

// LoggerIn takes an optional logger; modules fall back to slog.Default().
type LoggerIn struct {
	fx.In
	Logger *slog.Logger `optional:"true"`
}

func (in LoggerIn) logger() *slog.Logger {
	if in.Logger != nil {
		return in.Logger
	}
	return slog.Default()
}

// Lifecycle provides *graceful.Manager (signal handling left to fx) and
// *health.Checker, registers the readiness drain in graceful.Unready, and
// runs every consumer provided with [KafkaConsumer].
func Lifecycle(opts ...LifecycleOption) fx.Option {
	o := lifecycleOptions{timeout: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	return fx.Module("commonfx.lifecycle",
		fx.Provide(
			func(in LoggerIn) *graceful.Manager {
				return graceful.New(append([]graceful.Option{
					graceful.WithSignals(), // fx handles signals
					graceful.WithLogger(in.logger()),
					graceful.WithTimeout(o.timeout),
				}, o.graceful...)...)
			},
			func(in LoggerIn) *health.Checker {
				return health.New(append([]health.Option{health.WithLogger(in.logger())}, o.health...)...)
			},
			func() *lifecycleState { return &lifecycleState{timeout: o.timeout} },
		),
		fx.Invoke(func(lc fx.Lifecycle, g *graceful.Manager, checks *health.Checker, state *lifecycleState, consumers kafkaConsumers) error {
			if err := g.Register(graceful.Unready, "readiness", checks.Drain); err != nil {
				return err
			}
			lc.Append(fx.Hook{OnStart: func(context.Context) error {
				if !state.ready.Load() {
					return errors.InvalidArgument.New("commonfx: add commonfx.Ready() as the last fx option")
				}
				for _, c := range consumers.Consumers {
					if err := g.Register(graceful.StopIntake, "kafka consumer", c.Close); err != nil {
						return err
					}
					g.Go("kafka consumer", func() error { return c.Run(context.Background()) })
				}
				return nil
			}})
			return nil
		}),
	)
}

type kafkaConsumers struct {
	fx.In
	Consumers []*kafka.Consumer `group:"commonfx.kafka.consumers"`
}

// Ready marks the service ready after every other component started and
// runs the graceful shutdown phases before any other fx stop hook. It must
// be the last option passed to fx.New.
func Ready() fx.Option {
	return fx.Invoke(func(lc fx.Lifecycle, g *graceful.Manager, checks *health.Checker, state *lifecycleState, in LoggerIn) {
		state.ready.Store(true)
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error {
				checks.MarkStarted()
				return nil
			},
			OnStop: func(ctx context.Context) error {
				if d, ok := ctx.Deadline(); ok && time.Until(d) < state.timeout {
					in.logger().Warn("fx stop timeout may be shorter than the graceful shutdown timeout; set fx.StopTimeout",
						"fx_stop_remaining", time.Until(d).Round(time.Second).String())
				}
				return g.Shutdown(ctx)
			},
		})
	})
}
