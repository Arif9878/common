package otlpconf_test

import (
	"testing"
	"time"

	"github.com/Arif9878/common/go/observability/internal/otlpconf"
	"github.com/Arif9878/common/go/observability/otlp"
)

func TestResolve(t *testing.T) {
	shared := otlp.Config{Protocol: "http", Endpoint: "https://otlp.example:4318", Headers: "api-key=k",
		Compression: "GZIP", Timeout: time.Second}

	s, err := otlpconf.Resolve("TRACES", "otlp", shared, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Protocol != otlp.ProtocolHTTP || s.Endpoint != "https://otlp.example:4318/v1/traces" || !s.EndpointURL ||
		s.Headers["api-key"] != "k" || s.Compression != "gzip" || s.Timeout != time.Second {
		t.Errorf("settings = %+v", s)
	}

	// The signal's exporter and endpoint win over the shared settings.
	s, err = otlpconf.Resolve("TRACES", "otlp-grpc", shared, "tempo:4317", true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Protocol != otlp.ProtocolGRPC || s.Endpoint != "tempo:4317" || s.EndpointURL || !s.Insecure {
		t.Errorf("overridden settings = %+v", s)
	}

	// Without a protocol in the config, the signal's OTEL variable decides.
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")
	if s, _ := otlpconf.Resolve("METRICS", "otlp", otlp.Config{}, "", false); s.Protocol != otlp.ProtocolHTTP {
		t.Errorf("protocol from env = %q", s.Protocol)
	}

	for _, tc := range []struct{ shared, own, want string }{
		{"https://gw.example/otlp", "", "https://gw.example/otlp/v1/logs"},         // Grafana Cloud base path
		{"https://gw.example/otlp/v1/logs", "", "https://gw.example/otlp/v1/logs"}, // already the signal URL
		{"", "https://col.example/custom", "https://col.example/custom"},           // a signal URL is used as is
		{"", "http://col.example:4318", "http://col.example:4318/v1/logs"},         // unless it has no path
	} {
		s, err := otlpconf.Resolve("LOGS", "otlp-http", otlp.Config{Endpoint: tc.shared}, tc.own, false)
		if err != nil || s.Endpoint != tc.want {
			t.Errorf("shared %q own %q: endpoint %q, %v; want %q", tc.shared, tc.own, s.Endpoint, err, tc.want)
		}
	}

	if _, err := otlpconf.Resolve("LOGS", "otlp", otlp.Config{Headers: "bad"}, "", false); err == nil {
		t.Error("invalid shared config accepted")
	}
}
