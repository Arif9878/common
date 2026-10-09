package tenant_test

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/tenant"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func Example() {
	var verifier *jwtauth.Verifier // from jwtauth.New

	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders", func(w http.ResponseWriter, r *http.Request) {
		id, ok := tenant.FromContext(r.Context())
		if !ok {
			http.Error(w, "no tenant", http.StatusForbidden)
			return
		}
		// Logged with tenant_id and user_id; calls to other services carry
		// the tenant as baggage.
		slog.InfoContext(r.Context(), "listing orders")
		_ = listOrders(r.Context(), id)
	})

	// The edge service trusts the tenant in the verified token…
	_ = httpserver.Chain(mux,
		httpserver.Auth(jwtauth.HTTP(verifier)),
		tenant.Middleware(jwtauth.ClaimValue("tenant_id")))

	// …an internal service reached only through such services trusts the
	// baggage they send.
	_ = tenant.Middleware(tenant.FromBaggage())(mux)
}

func listOrders(context.Context, string) error { return nil }
