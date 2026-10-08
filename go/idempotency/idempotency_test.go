package idempotency_test

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/idempotency/idempotencytest"
)

func TestMemoryStore(t *testing.T) {
	idempotencytest.Run(t, idempotency.NewMemoryStore(), time.Sleep, "")
}

func TestInProgressIsRetryable(t *testing.T) {
	s := idempotency.NewMemoryStore()
	if _, _, _, err := s.Begin(context.Background(), "k", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err := idempotency.Do(context.Background(), s, "k", func(context.Context) (int, error) { return 1, nil })
	if !errors.Is(err, idempotency.ErrInProgress) || !errors.IsRetryable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := idempotency.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	s := idempotency.NewMemoryStore()
	fn := func(context.Context) (int, error) { return 1, nil }
	_, _ = idempotency.Do(context.Background(), s, "k", fn, mp, idempotency.WithName("charge"))
	_, _ = idempotency.Do(context.Background(), s, "k", fn, mp, idempotency.WithName("charge"))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, dp := range rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints {
		o, _ := dp.Attributes.Value("outcome")
		got[o.AsString()] = dp.Value
	}
	if got["executed"] != 1 || got["duplicate"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}
