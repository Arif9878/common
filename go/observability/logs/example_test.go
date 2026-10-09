package logs_test

import (
	"context"
	"log"

	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/logs"
	"github.com/Arif9878/common/go/observability/otlp"
)

func Example() {
	ctx := context.Background()
	shutdown := graceful.New()

	lp, err := logs.Init(ctx, logs.Config{Exporter: logs.ExporterOTLP, Service: "orders"},
		logs.WithOTLP(otlp.Config{Endpoint: "otel-collector:4317", Insecure: true}))
	if err != nil {
		log.Fatal(err)
	}
	_ = shutdown.Register(graceful.Telemetry, "logs", lp.Shutdown)

	// Output "both" writes JSON to stdout and exports every record.
	logger, err := logging.New(logging.Config{Output: logging.OutputBoth}, logging.WithLoggerProvider(lp.LoggerProvider()))
	if err != nil {
		log.Fatal(err)
	}
	logger.InfoContext(ctx, "order created", "order_id", "o-1")
}
