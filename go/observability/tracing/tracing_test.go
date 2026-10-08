package tracing_test

import (
	"context"
	stderrors "errors"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/tracing"
)

func ptr(f float64) *float64 { return &f }

func initWithMemory(t *testing.T, cfg tracing.Config, opts ...tracing.Option) (*tracing.Provider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	p, err := tracing.Init(context.Background(), cfg, append(opts, tracing.WithExporter(exp))...)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, exp
}

func attrMap(attrs []attribute.KeyValue) map[attribute.Key]string {
	m := make(map[attribute.Key]string)
	for _, kv := range attrs {
		m[kv.Key] = kv.Value.String()
	}
	return m
}

func TestExportAndResource(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=payments,service.version=from-env")
	p, exp := initWithMemory(t, tracing.Config{Service: "orders", Environment: "prod", Version: "1.2.3"})

	_, span := p.TracerProvider().Tracer("test").Start(context.Background(), "op")
	span.End()
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "op" {
		t.Fatalf("spans = %v", spans)
	}
	res := attrMap(spans[0].Resource.Attributes())
	want := map[attribute.Key]string{
		"service.name":                "orders",
		"deployment.environment.name": "prod",
		"service.version":             "1.2.3", // config overrides env
		"team":                        "payments",
	}
	for k, v := range want {
		if res[k] != v {
			t.Errorf("resource %s = %q, want %q", k, res[k], v)
		}
	}
}

func TestSampleRatio(t *testing.T) {
	p, _ := initWithMemory(t, tracing.Config{SampleRatio: ptr(0)})
	tracer := p.TracerProvider().Tracer("test")

	_, root := tracer.Start(context.Background(), "root")
	if root.IsRecording() {
		t.Error("ratio 0 sampled a new trace")
	}
	root.End()

	// A sampled parent from upstream is followed.
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	_, child := tracer.Start(trace.ContextWithRemoteSpanContext(context.Background(), parent), "child")
	if !child.IsRecording() {
		t.Error("sampled parent not followed")
	}
	child.End()
}

func TestInvalidConfig(t *testing.T) {
	for name, cfg := range map[string]tracing.Config{
		"exporter":   {Exporter: "zipkin"},
		"ratio high": {SampleRatio: ptr(1.5)},
		"ratio low":  {SampleRatio: ptr(-0.1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tracing.Init(context.Background(), cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestExportersStartWithoutCollector(t *testing.T) {
	for _, cfg := range []tracing.Config{
		{Exporter: tracing.ExporterNone},
		{Exporter: tracing.ExporterOTLPGRPC, Endpoint: "127.0.0.1:1", Insecure: true},
		{Exporter: tracing.ExporterOTLPHTTP, Endpoint: "http://127.0.0.1:1/v1/traces"},
	} {
		t.Run(cfg.Exporter, func(t *testing.T) {
			p, err := tracing.Init(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			_, span := p.TracerProvider().Tracer("test").Start(context.Background(), "op")
			if !span.SpanContext().IsValid() {
				t.Error("no trace ID generated")
			}
			span.End()

			// Shutdown must respect its deadline even though the collector is
			// unreachable; the export error itself is expected.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			_ = p.Shutdown(ctx)
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("Shutdown took %v", d)
			}
		})
	}
}

func TestShutdownIdempotent(t *testing.T) {
	p, err := tracing.Init(context.Background(), tracing.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}
}

func TestWithGlobalPropagation(t *testing.T) {
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	initWithMemory(t, tracing.Config{}, tracing.WithGlobal())

	ctx, span := otel.Tracer("test").Start(context.Background(), "client")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Fatal("global tracer provider not set")
	}

	header := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
	if header.Get("traceparent") == "" {
		t.Fatal("traceparent not injected")
	}
	got := trace.SpanContextFromContext(
		tracing.Propagator().Extract(context.Background(), propagation.HeaderCarrier(header)))
	if got.TraceID() != span.SpanContext().TraceID() {
		t.Fatalf("extracted trace %v, want %v", got.TraceID(), span.SpanContext().TraceID())
	}
}

func TestEnd(t *testing.T) {
	p, exp := initWithMemory(t, tracing.Config{})
	tracer := p.TracerProvider().Tracer("test")

	op := func(name string, opErr error) (err error) {
		_, span := tracer.Start(context.Background(), name)
		defer tracing.End(span, &err)
		return opErr
	}
	_ = op("ok", nil)
	_ = op("fail", errors.Unavailable.Wrap(stderrors.New("refused"), "query"))
	_, span := tracer.Start(context.Background(), "nil errp")
	tracing.End(span, nil)
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d spans", len(spans))
	}
	for _, s := range spans {
		switch s.Name {
		case "ok", "nil errp":
			if s.Status.Code != codes.Unset || len(s.Events) != 0 {
				t.Errorf("%s: status %v, events %v", s.Name, s.Status, s.Events)
			}
		case "fail":
			if s.Status.Code != codes.Error || s.Status.Description != "unavailable" {
				t.Errorf("status = %+v", s.Status)
			}
			if attrMap(s.Attributes)["error.type"] != "unavailable" {
				t.Errorf("attributes = %v", s.Attributes)
			}
			if len(s.Events) != 1 || s.Events[0].Name != "exception" {
				t.Errorf("events = %v", s.Events)
			}
		}
	}
}
