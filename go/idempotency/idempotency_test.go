package idempotency_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/testkit"

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
	mp, m := testkit.NewMetrics(t)
	s := idempotency.NewMemoryStore()
	fn := func(context.Context) (int, error) { return 1, nil }
	opts := []idempotency.Option{idempotency.WithMeterProvider(mp), idempotency.WithName("charge")}
	_, _ = idempotency.Do(context.Background(), s, "k", fn, opts...)
	_, _ = idempotency.Do(context.Background(), s, "k", fn, opts...)

	for _, o := range []string{"executed", "duplicate"} {
		if got := m.Sum("idempotency.calls", attribute.String("operation", "charge"), attribute.String("outcome", o)); got != 1 {
			t.Errorf("idempotency.calls{outcome=%s} = %v, want 1", o, got)
		}
	}
}
