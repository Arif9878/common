package otlp_test

import (
	"context"
	"fmt"

	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/logs"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/otlp"
	"github.com/Arif9878/common/go/observability/tracing"
)

// AppConfig is loaded with config.Load; in a container, OTLP_ENDPOINT,
// OTLP_HEADERS, TRACING_EXPORTER and so on choose the backend.
type AppConfig struct {
	OTLP    otlp.Config    `envPrefix:"OTLP_"`
	Log     logging.Config `envPrefix:"LOG_"`
	Logs    logs.Config    `envPrefix:"LOGS_"`
	Tracing tracing.Config `envPrefix:"TRACING_"`
	Metrics metrics.Config `envPrefix:"METRICS_"`
}

func Example() {
	ctx := context.Background()
	cfg := AppConfig{ // for New Relic; see the README for other backends
		OTLP:    otlp.Config{Protocol: "http", Endpoint: "https://otlp.nr-data.net:4318", Headers: "api-key=<license key>"},
		Log:     logging.Config{Output: logging.OutputBoth},
		Logs:    logs.Config{Exporter: logs.ExporterOTLP},
		Tracing: tracing.Config{Exporter: tracing.ExporterOTLP},
		Metrics: metrics.Config{Exporter: metrics.ExporterOTLP, Temporality: "delta"},
	}

	tp, err := tracing.Init(ctx, cfg.Tracing, tracing.WithGlobal(), tracing.WithOTLP(cfg.OTLP))
	if err != nil {
		panic(err)
	}
	mp, err := metrics.Init(ctx, cfg.Metrics, metrics.WithGlobal(), metrics.WithOTLP(cfg.OTLP))
	if err != nil {
		panic(err)
	}
	lp, err := logs.Init(ctx, cfg.Logs, logs.WithOTLP(cfg.OTLP))
	if err != nil {
		panic(err)
	}
	logger, err := logging.New(cfg.Log, logging.WithLoggerProvider(lp.LoggerProvider()))
	if err != nil {
		panic(err)
	}
	_ = logger
	// At shutdown, in graceful.Telemetry: tp.Shutdown, mp.Shutdown, lp.Shutdown.
	_, _, _ = tp, mp, lp
}

func ExampleParseHeaders() {
	h, err := otlp.ParseHeaders("api-key=abc,Authorization=Basic%20dXNlcjpwdw==")
	fmt.Println(h["api-key"], h["Authorization"], err)
	// Output: abc Basic dXNlcjpwdw== <nil>
}
