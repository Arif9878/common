// Package metrics sets up OpenTelemetry metrics, served in Prometheus
// format for scraping (the default), pushed over OTLP to any backend that
// accepts it (Grafana Cloud, New Relic, Datadog Agent, a Collector), or
// both: Config.Exporter chooses, so changing backend is configuration.
//
// Call [Init] once in main, serve [Provider.Handler] on the metrics port,
// and pass [Provider.MeterProvider] to packages (or use [WithGlobal]):
//
//	mp, err := metrics.Init(ctx, cfg.Metrics, metrics.WithGlobal(), metrics.WithOTLP(cfg.OTLP))
//	if err != nil {
//		return err
//	}
//	mux.Handle("/metrics", mp.Handler())
//
// Packages record metrics through the OpenTelemetry metric API only. They
// take a metric.MeterProvider option defaulting to otel.GetMeterProvider()
// and never import this package or the Prometheus client.
//
// # Naming conventions
//
// Instruments use OpenTelemetry names: lower-case, dot-separated, with the
// unit in the unit field, not the name. The exporter translates them:
//
//	name "http.server.request.duration", unit "s"  ->  http_server_request_duration_seconds
//	name "kafka.consumer.records", counter         ->  kafka_consumer_records_total
//
// Durations are recorded in seconds (unit "s") as float64 histograms.
// Histograms default to [DurationBuckets]; a histogram of anything else,
// such as sizes or counts, must declare its own boundaries with
// metric.WithExplicitBucketBoundaries. Use the OpenTelemetry
// semantic-convention names where one exists (HTTP, gRPC, database,
// messaging).
//
// # Attributes (labels)
//
// Every distinct combination of attribute values is a separate time series,
// so attribute values must come from a small, bounded set. Allowed:
// enumerations and names fixed at configuration time, such as method, route
// template, status class, topic, partition, outcome, error_type, pool and
// breaker name. Forbidden: user, order or request IDs, raw URLs and paths,
// error messages, message keys and offsets.
//
// As a safety net, each instrument keeps at most [Config.CardinalityLimit]
// series (2000 by default). Measurements for further combinations are
// aggregated into one series labeled otel_metric_overflow="true"; seeing
// that label means an instrument has a cardinality bug.
//
// The service name, version and environment are exported once, in the
// target_info metric, rather than as labels on every series.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Arif9878/common/go/observability/internal/otelres"
	"github.com/Arif9878/common/go/observability/internal/otlpconf"
	"github.com/Arif9878/common/go/observability/otlp"
)

// DurationBuckets returns the default histogram boundaries, in seconds. They follow the OpenTelemetry HTTP semantic
// conventions and cover 5ms to 10s.
func DurationBuckets() []float64 {
	return []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}
}

// Exporters accepted in [Config].Exporter.
const (
	// ExporterPrometheus serves metrics for scraping from [Provider.Handler].
	ExporterPrometheus = "prometheus"
	// ExporterOTLP pushes metrics over OTLP with the protocol of the shared
	// otlp.Config ([WithOTLP]), else OTEL_EXPORTER_OTLP_PROTOCOL, else gRPC.
	ExporterOTLP     = "otlp"
	ExporterOTLPGRPC = "otlp-grpc"
	ExporterOTLPHTTP = "otlp-http"
	// ExporterNone records metrics without exporting them.
	ExporterNone = "none"
)

// Config configures metrics. The zero value is valid and serves Prometheus
// metrics. Environment variable names are relative; the service chooses
// the prefix, for example METRICS_.
type Config struct {
	// Exporter lists where metrics go, comma-separated: "prometheus"
	// (scraped from Provider.Handler), "otlp", "otlp-grpc" or "otlp-http"
	// (pushed every Interval), or "none". "prometheus,otlp" does both, for
	// example while moving to a new backend. Empty means "prometheus".
	Exporter string `env:"EXPORTER" envDefault:"prometheus"`
	// Endpoint and Insecure override the shared otlp.Config for metrics.
	Endpoint string `env:"ENDPOINT"`
	Insecure bool   `env:"INSECURE"`
	// Interval is the time between OTLP pushes. Zero means 60s.
	Interval time.Duration `env:"EXPORT_INTERVAL"`
	// Temporality of pushed counters and histograms: "cumulative" (the
	// default; what Prometheus-style backends such as Grafana Mimir expect)
	// or "delta" (what New Relic and Datadog prefer). Up-down counters and
	// gauges are always cumulative. Prometheus scraping is unaffected.
	Temporality string `env:"TEMPORALITY"`

	// CardinalityLimit is the maximum number of series per instrument.
	// Zero means 2000. It cannot be disabled.
	CardinalityLimit int `env:"CARDINALITY_LIMIT"`

	// Service, Environment and Version describe the service: in target_info
	// for Prometheus, as resource attributes for OTLP.
	Service     string `env:"SERVICE"`
	Environment string `env:"ENVIRONMENT"`
	Version     string `env:"VERSION"`
}

// exporters parses cfg.Exporter into whether to serve Prometheus and which
// OTLP exporter to push with ("" for none).
func (cfg Config) exporters() (prom bool, otlpExp string, err error) {
	list := strings.TrimSpace(cfg.Exporter)
	if list == "" {
		return true, "", nil
	}
	for _, e := range strings.Split(strings.ToLower(list), ",") {
		switch e = strings.TrimSpace(e); e {
		case ExporterPrometheus:
			prom = true
		case ExporterOTLP, ExporterOTLPGRPC, ExporterOTLPHTTP:
			if otlpExp != "" {
				return false, "", errors.New("more than one OTLP exporter")
			}
			otlpExp = e
		case ExporterNone:
		default:
			return false, "", fmt.Errorf("unknown exporter %q (want prometheus, otlp, otlp-grpc, otlp-http or none)", e)
		}
	}
	return prom, otlpExp, nil
}

// Validate reports whether cfg is valid. [Init] calls it.
func (cfg Config) Validate() error {
	if cfg.CardinalityLimit < 0 {
		return errors.New("cardinality limit is negative")
	}
	if _, _, err := cfg.exporters(); err != nil {
		return err
	}
	if cfg.Interval < 0 {
		return errors.New("export interval is negative")
	}
	switch strings.ToLower(cfg.Temporality) {
	case "", "cumulative", "delta":
	default:
		return errors.New("invalid temporality (want cumulative or delta)")
	}
	return nil
}

// Option configures [Init].
type Option func(*options)

type options struct {
	registry *prometheus.Registry
	views    []sdkmetric.View
	global   bool
	otlp     otlp.Config
}

// WithOTLP sets the shared OTLP connection settings: endpoint, headers
// (such as an API key), protocol, TLS, compression and timeout. Config's
// Endpoint and Insecure override them for metrics.
func WithOTLP(cfg otlp.Config) Option {
	return func(o *options) { o.otlp = cfg }
}

// WithRegistry exports into reg instead of a new private registry, for
// example to serve metrics from existing Prometheus collectors on the same
// endpoint. Go runtime and process collectors are not added to reg.
func WithRegistry(reg *prometheus.Registry) Option {
	return func(o *options) { o.registry = reg }
}

// WithView adds OpenTelemetry views, for example to drop an attribute or
// change histogram boundaries for a specific instrument.
func WithView(views ...sdkmetric.View) Option {
	return func(o *options) { o.views = append(o.views, views...) }
}

// WithGlobal registers the meter provider as the OpenTelemetry global.
// Instrumentation libraries use the global by default.
func WithGlobal() Option {
	return func(o *options) { o.global = true }
}

// Provider owns the meter provider and registry created by [Init].
type Provider struct {
	mp      *sdkmetric.MeterProvider
	handler http.Handler

	shutdownOnce sync.Once
	shutdownErr  error
}

// Init creates a meter provider that exports as cfg.Exporter says: to a
// Prometheus registry served by [Provider.Handler], over OTLP, or both.
// The OTLP exporter connects lazily, so Init succeeds while the backend is
// unreachable; pushes then fail and are retried at the next interval.
func Init(ctx context.Context, cfg Config, opts ...Option) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	limit := cfg.CardinalityLimit
	if limit == 0 {
		limit = 2000
	}
	prom, otlpExp, _ := cfg.exporters()

	res, err := otelres.New(ctx, cfg.Service, cfg.Environment, cfg.Version)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	mpOpts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithCardinalityLimit(limit),
		sdkmetric.WithView(o.views...),
	}

	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "metrics are not served for scraping (METRICS_EXPORTER has no prometheus)", http.StatusNotFound)
	}))
	if prom {
		reg := o.registry
		if reg == nil {
			reg = prometheus.NewRegistry()
			reg.MustRegister(
				collectors.NewGoCollector(),
				collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			)
		}
		exp, err := otelprom.New(
			otelprom.WithRegisterer(reg),
			otelprom.WithAggregationSelector(aggregationSelector),
		)
		if err != nil {
			return nil, fmt.Errorf("metrics: create exporter: %w", err)
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(exp))
		handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorHandling:       promhttp.ContinueOnError,
			MaxRequestsInFlight: 4,
			Timeout:             10 * time.Second,
		})
	}
	if otlpExp != "" {
		exp, err := newOTLPExporter(ctx, cfg, otlpExp, o.otlp)
		if err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
		interval := cfg.Interval
		if interval == 0 {
			interval = 60 * time.Second
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))))
	}

	mp := sdkmetric.NewMeterProvider(mpOpts...)
	if otlpExp != "" && !prom {
		// Without Prometheus's Go collector, report the runtime through OTel.
		if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
			_ = mp.Shutdown(ctx)
			return nil, fmt.Errorf("metrics: runtime metrics: %w", err)
		}
	}
	if o.global {
		otel.SetMeterProvider(mp)
	}
	return &Provider{mp: mp, handler: handler}, nil
}

func newOTLPExporter(ctx context.Context, cfg Config, exporter string, shared otlp.Config) (sdkmetric.Exporter, error) {
	set, err := otlpconf.Resolve("METRICS", exporter, shared, cfg.Endpoint, cfg.Insecure)
	if err != nil {
		return nil, err
	}
	temporality := sdkmetric.DefaultTemporalitySelector
	if strings.EqualFold(cfg.Temporality, "delta") {
		temporality = deltaTemporality
	}
	if set.Protocol == otlp.ProtocolGRPC {
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithAggregationSelector(aggregationSelector),
			otlpmetricgrpc.WithTemporalitySelector(temporality),
		}
		switch {
		case set.EndpointURL:
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(set.Endpoint))
		case set.Endpoint != "":
			opts = append(opts, otlpmetricgrpc.WithEndpoint(set.Endpoint))
		}
		if set.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		if set.Headers != nil {
			opts = append(opts, otlpmetricgrpc.WithHeaders(set.Headers))
		}
		if set.Compression == "gzip" {
			opts = append(opts, otlpmetricgrpc.WithCompressor("gzip"))
		}
		if set.Timeout > 0 {
			opts = append(opts, otlpmetricgrpc.WithTimeout(set.Timeout))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithAggregationSelector(aggregationSelector),
		otlpmetrichttp.WithTemporalitySelector(temporality),
	}
	switch {
	case set.EndpointURL:
		opts = append(opts, otlpmetrichttp.WithEndpointURL(set.Endpoint))
	case set.Endpoint != "":
		opts = append(opts, otlpmetrichttp.WithEndpoint(set.Endpoint))
	}
	if set.Insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	if set.Headers != nil {
		opts = append(opts, otlpmetrichttp.WithHeaders(set.Headers))
	}
	switch set.Compression {
	case "gzip":
		opts = append(opts, otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression))
	case "none":
		opts = append(opts, otlpmetrichttp.WithCompression(otlpmetrichttp.NoCompression))
	}
	if set.Timeout > 0 {
		opts = append(opts, otlpmetrichttp.WithTimeout(set.Timeout))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

// deltaTemporality reports counters and histograms as deltas, as New Relic
// and Datadog prefer. Up-down counters stay cumulative: their deltas would
// lose the current value.
func deltaTemporality(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	switch kind {
	case sdkmetric.InstrumentKindUpDownCounter, sdkmetric.InstrumentKindObservableUpDownCounter:
		return metricdata.CumulativeTemporality
	default:
		return metricdata.DeltaTemporality
	}
}

// aggregationSelector makes DurationBuckets the default histogram
// boundaries. Being a reader default rather than a view, it yields to
// boundaries an instrument declares with metric.WithExplicitBucketBoundaries.
func aggregationSelector(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	if kind == sdkmetric.InstrumentKindHistogram {
		return sdkmetric.AggregationExplicitBucketHistogram{Boundaries: DurationBuckets()}
	}
	return sdkmetric.DefaultAggregationSelector(kind)
}

// MeterProvider returns the provider for passing to packages and
// instrumentation.
func (p *Provider) MeterProvider() metric.MeterProvider { return p.mp }

// Handler serves the registry in the Prometheus exposition format. It allows
// at most 4 concurrent scrapes and gives up on a scrape after 10 seconds.
// Without the prometheus exporter it answers 404.
func (p *Provider) Handler() http.Handler { return p.handler }

// Shutdown pushes the last OTLP measurements, if any, and stops the meter
// provider; call it after the last scrape is no longer needed. Only the
// first call has an effect; later calls return the first result.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.shutdownOnce.Do(func() { p.shutdownErr = p.mp.Shutdown(ctx) })
	return p.shutdownErr
}
