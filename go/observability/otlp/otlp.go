// Package otlp holds the OTLP connection settings shared by traces, metrics
// and logs, so a service points all three at one backend with one set of
// variables:
//
//	type AppConfig struct {
//		OTLP    otlp.Config    `envPrefix:"OTLP_"`
//		Tracing tracing.Config `envPrefix:"TRACING_"`
//		Metrics metrics.Config `envPrefix:"METRICS_"`
//		Logs    logs.Config    `envPrefix:"LOGS_"`
//	}
//
//	tp, err := tracing.Init(ctx, cfg.Tracing, tracing.WithOTLP(cfg.OTLP))
//	mp, err := metrics.Init(ctx, cfg.Metrics, metrics.WithOTLP(cfg.OTLP))
//	lp, err := logs.Init(ctx, cfg.Logs, logs.WithOTLP(cfg.OTLP))
//
// Each signal still chooses whether to export over OTLP (its own Exporter
// setting), and its own Endpoint and Insecure settings, when set, override
// the shared ones, for example to send traces to a different collector.
//
// Any backend that accepts OTLP works: an OpenTelemetry Collector or
// Grafana Alloy, Grafana Cloud, New Relic, Honeycomb, Elastic, the Datadog
// Agent, Jaeger (traces). The repository README has a configuration for
// each.
//
// Settings left empty fall back to the standard OpenTelemetry variables,
// which the exporters read themselves (OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_EXPORTER_OTLP_HEADERS, OTEL_EXPORTER_OTLP_PROTOCOL, …), then to the
// exporters' defaults (localhost:4317 for gRPC, localhost:4318 for HTTP).
package otlp

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Arif9878/common/go/config"
)

// Protocols accepted in [Config].
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http"
)

// Config configures the OTLP connection. Environment variable names are
// relative; the service chooses the prefix, for example OTLP_.
type Config struct {
	// Protocol is "grpc" or "http" (OTLP/HTTP with protobuf). Empty uses
	// OTEL_EXPORTER_OTLP_PROTOCOL if set ("grpc" or "http/protobuf"), else
	// gRPC.
	Protocol string `env:"PROTOCOL"`
	// Endpoint is the backend address: host:port, or a URL such as
	// https://otlp.nr-data.net:4318. With HTTP, the exporters append the
	// signal path (/v1/traces, /v1/metrics, /v1/logs) to a URL without one.
	Endpoint string `env:"ENDPOINT"`
	// Headers are sent with every export, as comma-separated key=value
	// pairs with URL-encoded values, like OTEL_EXPORTER_OTLP_HEADERS:
	// "api-key=…" for New Relic, "Authorization=Basic%20…" for Grafana
	// Cloud. It is a secret: it never appears in logs.
	Headers config.Secret `env:"HEADERS"`
	// Insecure disables TLS, for a collector on the same host or network.
	Insecure bool `env:"INSECURE"`
	// Compression is "gzip" or "none". Empty uses the exporters' default.
	Compression string `env:"COMPRESSION"`
	// Timeout bounds each export. Zero uses the exporters' default (10s).
	Timeout time.Duration `env:"TIMEOUT"`
}

// Validate reports whether cfg is valid.
func (cfg Config) Validate() error {
	if _, err := cfg.ResolvedProtocol(""); err != nil {
		return err
	}
	switch strings.ToLower(cfg.Compression) {
	case "", "gzip", "none":
	default:
		return errors.New("otlp: invalid compression (want gzip or none)")
	}
	if cfg.Timeout < 0 {
		return errors.New("otlp: negative timeout")
	}
	if _, err := ParseHeaders(cfg.Headers.Reveal()); err != nil {
		return err
	}
	return nil
}

// ResolvedProtocol returns ProtocolGRPC or ProtocolHTTP: cfg.Protocol if
// set, else env (the value of OTEL_EXPORTER_OTLP_PROTOCOL, which the
// caller reads), else gRPC.
func (cfg Config) ResolvedProtocol(env string) (string, error) {
	for _, p := range []string{cfg.Protocol, env} {
		switch strings.ToLower(p) {
		case "":
			continue
		case "grpc":
			return ProtocolGRPC, nil
		case "http", "http/protobuf":
			return ProtocolHTTP, nil
		default:
			return "", fmt.Errorf("otlp: invalid protocol %q (want grpc or http)", p)
		}
	}
	return ProtocolGRPC, nil
}

// ParseHeaders parses "key=value,key2=value2" with URL-encoded values, the
// format of OTEL_EXPORTER_OTLP_HEADERS. Error messages never include
// values.
func ParseHeaders(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for i, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("otlp: header %d is not key=value", i+1)
		}
		dv, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("otlp: header %q has an invalid escaped value", k)
		}
		out[k] = dv
	}
	return out, nil
}
