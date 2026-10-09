package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/Arif9878/common/go/observability/internal/otlptest"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/otlp"
)

// pushed returns the metrics received, by name.
func pushed(rc *otlptest.Receiver) map[string]*metricspb.Metric {
	out := map[string]*metricspb.Metric{}
	for _, r := range rc.Requests("/v1/metrics") {
		for _, rm := range r.Metrics.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					out[m.GetName()] = m
				}
			}
		}
	}
	return out
}

func scrapeStatus(t *testing.T, p *metrics.Provider) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Code, rec.Body.String()
}

func TestPushesOverOTLPWithDeltaTemporality(t *testing.T) {
	rc := otlptest.New(t)
	ctx := context.Background()
	p, err := metrics.Init(ctx, metrics.Config{Exporter: "otlp", Temporality: "delta"},
		metrics.WithOTLP(otlp.Config{Protocol: "http", Endpoint: rc.URL, Headers: "api-key=k"}))
	if err != nil {
		t.Fatal(err)
	}
	meter := p.MeterProvider().Meter("test")
	c, _ := meter.Int64Counter("orders.created")
	c.Add(ctx, 3)
	ud, _ := meter.Int64UpDownCounter("orders.open")
	ud.Add(ctx, 2)
	if err := p.Shutdown(ctx); err != nil { // pushes the last measurements
		t.Fatal(err)
	}

	got := pushed(rc)
	sum := got["orders.created"].GetSum()
	if sum == nil || sum.GetDataPoints()[0].GetAsInt() != 3 ||
		sum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
		t.Errorf("orders.created = %v, want a delta sum of 3", got["orders.created"])
	}
	if ud := got["orders.open"].GetSum(); ud.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Errorf("up-down counter temporality = %v, want cumulative", ud.GetAggregationTemporality())
	}
	if _, ok := got["go.memory.used"]; !ok {
		t.Error("no Go runtime metrics pushed without Prometheus")
	}
	if rc.Requests("/v1/metrics")[0].Header.Get("api-key") != "k" {
		t.Error("api-key header missing")
	}
	if code, _ := scrapeStatus(t, p); code != http.StatusNotFound {
		t.Errorf("scrape without the prometheus exporter = %d, want 404", code)
	}
}

func TestPrometheusAndOTLPTogether(t *testing.T) {
	rc := otlptest.New(t)
	ctx := context.Background()
	p, err := metrics.Init(ctx, metrics.Config{Exporter: "prometheus, otlp-http", Endpoint: rc.URL})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := p.MeterProvider().Meter("test").Int64Counter("orders.created")
	c.Add(ctx, 1)
	if code, body := scrapeStatus(t, p); code != http.StatusOK || !strings.Contains(body, "orders_created_total{") {
		t.Errorf("scrape = %d:\n%s", code, body)
	}
	_ = p.Shutdown(ctx)
	sum := pushed(rc)["orders.created"].GetSum()
	if sum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Errorf("default temporality = %v, want cumulative", sum.GetAggregationTemporality())
	}
}

func TestExporterValidation(t *testing.T) {
	for _, cfg := range []metrics.Config{
		{Exporter: "statsd"}, {Exporter: "otlp,otlp-http"}, {Temporality: "monthly"}, {Interval: -1},
	} {
		if _, err := metrics.Init(context.Background(), cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
	p, err := metrics.Init(context.Background(), metrics.Config{Exporter: "none"})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Shutdown(context.Background())
}
