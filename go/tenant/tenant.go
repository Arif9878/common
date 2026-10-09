// Package tenant carries the tenant a unit of work belongs to through a
// context.Context, into logs and traces, and on to downstream services.
//
//	// The edge service takes the tenant from the verified token…
//	api := tenant.Middleware(jwtauth.ClaimValue("tenant_id"))(mux) // after httpserver.Auth
//
//	// …and handlers read it.
//	id, ok := tenant.FromContext(r.Context())
//
// [NewContext], which the middleware calls, makes the tenant visible
// everywhere the request goes:
//
//   - logs: every record logged with the context gets tenant_id;
//   - traces: the current span gets the tenant.id attribute;
//   - downstream: the tenant.id W3C baggage member, which the standard HTTP,
//     gRPC and Kafka clients send along with the trace context.
//
// # Trust
//
// [FromContext] returns only a tenant this service set with NewContext. A
// tenant in incoming baggage or headers comes from the caller, who can send
// anything, so a service adopts it only by choosing a [Source]:
// jwtauth.ClaimValue (a claim of the verified token) at the edge, [FromBaggage] in internal
// services reached only through services that set it themselves, and
// [FromHeader] behind a gateway that overwrites the header. Kafka
// consumers adopt the producer's tenant with kafka.WithTenant.
//
// # Metrics
//
// The tenant is not added to metrics: one series per tenant multiplies
// every metric by the number of tenants. Add it yourself to the few
// metrics where per-tenant numbers are worth that.
package tenant

import (
	"context"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/observability/logging"
)

// BaggageKey is the W3C baggage member carrying the tenant to downstream
// services, and the span attribute naming it.
const BaggageKey = "tenant.id"

// MaxLen is the longest tenant ID accepted.
const MaxLen = 128

type ctxKey struct{}

// Valid reports whether id is a usable tenant ID: 1 to MaxLen characters
// among letters, digits and - _ . :
func Valid(id string) bool {
	if id == "" || len(id) > MaxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !allowed(id[i]) {
			return false
		}
	}
	return true
}

func allowed(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return c == '-' || c == '_' || c == '.' || c == ':'
}

// NewContext returns a copy of ctx belonging to tenant id: FromContext
// returns it, logs carry tenant_id, the current span gets the tenant.id
// attribute and outgoing calls carry it as baggage. An invalid id returns
// ctx unchanged.
func NewContext(ctx context.Context, id string) context.Context {
	if !Valid(id) {
		return ctx
	}
	ctx = context.WithValue(ctx, ctxKey{}, id)
	if m, err := baggage.NewMemberRaw(BaggageKey, id); err == nil {
		if b, err := baggage.FromContext(ctx).SetMember(m); err == nil {
			ctx = baggage.ContextWithBaggage(ctx, b)
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(BaggageKey, id))
	return logging.ContextWithAttrs(ctx, slog.String(logging.KeyTenantID, id))
}

// FromContext returns the tenant set with NewContext.
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

// BaggageValue returns the tenant in ctx's incoming baggage, if valid. It is
// what the caller claims; see the package documentation before trusting it.
func BaggageValue(ctx context.Context) (string, bool) {
	id := baggage.FromContext(ctx).Member(BaggageKey).Value()
	return id, Valid(id)
}

// Source finds the tenant of an HTTP request.
type Source func(r *http.Request) (string, bool)

// FromBaggage reads the tenant from the request's incoming baggage, which
// httpserver's handler extracts with the trace context.
func FromBaggage() Source {
	return func(r *http.Request) (string, bool) { return BaggageValue(r.Context()) }
}

// FromHeader reads the tenant from a request header, such as one a gateway
// sets after authenticating the caller.
func FromHeader(name string) Source {
	return func(r *http.Request) (string, bool) {
		id := r.Header.Get(name)
		return id, id != ""
	}
}

// Middleware puts the tenant src finds into the request's context with
// NewContext. Requests without one, or with an invalid one, pass through
// without a tenant; handlers that need it check FromContext.
func Middleware(src Source) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id, ok := src(r); ok && Valid(id) {
				r = r.WithContext(NewContext(r.Context(), id))
			}
			next.ServeHTTP(w, r)
		})
	}
}
