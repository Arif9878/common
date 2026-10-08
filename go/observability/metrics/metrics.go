// Package metrics sets up OpenTelemetry metrics exported in Prometheus
// format for scraping.
//
// Call [Init] once in main, serve [Provider.Handler] on the metrics port,
// and pass [Provider.MeterProvider] to packages (or use [WithGlobal]):
//
//	mp, err := metrics.Init(ctx, cfg.Metrics, metrics.WithGlobal())
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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/Arif9878/common/go/observability/internal/otelres"
)

// DurationBuckets returns the default histogram boundaries, in seconds. They follow the OpenTelemetry HTTP semantic
// conventions and cover 5ms to 10s.
func DurationBuckets() []float64 {
	return []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}
}

// Config configures metrics. The zero value is valid. Environment variable
// names are relative; the service chooses the prefix, for example METRICS_.
type Config struct {
	// CardinalityLimit is the maximum number of series per instrument.
	// Zero means 2000. It cannot be disabled.
	CardinalityLimit int `env:"CARDINALITY_LIMIT"`

	// Service, Environment and Version are exported in target_info.
	Service     string `env:"SERVICE"`
	Environment string `env:"ENVIRONMENT"`
	Version     string `env:"VERSION"`
}

// Validate reports whether cfg is valid. [Init] calls it.
func (cfg Config) Validate() error {
	if cfg.CardinalityLimit < 0 {
		return errors.New("cardinality limit is negative")
	}
	return nil
}

// Option configures [Init].
type Option func(*options)

type options struct {
	registry *prometheus.Registry
	views    []sdkmetric.View
	global   bool
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

// Init creates a meter provider that exports to a Prometheus registry.
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
	res, err := otelres.New(ctx, cfg.Service, cfg.Environment, cfg.Version)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(exp),
		sdkmetric.WithCardinalityLimit(limit),
		sdkmetric.WithView(o.views...),
	)
	if o.global {
		otel.SetMeterProvider(mp)
	}

	return &Provider{
		mp: mp,
		handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorHandling:       promhttp.ContinueOnError,
			MaxRequestsInFlight: 4,
			Timeout:             10 * time.Second,
		}),
	}, nil
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
func (p *Provider) Handler() http.Handler { return p.handler }

// Shutdown stops the meter provider. Prometheus is pull-based, so there is
// nothing to flush; call it after the last scrape is no longer needed. Only
// the first call has an effect; later calls return the first result.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.shutdownOnce.Do(func() { p.shutdownErr = p.mp.Shutdown(ctx) })
	return p.shutdownErr
}
