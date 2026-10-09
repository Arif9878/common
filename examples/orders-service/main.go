// Command orders-service is the golden-path example of a service built on
// the common library: an Echo API that writes orders to PostgreSQL and
// publishes "order created" events through the transactional outbox, and a
// Kafka consumer that handles each event once, with Redis idempotency.
// Logs, traces and metrics go wherever the environment says (see
// ENVIRONMENT.md and the README).
package main

import (
	"time"

	"go.uber.org/fx"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/redis"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/outbox"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/logs"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/otlp"
	"github.com/Arif9878/common/go/observability/tracing"
	"github.com/Arif9878/common/go/transport/http/httpserver"

	"github.com/Arif9878/common/examples/orders-service/internal/orders"
)

// Config is read from the environment; every variable is listed in the
// common library's ENVIRONMENT.md, under these prefixes.
type Config struct {
	OTLP     otlp.Config          `envPrefix:"OTLP_"`
	Log      logging.Config       `envPrefix:"LOG_"`
	Logs     logs.Config          `envPrefix:"LOGS_"`
	Tracing  tracing.Config       `envPrefix:"TRACING_"`
	Metrics  metrics.Config       `envPrefix:"METRICS_"`
	HTTP     httpserver.Config    `envPrefix:"HTTP_"`
	Admin    commonfx.AdminConfig `envPrefix:"ADMIN_"`
	Postgres postgres.Config      `envPrefix:"POSTGRES_"`
	Redis    redis.Config         `envPrefix:"REDIS_"`
	Kafka    kafka.Config         `envPrefix:"KAFKA_"`
}

func main() { app().Run() }

// options are the application's fx options, shared with the test.
func options() []fx.Option {
	return []fx.Option{
		commonfx.ConfigFields[Config](),
		commonfx.Observability(),
		commonfx.Lifecycle(commonfx.WithShutdownTimeout(30 * time.Second)),
		commonfx.AdminServer(), // /live, /ready, /startup and /metrics on ADMIN_ADDR
		commonfx.EchoServer(),  // the API on HTTP_ADDR
		commonfx.Postgres(),
		commonfx.Redis(),
		commonfx.RedisIdempotency(),
		commonfx.KafkaIdempotency(kafka.Idempotency{Key: kafka.HeaderKey(outbox.EventIDHeader)}),
		commonfx.KafkaProducer(),
		commonfx.Outbox(),
		commonfx.OutboxRelay(),
		orders.Module,
	}
}

func app() *fx.App {
	return fx.New(append([]fx.Option{commonfx.Config[Config]()}, append(options(),
		commonfx.Ready(), // always last
		fx.StopTimeout(45*time.Second),
	)...)...)
}
