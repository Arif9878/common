// Package otlpconf resolves the OTLP settings of one signal from the shared
// otlp.Config and the signal's own overrides.
package otlpconf

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Arif9878/common/go/observability/otlp"
)

// Settings are the resolved settings of one signal's exporter. Empty
// fields are left to the exporter (and so to OTEL_EXPORTER_OTLP_*).
type Settings struct {
	Protocol    string // otlp.ProtocolGRPC or otlp.ProtocolHTTP
	Endpoint    string
	EndpointURL bool // Endpoint is a URL rather than host:port
	Insecure    bool
	Headers     map[string]string
	Compression string // "", "gzip" or "none"
	Timeout     time.Duration
}

// Resolve returns the settings for a signal ("TRACES", "METRICS" or "LOGS")
// whose exporter is "otlp" (protocol from shared, then the environment),
// "otlp-grpc" or "otlp-http". endpoint and insecure are the signal's own
// overrides.
func Resolve(signal, exporter string, shared otlp.Config, endpoint string, insecure bool) (Settings, error) {
	if err := shared.Validate(); err != nil {
		return Settings{}, err
	}
	var s Settings
	switch strings.ToLower(exporter) {
	case "otlp-grpc":
		s.Protocol = otlp.ProtocolGRPC
	case "otlp-http":
		s.Protocol = otlp.ProtocolHTTP
	case "otlp":
		env := os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL")
		if env == "" {
			env = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
		}
		p, err := shared.ResolvedProtocol(env)
		if err != nil {
			return Settings{}, err
		}
		s.Protocol = p
	default:
		return Settings{}, fmt.Errorf("unknown OTLP exporter %q", exporter)
	}
	// A shared URL is a base, as OTEL_EXPORTER_OTLP_ENDPOINT: the signal path
	// is appended. A signal's own URL is used as is, like
	// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, unless it has no path at all.
	// gRPC has no paths.
	s.Endpoint = shared.Endpoint
	base := true
	if endpoint != "" {
		s.Endpoint, base = endpoint, false
	}
	s.EndpointURL = strings.Contains(s.Endpoint, "://")
	if s.EndpointURL && s.Protocol == otlp.ProtocolHTTP {
		e, err := signalURL(s.Endpoint, "/v1/"+strings.ToLower(signal), base)
		if err != nil {
			return Settings{}, err
		}
		s.Endpoint = e
	}
	s.Insecure = shared.Insecure || insecure
	s.Headers, _ = otlp.ParseHeaders(shared.Headers.Reveal()) // validated above
	s.Compression = strings.ToLower(shared.Compression)
	s.Timeout = shared.Timeout
	return s, nil
}

func signalURL(raw, path string, base bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid OTLP endpoint URL: %w", err)
	}
	p := strings.TrimSuffix(u.Path, "/")
	switch {
	case strings.HasSuffix(p, path):
	case base || p == "":
		u.Path = p + path
	}
	return u.String(), nil
}
