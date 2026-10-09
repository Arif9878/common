// Package resenv reads the service identity from the standard
// OpenTelemetry environment variables, without the OpenTelemetry SDK, for
// packages that describe the service outside a resource (log fields).
package resenv

import (
	"net/url"
	"os"
	"strings"
)

// Identity is the service name, deployment environment and version.
type Identity struct {
	Service     string
	Environment string
	Version     string
}

// Lookup returns the identity in OTEL_SERVICE_NAME and in the
// service.name, deployment.environment.name (or the older
// deployment.environment) and service.version entries of
// OTEL_RESOURCE_ATTRIBUTES. OTEL_SERVICE_NAME wins over service.name, as in
// the OpenTelemetry SDK. Malformed entries are skipped.
func Lookup() Identity {
	var id Identity
	attrs := parse(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"))
	id.Service = attrs["service.name"]
	if s := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); s != "" {
		id.Service = s
	}
	id.Environment = attrs["deployment.environment.name"]
	if id.Environment == "" {
		id.Environment = attrs["deployment.environment"]
	}
	id.Version = attrs["service.version"]
	return id
}

// parse reads "key=value,key2=value2" with percent-encoded values, the
// format of OTEL_RESOURCE_ATTRIBUTES.
func parse(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		dv, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		out[k] = dv
	}
	return out
}
