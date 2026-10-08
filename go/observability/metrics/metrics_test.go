package metrics_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/observability/metrics"
)

func scrape(t *testing.T, p *metrics.Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func initProvider(t *testing.T, cfg metrics.Config, opts ...metrics.Option) *metrics.Provider {
	t.Helper()
	p, err := metrics.Init(context.Background(), cfg, opts...)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func TestExport(t *testing.T) {
	p := initProvider(t, metrics.Config{Service: "orders", Environment: "prod", Version: "1.2.3"})
	meter := p.MeterProvider().Meter("test")

	created, _ := meter.Int64Counter("orders.created")
	created.Add(context.Background(), 2, metric.WithAttributes(attribute.String("outcome", "ok")))
	dur, _ := meter.Float64Histogram("orders.process.duration", metric.WithUnit("s"))
	dur.Record(context.Background(), 0.03)

	out := scrape(t, p)
	for _, want := range []string{
		`outcome="ok"`,
		`orders_created_total{`,
		`orders_process_duration_seconds_bucket{`,
		`le="0.005"`,
		`le="10"`,
		`service_name="orders"`,
		`deployment_environment_name="prod"`,
		`service_version="1.2.3"`,
		"go_goroutines",
		"process_cpu_seconds_total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
	if t.Failed() {
		t.Log(out)
	}
}

func TestInstrumentBoundariesOverrideDefault(t *testing.T) {
	p := initProvider(t, metrics.Config{})
	h, _ := p.MeterProvider().Meter("test").Float64Histogram("custom.duration",
		metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(0.5, 1.5))
	h.Record(context.Background(), 1)

	out := scrape(t, p)
	if !strings.Contains(out, `le="1.5"`) {
		t.Errorf("instrument boundaries not applied:\n%s", out)
	}
	if strings.Contains(out, `le="0.005"`) {
		t.Errorf("default boundaries overrode instrument boundaries")
	}
}

func TestCardinalityLimit(t *testing.T) {
	p := initProvider(t, metrics.Config{CardinalityLimit: 3})
	c, _ := p.MeterProvider().Meter("test").Int64Counter("requests")
	for i := range 10 {
		c.Add(context.Background(), 1, metric.WithAttributes(attribute.String("user_id", fmt.Sprint(i))))
	}

	out := scrape(t, p)
	if !strings.Contains(out, `otel_metric_overflow="true"`) {
		t.Fatalf("no overflow series:\n%s", out)
	}
	if n := strings.Count(out, "requests_total{"); n > 3 {
		t.Fatalf("%d series exported, want at most 3", n)
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := metrics.Init(context.Background(), metrics.Config{CardinalityLimit: -1}); err == nil {
		t.Fatal("expected error for negative limit")
	}
}

func TestWithRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	legacy := prometheus.NewCounter(prometheus.CounterOpts{Name: "legacy_events_total"})
	reg.MustRegister(legacy)
	legacy.Inc()

	p := initProvider(t, metrics.Config{}, metrics.WithRegistry(reg))
	c, _ := p.MeterProvider().Meter("test").Int64Counter("new.events")
	c.Add(context.Background(), 1)

	out := scrape(t, p)
	if !strings.Contains(out, "legacy_events_total 1") || !strings.Contains(out, "new_events_total") {
		t.Fatalf("shared registry missing metrics:\n%s", out)
	}
	if strings.Contains(out, "go_goroutines") {
		t.Fatal("runtime collectors added to caller's registry")
	}
}

func TestConcurrentRecordAndScrape(t *testing.T) {
	p := initProvider(t, metrics.Config{})
	meter := p.MeterProvider().Meter("test")
	c, _ := meter.Int64Counter("ops")
	h, _ := meter.Float64Histogram("ops.duration", metric.WithUnit("s"))

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			attrs := metric.WithAttributes(attribute.Int("worker", i%4))
			for range 200 {
				c.Add(context.Background(), 1, attrs)
				h.Record(context.Background(), 0.01, attrs)
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 10 {
				scrape(t, p)
			}
		})
	}
	wg.Wait()

	out := scrape(t, p)
	total := 0
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, "ops_total{") {
			var n int
			_, _ = fmt.Sscan(line[strings.LastIndexByte(line, ' ')+1:], &n)
			total += n
		}
	}
	if total != 16*200 {
		t.Fatalf("ops_total sum = %d, want %d", total, 16*200)
	}
}

func TestShutdownIdempotent(t *testing.T) {
	p, err := metrics.Init(context.Background(), metrics.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}
