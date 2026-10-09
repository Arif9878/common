// Package logs exports log records over OTLP, so logs reach the same
// backend as traces and metrics (Grafana Loki, New Relic, Datadog, Elastic,
// a Collector) without a log shipper reading standard output.
//
// It provides the OpenTelemetry LoggerProvider; the logging package's
// logger writes to it when its Output includes "otlp":
//
//	lp, err := logs.Init(ctx, cfg.Logs, logs.WithOTLP(cfg.OTLP))  // LOGS_EXPORTER=otlp
//	...
//	logger, err := logging.New(cfg.Log, logging.WithLoggerProvider(lp.LoggerProvider())) // LOG_OUTPUT=both
//	...
//	shutdown.Register(graceful.Telemetry, "logs", lp.Shutdown)
//
// Records keep the logger's redaction and attributes. Their trace and span
// IDs are set from the context natively, so backends link logs to traces
// without parsing fields.
//
// Exporting is asynchronous: records are batched and sent in the
// background, and dropped when the queue is full (for example while the
// backend is unreachable). Standard output, when also enabled, is not
// affected.
package logs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/Arif9878/common/go/observability/internal/otelres"
	"github.com/Arif9878/common/go/observability/internal/otlpconf"
	"github.com/Arif9878/common/go/observability/otlp"
)

// Exporters accepted in [Config].Exporter.
const (
	ExporterNone = "none"
	// ExporterOTLP exports with the protocol of the shared otlp.Config
	// ([WithOTLP]), else OTEL_EXPORTER_OTLP_PROTOCOL, else gRPC.
	ExporterOTLP     = "otlp"
	ExporterOTLPGRPC = "otlp-grpc"
	ExporterOTLPHTTP = "otlp-http"
)

// Config configures log export. The zero value exports nothing.
// Environment variable names are relative; the service chooses the prefix,
// for example LOGS_.
type Config struct {
	// Exporter is "none", "otlp", "otlp-grpc" or "otlp-http".
	Exporter string `env:"EXPORTER" envDefault:"none"`
	// Endpoint and Insecure override the shared otlp.Config for logs.
	Endpoint string `env:"ENDPOINT"`
	Insecure bool   `env:"INSECURE"`

	// Service, Environment and Version describe the service in every
	// record's resource.
	Service     string `env:"SERVICE"`
	Environment string `env:"ENVIRONMENT"`
	Version     string `env:"VERSION"`
}

// Validate reports whether cfg is valid. [Init] calls it.
func (cfg Config) Validate() error {
	switch strings.ToLower(cfg.Exporter) {
	case "", ExporterNone, ExporterOTLP, ExporterOTLPGRPC, ExporterOTLPHTTP:
		return nil
	default:
		return errors.New("unknown exporter (want none, otlp, otlp-grpc or otlp-http)")
	}
}

// Option configures [Init].
type Option func(*options)

type options struct {
	otlp     otlp.Config
	exporter sdklog.Exporter
	global   bool
}

// WithOTLP sets the shared OTLP connection settings. Config's Endpoint and
// Insecure override them for logs.
func WithOTLP(cfg otlp.Config) Option { return func(o *options) { o.otlp = cfg } }

// WithExporter uses exp instead of the exporter named in Config, for
// example an in-memory exporter in tests.
func WithExporter(exp sdklog.Exporter) Option { return func(o *options) { o.exporter = exp } }

// WithGlobal registers the provider as the OpenTelemetry global logger
// provider, for libraries that log through the OpenTelemetry API.
func WithGlobal() Option { return func(o *options) { o.global = true } }

// Provider owns the logger provider created by [Init].
type Provider struct {
	lp *sdklog.LoggerProvider

	shutdownOnce sync.Once
	shutdownErr  error
}

// Init creates a logger provider from cfg. With Exporter "none" the
// provider discards records.
func Init(ctx context.Context, cfg Config, opts ...Option) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	res, err := otelres.New(ctx, cfg.Service, cfg.Environment, cfg.Version)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	lpOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	exp := o.exporter
	if exp == nil {
		if exp, err = newExporter(ctx, cfg, o.otlp); err != nil {
			return nil, fmt.Errorf("logs: %w", err)
		}
	}
	if exp != nil {
		lpOpts = append(lpOpts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	}
	lp := sdklog.NewLoggerProvider(lpOpts...)
	if o.global {
		otel.SetLoggerProvider(lp)
	}
	return &Provider{lp: lp}, nil
}

func newExporter(ctx context.Context, cfg Config, shared otlp.Config) (sdklog.Exporter, error) {
	exporter := strings.ToLower(cfg.Exporter)
	if exporter == "" || exporter == ExporterNone {
		return nil, nil
	}
	set, err := otlpconf.Resolve("LOGS", exporter, shared, cfg.Endpoint, cfg.Insecure)
	if err != nil {
		return nil, err
	}
	if set.Protocol == otlp.ProtocolGRPC {
		var opts []otlploggrpc.Option
		switch {
		case set.EndpointURL:
			opts = append(opts, otlploggrpc.WithEndpointURL(set.Endpoint))
		case set.Endpoint != "":
			opts = append(opts, otlploggrpc.WithEndpoint(set.Endpoint))
		}
		if set.Insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		if set.Headers != nil {
			opts = append(opts, otlploggrpc.WithHeaders(set.Headers))
		}
		if set.Compression == "gzip" {
			opts = append(opts, otlploggrpc.WithCompressor("gzip"))
		}
		if set.Timeout > 0 {
			opts = append(opts, otlploggrpc.WithTimeout(set.Timeout))
		}
		return otlploggrpc.New(ctx, opts...)
	}
	var opts []otlploghttp.Option
	switch {
	case set.EndpointURL:
		opts = append(opts, otlploghttp.WithEndpointURL(set.Endpoint))
	case set.Endpoint != "":
		opts = append(opts, otlploghttp.WithEndpoint(set.Endpoint))
	}
	if set.Insecure {
		opts = append(opts, otlploghttp.WithInsecure())
	}
	if set.Headers != nil {
		opts = append(opts, otlploghttp.WithHeaders(set.Headers))
	}
	switch set.Compression {
	case "gzip":
		opts = append(opts, otlploghttp.WithCompression(otlploghttp.GzipCompression))
	case "none":
		opts = append(opts, otlploghttp.WithCompression(otlploghttp.NoCompression))
	}
	if set.Timeout > 0 {
		opts = append(opts, otlploghttp.WithTimeout(set.Timeout))
	}
	return otlploghttp.New(ctx, opts...)
}

// LoggerProvider returns the provider, for logging.WithLoggerProvider.
func (p *Provider) LoggerProvider() otellog.LoggerProvider { return p.lp }

// ForceFlush exports all buffered records.
func (p *Provider) ForceFlush(ctx context.Context) error { return p.lp.ForceFlush(ctx) }

// Shutdown flushes buffered records and stops the provider. Only the first
// call has an effect; later calls return the first result.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.shutdownOnce.Do(func() { p.shutdownErr = p.lp.Shutdown(ctx) })
	return p.shutdownErr
}
