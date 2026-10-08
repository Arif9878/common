package testkit_test

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/testkit"
)

func TestLogs(t *testing.T) {
	logger, logs := testkit.NewLogger(t)
	ctx := requestid.NewContext(context.Background(), "req-1")
	logger.InfoContext(ctx, "order created", "order_id", "o-1", "password", "hunter2")
	logger.Debug("cache miss")
	logger.Warn("order created", slog.Int("attempt", 2))

	if got := len(logs.Records()); got != 3 {
		t.Fatalf("records = %d", got)
	}
	created := logs.Messages("order created")
	if len(created) != 2 || created[0]["order_id"] != "o-1" || created[0]["request_id"] != "req-1" {
		t.Errorf("records = %v", created)
	}
	// Production redaction applies.
	if logs.Contains("hunter2") || !logs.Contains("[REDACTED]") {
		t.Errorf("redaction not applied:\n%s", logs)
	}
}

func TestMetrics(t *testing.T) {
	mp, m := testkit.NewMetrics(t)
	meter := mp.Meter("test")
	calls, _ := meter.Int64Counter("calls")
	calls.Add(context.Background(), 2, metric.WithAttributes(attribute.String("outcome", "ok"), attribute.String("op", "a")))
	calls.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", "error"), attribute.String("op", "a")))
	ratio, _ := meter.Float64Counter("ratio")
	ratio.Add(context.Background(), 0.5)
	depth, _ := meter.Int64Gauge("depth")
	depth.Record(context.Background(), 7, metric.WithAttributes(attribute.String("pool", "p")))
	dur, _ := meter.Float64Histogram("duration")
	dur.Record(context.Background(), 0.1, metric.WithAttributes(attribute.String("op", "a")))
	dur.Record(context.Background(), 0.2, metric.WithAttributes(attribute.String("op", "b")))

	tests := []struct {
		name string
		got  float64
		want float64
	}{
		{"sum all", m.Sum("calls"), 3},
		{"sum filtered", m.Sum("calls", attribute.String("outcome", "ok")), 2},
		{"sum no match", m.Sum("calls", attribute.String("outcome", "timeout")), 0},
		{"sum float", m.Sum("ratio"), 0.5},
		{"gauge", m.Gauge("depth", attribute.String("pool", "p")), 7},
		{"histogram all", float64(m.HistogramCount("duration")), 2},
		{"histogram filtered", float64(m.HistogramCount("duration", attribute.String("op", "b"))), 1},
		{"histogram sum", m.HistogramSum("duration"), 0.30000000000000004},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if !m.Has("calls") || m.Has("missing") {
		t.Error("Has")
	}
}

func TestSpans(t *testing.T) {
	tp, spans := testkit.NewTracer(t)
	_, a := tp.Tracer("test").Start(context.Background(), "a")
	a.End()
	_, b := tp.Tracer("test").Start(context.Background(), "b")
	b.End()
	if len(spans.Ended()) != 2 || len(spans.Named("b")) != 1 {
		t.Errorf("spans = %v", spans.Ended())
	}
}

func TestEventually(t *testing.T) {
	var n atomic.Int32
	go func() {
		for range 3 {
			time.Sleep(5 * time.Millisecond)
			n.Add(1)
		}
	}()
	testkit.Eventually(t, time.Second, "three increments", func() bool { return n.Load() == 3 })
}

func TestFreeAddr(t *testing.T) {
	addr := testkit.FreeAddr(t)
	if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":0") {
		t.Errorf("addr = %q", addr)
	}
}

func TestGetenv(t *testing.T) {
	t.Setenv("TESTKIT_PRESENT", "x")
	if testkit.Getenv(t, "TESTKIT_PRESENT") != "x" {
		t.Error("Getenv")
	}
	t.Run("unset skips", func(t *testing.T) {
		testkit.Getenv(t, "TESTKIT_DEFINITELY_UNSET")
		t.Error("not skipped")
	})
}
