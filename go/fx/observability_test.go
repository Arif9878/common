package commonfx_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/Arif9878/common/go/config"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/logs"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/otlp"
	"github.com/Arif9878/common/go/observability/tracing"
)

type otlpAppConfig struct {
	OTLP    otlp.Config    `envPrefix:"OTLP_"`
	Log     logging.Config `envPrefix:"LOG_"`
	Logs    logs.Config    `envPrefix:"LOGS_"`
	Tracing tracing.Config `envPrefix:"TRACING_"`
	Metrics metrics.Config `envPrefix:"METRICS_"`
}

// TestObservabilityToOneOTLPBackend points traces, metrics and logs at one
// OTLP/HTTP backend with environment variables only, as a service would
// for New Relic or Grafana Cloud.
func TestObservabilityToOneOTLPBackend(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{} // path -> api-key header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("api-key")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer srv.Close()

	env := map[string]string{
		"OTLP_PROTOCOL":    "http",
		"OTLP_ENDPOINT":    srv.URL,
		"OTLP_HEADERS":     "api-key=secret-key",
		"TRACING_EXPORTER": "otlp",
		"METRICS_EXPORTER": "otlp",
		"LOGS_EXPORTER":    "otlp",
		"LOG_OUTPUT":       "otlp",
	}
	var tp trace.TracerProvider
	var logger *slog.Logger
	app := fxtest.New(t,
		commonfx.Config[otlpAppConfig](config.WithEnvironment(env)),
		commonfx.ConfigFields[otlpAppConfig](),
		commonfx.Observability(),
		commonfx.Lifecycle(commonfx.WithShutdownTimeout(10*time.Second)),
		fx.Populate(&tp, &logger),
		commonfx.Ready(),
	)
	app.RequireStart()
	_, span := tp.Tracer("test").Start(context.Background(), "work")
	span.End()
	logger.Info("hello", "OTLP_HEADERS", "should be redacted anyway")
	app.RequireStop() // flushes all three in graceful.Telemetry

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		if key, ok := got[path]; !ok || key != "secret-key" {
			t.Errorf("%s: received %v with api-key %q", path, ok, key)
		}
	}
	for path := range got {
		if !strings.HasPrefix(path, "/v1/") {
			t.Errorf("unexpected export path %s", path)
		}
	}
}
