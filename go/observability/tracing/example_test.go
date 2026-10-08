package tracing_test

import (
	"context"
	"log"

	"go.opentelemetry.io/otel"

	"github.com/Arif9878/common/go/observability/tracing"
)

func Example() {
	ctx := context.Background()
	tp, err := tracing.Init(ctx, tracing.Config{
		Exporter: tracing.ExporterOTLPGRPC,
		Endpoint: "otel-collector:4317",
		Insecure: true,
		Service:  "orders",
	}, tracing.WithGlobal())
	if err != nil {
		log.Fatal(err)
	}
	// Shut down last, so spans from the rest of shutdown are exported.
	defer func() { _ = tp.Shutdown(context.WithoutCancel(ctx)) }()

	_ = loadOrder(ctx, "o-1")
}

func loadOrder(ctx context.Context, id string) (err error) {
	ctx, span := otel.Tracer("github.com/acme/orders/store").Start(ctx, "Store.LoadOrder")
	defer tracing.End(span, &err)
	_ = ctx
	_ = id
	return nil
}
