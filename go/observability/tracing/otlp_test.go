package tracing_test

import (
	"context"
	"testing"

	"github.com/Arif9878/common/go/observability/internal/otlptest"
	"github.com/Arif9878/common/go/observability/otlp"
	"github.com/Arif9878/common/go/observability/tracing"
)

func TestExportsWithSharedOTLPConfig(t *testing.T) {
	rc := otlptest.New(t)
	ctx := context.Background()
	p, err := tracing.Init(ctx, tracing.Config{Exporter: tracing.ExporterOTLP, Service: "orders"},
		tracing.WithOTLP(otlp.Config{Protocol: "http", Endpoint: rc.URL, Headers: "api-key=nr-key", Compression: "gzip"}))
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.TracerProvider().Tracer("test").Start(ctx, "charge")
	span.End()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	reqs := rc.Requests("/v1/traces")
	if len(reqs) != 1 {
		t.Fatalf("%d trace exports, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Header.Get("api-key") != "nr-key" || !r.Gzipped {
		t.Errorf("api-key %q, gzipped %v", r.Header.Get("api-key"), r.Gzipped)
	}
	rs := r.Traces.GetResourceSpans()[0]
	if name := rs.GetScopeSpans()[0].GetSpans()[0].GetName(); name != "charge" {
		t.Errorf("span = %q", name)
	}
	found := false
	for _, a := range rs.GetResource().GetAttributes() {
		found = found || (a.GetKey() == "service.name" && a.GetValue().GetStringValue() == "orders")
	}
	if !found {
		t.Error("resource has no service.name=orders")
	}
}

func TestSignalEndpointOverridesShared(t *testing.T) {
	shared, own := otlptest.New(t), otlptest.New(t)
	ctx := context.Background()
	p, err := tracing.Init(ctx, tracing.Config{Exporter: tracing.ExporterOTLPHTTP, Endpoint: own.URL},
		tracing.WithOTLP(otlp.Config{Endpoint: shared.URL}))
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.TracerProvider().Tracer("test").Start(ctx, "s")
	span.End()
	_ = p.Shutdown(ctx)
	if len(own.Requests("/v1/traces")) != 1 || len(shared.Requests("/v1/traces")) != 0 {
		t.Error("traces did not go to the signal's own endpoint")
	}
}
