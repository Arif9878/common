package dashboards_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/observability/metrics"
)

var familyRE = regexp.MustCompile(`(?m)^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (\w+)`)

// promNames creates every instrument on a Prometheus metrics provider,
// records a value, scrapes it and returns the exposed series names
// (histograms as _bucket, _sum and _count).
func promNames(t *testing.T) map[string]bool {
	t.Helper()
	ctx := context.Background()
	p, err := metrics.Init(ctx, metrics.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(ctx) })
	m := p.MeterProvider().Meter("inventory")
	for _, in := range inventory(t) {
		u := metric.WithUnit(in.Unit)
		switch in.Kind {
		case "Int64Counter":
			c, _ := m.Int64Counter(in.Name, u)
			c.Add(ctx, 1)
		case "Float64Counter":
			c, _ := m.Float64Counter(in.Name, u)
			c.Add(ctx, 1)
		case "Int64UpDownCounter":
			c, _ := m.Int64UpDownCounter(in.Name, u)
			c.Add(ctx, 1)
		case "Float64UpDownCounter":
			c, _ := m.Float64UpDownCounter(in.Name, u)
			c.Add(ctx, 1)
		case "Int64Histogram":
			h, _ := m.Int64Histogram(in.Name, u)
			h.Record(ctx, 1)
		case "Float64Histogram":
			h, _ := m.Float64Histogram(in.Name, u)
			h.Record(ctx, 1)
		case "Int64Gauge":
			g, _ := m.Int64Gauge(in.Name, u)
			g.Record(ctx, 1)
		case "Float64Gauge":
			g, _ := m.Float64Gauge(in.Name, u)
			g.Record(ctx, 1)
		case "Int64ObservableCounter":
			_, _ = m.Int64ObservableCounter(in.Name, u, metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(1); return nil }))
		case "Float64ObservableCounter":
			_, _ = m.Float64ObservableCounter(in.Name, u, metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error { o.Observe(1); return nil }))
		case "Int64ObservableUpDownCounter":
			_, _ = m.Int64ObservableUpDownCounter(in.Name, u, metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(1); return nil }))
		case "Float64ObservableUpDownCounter":
			_, _ = m.Float64ObservableUpDownCounter(in.Name, u, metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error { o.Observe(1); return nil }))
		case "Int64ObservableGauge":
			_, _ = m.Int64ObservableGauge(in.Name, u, metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(1); return nil }))
		case "Float64ObservableGauge":
			_, _ = m.Float64ObservableGauge(in.Name, u, metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error { o.Observe(1); return nil }))
		default:
			t.Fatalf("unhandled instrument kind %s (%s)", in.Kind, in.Name)
		}
	}
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	names := map[string]bool{}
	for _, m := range familyRE.FindAllStringSubmatch(string(body), -1) {
		name, typ := m[1], m[2]
		if typ == "histogram" {
			for _, s := range []string{"_bucket", "_sum", "_count"} {
				names[name+s] = true
			}
			continue
		}
		names[name] = true
	}
	return names
}
