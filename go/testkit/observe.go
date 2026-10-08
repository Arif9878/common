package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Arif9878/common/go/observability/logging"
)

// Record is one decoded JSON log record.
type Record map[string]any

// Logs holds the records written by a logger from [NewLogger]. It is safe
// for concurrent use.
type Logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// NewLogger returns a logger built like production loggers
// (observability/logging: JSON, redaction, trace and request IDs) at debug
// level, writing to the returned Logs. If the test fails, the captured logs
// are printed with the failure.
func NewLogger(t testing.TB, opts ...logging.Option) (*slog.Logger, *Logs) {
	t.Helper()
	logs := &Logs{}
	logger, err := logging.New(logging.Config{Level: "debug"}, append(opts, logging.WithWriter(logs))...)
	if err != nil {
		t.Fatalf("testkit: logger: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() && logs.String() != "" {
			t.Logf("captured logs:\n%s", logs)
		}
	})
	return logger, logs
}

// String returns the raw log output.
func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Contains reports whether s appears anywhere in the output, for checks
// such as "this secret was never logged".
func (l *Logs) Contains(s string) bool { return strings.Contains(l.String(), s) }

// Records decodes all records written so far. Lines that are not JSON
// objects are skipped.
func (l *Logs) Records() []Record {
	var out []Record
	dec := json.NewDecoder(strings.NewReader(l.String()))
	for dec.More() {
		var r Record
		if err := dec.Decode(&r); err != nil {
			break
		}
		out = append(out, r)
	}
	return out
}

// Messages returns the records whose "msg" is msg.
func (l *Logs) Messages(msg string) []Record {
	return slices.DeleteFunc(l.Records(), func(r Record) bool { return r["msg"] != msg })
}

// Metrics reads what a meter provider from [NewMetrics] recorded.
type Metrics struct {
	t      testing.TB
	reader *sdkmetric.ManualReader
}

// NewMetrics returns a meter provider recording in memory, and the Metrics
// that reads it. Pass the provider to the code under test (every platform
// package has a WithMeterProvider option).
func NewMetrics(t testing.TB) (*sdkmetric.MeterProvider, *Metrics) {
	reader := sdkmetric.NewManualReader()
	return sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), &Metrics{t: t, reader: reader}
}

// Collect returns everything recorded so far.
func (m *Metrics) Collect() metricdata.ResourceMetrics {
	m.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(context.Background(), &rm); err != nil {
		m.t.Fatalf("testkit: collect metrics: %v", err)
	}
	return rm
}

// Has reports whether an instrument named name recorded anything.
func (m *Metrics) Has(name string) bool {
	m.t.Helper()
	return m.find(name) != nil
}

func (m *Metrics) find(name string) *metricdata.Metrics {
	m.t.Helper()
	rm := m.Collect()
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}

func (m *Metrics) mustFind(name string) metricdata.Metrics {
	m.t.Helper()
	found := m.find(name)
	if found == nil {
		m.t.Fatalf("testkit: no metric %q recorded", name)
	}
	return *found
}

func matches(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		if v, ok := set.Value(kv.Key); !ok || v != kv.Value {
			return false
		}
	}
	return true
}

// Sum returns the total of counter or up-down-counter name over the data
// points whose attributes include all of attrs. It fails the test if no
// such instrument recorded anything, so a misspelled name cannot pass as 0.
func (m *Metrics) Sum(name string, attrs ...attribute.KeyValue) float64 {
	m.t.Helper()
	var total float64
	switch d := m.mustFind(name).Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				total += float64(dp.Value)
			}
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				total += dp.Value
			}
		}
	default:
		m.t.Fatalf("testkit: metric %q is %T, not a sum", name, d)
	}
	return total
}

// Gauge returns the value of gauge name for the first data point whose
// attributes include all of attrs. It fails the test if there is none.
func (m *Metrics) Gauge(name string, attrs ...attribute.KeyValue) float64 {
	m.t.Helper()
	switch d := m.mustFind(name).Data.(type) {
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				return float64(dp.Value)
			}
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				return dp.Value
			}
		}
	default:
		m.t.Fatalf("testkit: metric %q is %T, not a gauge", name, d)
	}
	m.t.Fatalf("testkit: gauge %q has no data point with %v", name, attrs)
	return 0
}

// HistogramCount returns how many values histogram name recorded over the
// data points whose attributes include all of attrs. It fails the test if
// no such instrument recorded anything.
func (m *Metrics) HistogramCount(name string, attrs ...attribute.KeyValue) uint64 {
	m.t.Helper()
	var n uint64
	switch d := m.mustFind(name).Data.(type) {
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				n += dp.Count
			}
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				n += dp.Count
			}
		}
	default:
		m.t.Fatalf("testkit: metric %q is %T, not a histogram", name, d)
	}
	return n
}

// HistogramSum returns the sum of the values histogram name recorded over
// the data points whose attributes include all of attrs. It fails the test
// if no such instrument recorded anything.
func (m *Metrics) HistogramSum(name string, attrs ...attribute.KeyValue) float64 {
	m.t.Helper()
	var sum float64
	switch d := m.mustFind(name).Data.(type) {
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				sum += dp.Sum
			}
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			if matches(dp.Attributes, attrs) {
				sum += float64(dp.Sum)
			}
		}
	default:
		m.t.Fatalf("testkit: metric %q is %T, not a histogram", name, d)
	}
	return sum
}

// Spans holds the spans ended on a tracer provider from [NewTracer].
type Spans struct {
	rec *tracetest.SpanRecorder
}

// NewTracer returns a tracer provider that records spans in memory, and
// the Spans that reads them. Every span is sampled.
func NewTracer(t testing.TB) (*sdktrace.TracerProvider, *Spans) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, &Spans{rec: rec}
}

// Ended returns the spans ended so far, in end order.
func (s *Spans) Ended() []sdktrace.ReadOnlySpan { return s.rec.Ended() }

// Named returns the ended spans called name.
func (s *Spans) Named(name string) []sdktrace.ReadOnlySpan {
	return slices.DeleteFunc(s.Ended(), func(sp sdktrace.ReadOnlySpan) bool { return sp.Name() != name })
}
