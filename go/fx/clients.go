package commonfx

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/fx"
	"google.golang.org/grpc"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/transport/grpc/grpcclient"
	"github.com/Arif9878/common/go/transport/http/httpclient"
)

func nameTag(name string) string { return `name:"` + name + `"` }

const kafkaConsumersGroup = `group:"commonfx.kafka.consumers"`

// GRPCClientConfig configures a gRPC client connection. Environment variable
// names are relative; for example INVENTORY_GRPC_TARGET with prefix
// INVENTORY_GRPC_.
type GRPCClientConfig struct {
	// Target is a gRPC target such as "dns:///inventory.internal:9091".
	Target string `env:"TARGET,required"`
	// Insecure disables TLS (plaintext inside a trusted network).
	Insecure bool `env:"INSECURE"`
	// Timeout is the default deadline for calls without one.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10s"`
}

// GRPCClient provides a *grpc.ClientConn named name, built with grpcclient
// and the graph's telemetry, from the GRPCClientConfig named name:
//
//	commonfx.GRPCClient("inventory", grpcclient.WithRetry(grpcclient.RetryPolicy{})),
//	fx.Provide(fx.Annotate(func(c AppConfig) commonfx.GRPCClientConfig { return c.Inventory },
//		fx.ResultTags(`name:"inventory"`))),
//	fx.Provide(fx.Annotate(NewOrderService, fx.ParamTags(`name:"inventory"`))),
//
// The connection is lazy (no I/O at startup) and closed in
// graceful.CloseDeps.
func GRPCClient(name string, opts ...grpcclient.Option) fx.Option {
	return fx.Provide(fx.Annotate(
		func(cfg GRPCClientConfig, t *telemetry, g *graceful.Manager) (*grpc.ClientConn, error) {
			if cfg.Target == "" {
				return nil, errors.InvalidArgument.New("commonfx: GRPCClientConfig " + name + " has no Target")
			}
			base := t.grpcClient()
			if cfg.Insecure {
				base = append(base, grpcclient.WithInsecure())
			}
			if cfg.Timeout > 0 {
				base = append(base, grpcclient.WithTimeout(cfg.Timeout))
			}
			conn, err := grpcclient.New(cfg.Target, append(base, opts...)...)
			if err != nil {
				return nil, err
			}
			return conn, g.Register(graceful.CloseDeps, "grpc client "+name, func(context.Context) error {
				return conn.Close()
			})
		},
		fx.ParamTags(nameTag(name)),
		fx.ResultTags(nameTag(name)),
	))
}

// HTTPClient provides an *http.Client named name, built with httpclient and
// the graph's telemetry, from the httpclient.Config named name if the graph
// has one (defaults otherwise). Idle connections are closed in
// graceful.CloseDeps.
//
//	commonfx.HTTPClient("payments", httpclient.WithRetry(), httpclient.WithName("payments")),
//	fx.Provide(fx.Annotate(NewPaymentsGateway, fx.ParamTags(`name:"payments"`))),
func HTTPClient(name string, opts ...httpclient.Option) fx.Option {
	return fx.Provide(fx.Annotate(
		func(cfg httpclient.Config, t *telemetry, g *graceful.Manager) (*http.Client, error) {
			c := httpclient.New(cfg, append(append(t.httpClient(), httpclient.WithName(name)), opts...)...)
			return c, g.Register(graceful.CloseDeps, "http client "+name, func(context.Context) error {
				c.CloseIdleConnections()
				return nil
			})
		},
		fx.ParamTags(nameTag(name)+` optional:"true"`),
		fx.ResultTags(nameTag(name)),
	))
}

// KafkaConsumer builds a kafka consumer for group and topics with the
// graph's kafka.Config, telemetry and the options contributed to
// [KafkaOptions], plus opts. handler is a constructor returning a
// kafka.Handler, so it can depend on the rest of the graph:
//
//	commonfx.KafkaConsumer("billing", []string{"orders.created"}, NewBillingHandler,
//		kafka.WithConcurrency(16)),
//
// [Lifecycle] runs the consumer on app start and closes it in
// graceful.StopIntake.
func KafkaConsumer(group string, topics []string, handler any, opts ...kafka.Option) fx.Option {
	tag := nameTag("commonfx.kafka.handler." + group)
	return fx.Options(
		fx.Provide(fx.Annotate(handler, fx.ResultTags(tag))),
		fx.Provide(fx.Annotate(
			func(h kafka.Handler, cfg kafka.Config, shared []kafka.Option, t *telemetry) (*kafka.Consumer, error) {
				ctx, cancel := connectCtx()
				defer cancel()
				all := append(append(t.kafka(), shared...), opts...)
				return kafka.NewConsumer(ctx, cfg, group, topics, h, all...)
			},
			fx.ParamTags(tag, ``, KafkaOptions),
			fx.ResultTags(kafkaConsumersGroup),
		)),
	)
}

// KafkaConsumerConstructor registers a constructor returning
// (*kafka.Consumer, error) that you build entirely yourself; [Lifecycle]
// runs and closes it like those from [KafkaConsumer]. Prefer KafkaConsumer,
// which also passes the graph's telemetry.
func KafkaConsumerConstructor(constructor any) fx.Option {
	return fx.Provide(fx.Annotate(constructor, fx.ResultTags(kafkaConsumersGroup)))
}
