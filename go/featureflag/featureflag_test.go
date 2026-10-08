package featureflag_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/featureflag"
)

func newEvaluator(t *testing.T, opts ...featureflag.Option) *featureflag.OpenFeature {
	t.Helper()
	domain := t.Name()
	// Percentage-style targeting: enabled for user "u-beta" only.
	betaOnly := func(_ memprovider.InMemoryFlag, ctx openfeature.FlattenedContext) (any, openfeature.ProviderResolutionDetail) {
		if ctx[openfeature.TargetingKey] == "u-beta" {
			return true, openfeature.ProviderResolutionDetail{Reason: openfeature.TargetingMatchReason, Variant: "on"}
		}
		return false, openfeature.ProviderResolutionDetail{Reason: openfeature.TargetingMatchReason, Variant: "off"}
	}
	provider := memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{
		"checkout.new-pricing": {Key: "checkout.new-pricing", State: memprovider.Enabled, DefaultVariant: "off",
			Variants: map[string]any{"on": true, "off": false}, ContextEvaluator: memprovider.ContextEvaluator(&betaOnly)},
		"search.ranker": {Key: "search.ranker", State: memprovider.Enabled, DefaultVariant: "v2",
			Variants: map[string]any{"v1": "bm25", "v2": "hybrid"}},
		"batch.size": {Key: "batch.size", State: memprovider.Enabled, DefaultVariant: "big",
			Variants: map[string]any{"big": int64(500)}},
	})
	if err := openfeature.SetNamedProviderAndWait(domain, provider); err != nil {
		t.Fatal(err)
	}
	return featureflag.NewOpenFeature(openfeature.NewClient(domain), opts...)
}

func TestOpenFeatureEvaluation(t *testing.T) {
	e := newEvaluator(t)
	ctx := context.Background()

	on, err := e.Bool(ctx, "checkout.new-pricing", false, featureflag.Attributes{featureflag.TargetingKey: "u-beta", "country": "ID"})
	if err != nil || !on {
		t.Errorf("beta user = %v, %v", on, err)
	}
	on, err = e.Bool(ctx, "checkout.new-pricing", false, featureflag.Attributes{featureflag.TargetingKey: "u-other"})
	if err != nil || on {
		t.Errorf("other user = %v, %v", on, err)
	}
	if s, err := e.String(ctx, "search.ranker", "bm25", nil); err != nil || s != "hybrid" {
		t.Errorf("String = %q, %v", s, err)
	}
	if n, err := e.Int(ctx, "batch.size", 100, nil); err != nil || n != 500 {
		t.Errorf("Int = %d, %v", n, err)
	}
}

func TestFailuresReturnDefault(t *testing.T) {
	e := newEvaluator(t)
	ctx := context.Background()

	// Fail open: an unknown flag with default true keeps behavior on.
	on, err := e.Bool(ctx, "missing.kill-switch", true, nil)
	if !on || errors.KindOf(err) != errors.NotFound {
		t.Errorf("missing flag = %v, %v", on, err)
	}
	// Wrong type: the default is returned.
	n, err := e.Int(ctx, "search.ranker", 7, nil)
	if n != 7 || errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("type mismatch = %v, %v", n, err)
	}

	// No provider registered for this domain: OpenFeature falls back to its
	// no-op provider, which returns defaults.
	none := featureflag.NewOpenFeature(openfeature.NewClient(t.Name() + "-unregistered"))
	if on, _ := none.Bool(ctx, "anything", false, nil); on {
		t.Error("fail-closed default not honored without a provider")
	}
}

func TestTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	e := newEvaluator(t, featureflag.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	ctx, span := tp.Tracer("test").Start(context.Background(), "request")
	_, _ = e.Bool(ctx, "checkout.new-pricing", false, featureflag.Attributes{featureflag.TargetingKey: "u-beta", "email": "a@b.c"})
	_, _ = e.Bool(ctx, "missing", false, nil)
	span.End()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, dp := range rm.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64]).DataPoints {
		f, _ := dp.Attributes.Value("flag")
		o, _ := dp.Attributes.Value("outcome")
		got[f.AsString()+"/"+o.AsString()] = dp.Value
	}
	if got["checkout.new-pricing/ok"] != 1 || got["missing/error"] != 1 {
		t.Errorf("metrics = %v", got)
	}

	events := spans.Ended()[0].Events()
	if len(events) != 2 || events[0].Name != "feature_flag.evaluation" {
		t.Fatalf("events = %v", events)
	}
	for _, ev := range events {
		for _, kv := range ev.Attributes {
			if strings.Contains(kv.Value.String(), "a@b.c") || strings.Contains(kv.Value.String(), "u-beta") {
				t.Errorf("targeting attribute recorded: %v", kv)
			}
		}
	}
}

func TestStatic(t *testing.T) {
	var e featureflag.Evaluator = featureflag.Static{"a": true, "b": "x", "c": 3, "d": int64(4)}
	ctx := context.Background()
	if v, err := e.Bool(ctx, "a", false, nil); !v || err != nil {
		t.Error("bool")
	}
	if v, err := e.String(ctx, "b", "", nil); v != "x" || err != nil {
		t.Error("string")
	}
	if v, err := e.Int(ctx, "c", 0, nil); v != 3 || err != nil {
		t.Error("int")
	}
	if v, err := e.Int(ctx, "d", 0, nil); v != 4 || err != nil {
		t.Error("int64")
	}
	if v, err := e.Bool(ctx, "missing", true, nil); !v || errors.KindOf(err) != errors.NotFound {
		t.Errorf("missing = %v, %v", v, err)
	}
	if v, err := e.Bool(ctx, "b", true, nil); !v || errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("wrong type = %v, %v", v, err)
	}
}
