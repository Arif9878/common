package testkit_test

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/testkit"
)

// chargeCard is the code under test: it logs and counts.
func chargeCard(ctx context.Context, log *slog.Logger, charges metric.Int64Counter) {
	log.InfoContext(ctx, "card charged", "amount", 42)
	charges.Add(ctx, 1)
}

func Example() {
	// In a _test.go file:
	testCharge := func(t *testing.T) {
		logger, logs := testkit.NewLogger(t)
		mp, metrics := testkit.NewMetrics(t)
		charges, _ := mp.Meter("payments").Int64Counter("payments.charges")

		chargeCard(context.Background(), logger, charges)

		if got := logs.Messages("card charged"); len(got) != 1 || got[0]["amount"] != float64(42) {
			t.Errorf("logs = %v", got)
		}
		if got := metrics.Sum("payments.charges"); got != 1 {
			t.Errorf("payments.charges = %v, want 1", got)
		}
	}
	_ = testCharge
}
