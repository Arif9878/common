package tenant_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Arif9878/common/go/tenant"
	"github.com/Arif9878/common/go/testkit"
)

func TestValid(t *testing.T) {
	for id, want := range map[string]bool{
		"acme": true, "org_42.eu-west:prod": true,
		"": false, "a b": false, "ü": false, "a,b": false, strings.Repeat("x", tenant.MaxLen+1): false,
	} {
		if got := tenant.Valid(id); got != want {
			t.Errorf("Valid(%q) = %v", id, got)
		}
	}
}

func TestNewContext(t *testing.T) {
	logger, logs := testkit.NewLogger(t)
	tp, spans := testkit.NewTracer(t)
	ctx, span := tp.Tracer("t").Start(context.Background(), "handle")

	ctx = tenant.NewContext(ctx, "acme")
	if id, ok := tenant.FromContext(ctx); !ok || id != "acme" {
		t.Errorf("FromContext = %q, %v", id, ok)
	}
	logger.InfoContext(ctx, "order created")
	span.End()

	if got := logs.Messages("order created"); len(got) != 1 || got[0]["tenant_id"] != "acme" {
		t.Errorf("log records = %v", got)
	}
	var found bool
	for _, a := range spans.Named("handle")[0].Attributes() {
		found = found || a == attribute.String("tenant.id", "acme")
	}
	if !found {
		t.Error("span has no tenant.id attribute")
	}

	// Outgoing calls carry it as baggage.
	h := http.Header{}
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(h))
	if !strings.Contains(h.Get("Baggage"), "tenant.id=acme") {
		t.Errorf("baggage header = %q", h.Get("Baggage"))
	}
}

func TestNewContextInvalid(t *testing.T) {
	ctx := context.Background()
	if got := tenant.NewContext(ctx, "a b"); got != ctx {
		t.Error("an invalid tenant changed the context")
	}
}

// TestBaggageIsNotTrusted checks that a caller's baggage alone does not
// set the tenant.
func TestBaggageIsNotTrusted(t *testing.T) {
	m, _ := baggage.NewMemberRaw(tenant.BaggageKey, "evil")
	b, _ := baggage.New(m)
	ctx := baggage.ContextWithBaggage(context.Background(), b)
	if id, ok := tenant.FromContext(ctx); ok {
		t.Errorf("FromContext trusted incoming baggage: %q", id)
	}
	if id, ok := tenant.BaggageValue(ctx); !ok || id != "evil" {
		t.Errorf("BaggageValue = %q, %v", id, ok)
	}
}

func TestMiddleware(t *testing.T) {
	var got string
	h := func(src tenant.Source) http.Handler {
		return tenant.Middleware(src)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got, _ = tenant.FromContext(r.Context())
		}))
	}
	serve := func(handler http.Handler, r *http.Request) string {
		got = ""
		handler.ServeHTTP(httptest.NewRecorder(), r)
		return got
	}
	req := func() *http.Request {
		return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	}

	r := req()
	r.Header.Set("X-Tenant-ID", "acme")
	if id := serve(h(tenant.FromHeader("X-Tenant-ID")), r); id != "acme" {
		t.Errorf("FromHeader: %q", id)
	}
	r = req()
	r.Header.Set("X-Tenant-ID", "not valid")
	if id := serve(h(tenant.FromHeader("X-Tenant-ID")), r); id != "" {
		t.Errorf("invalid header adopted: %q", id)
	}
	if id := serve(h(tenant.FromHeader("X-Tenant-ID")), req()); id != "" {
		t.Errorf("missing header: %q", id)
	}

	m, _ := baggage.NewMemberRaw(tenant.BaggageKey, "globex")
	b, _ := baggage.New(m)
	r = req().WithContext(baggage.ContextWithBaggage(context.Background(), b))
	if id := serve(h(tenant.FromBaggage()), r); id != "globex" {
		t.Errorf("FromBaggage: %q", id)
	}
}
