package metrics_test

import (
	"context"
	"log"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/observability/metrics"
)

func Example() {
	ctx := context.Background()
	mp, err := metrics.Init(ctx, metrics.Config{Service: "orders"}, metrics.WithGlobal())
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = mp.Shutdown(ctx) }()

	// Serve on a separate port from the public API.
	mux := http.NewServeMux()
	mux.Handle("/metrics", mp.Handler())

	// In a package: record through the OpenTelemetry API only.
	meter := mp.MeterProvider().Meter("github.com/acme/orders/checkout")
	duration, _ := meter.Float64Histogram("checkout.duration", metric.WithUnit("s"))
	duration.Record(ctx, 0.042, metric.WithAttributes(attribute.String("outcome", "ok")))
}
