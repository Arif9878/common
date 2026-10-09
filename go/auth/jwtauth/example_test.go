package jwtauth_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func Example() {
	ctx := context.Background()
	shutdown := graceful.New()

	// AUTH_ISSUER=https://keycloak.example.com/realms/main
	// AUTH_AUDIENCE=orders-api
	cfg, err := config.Load[jwtauth.Config](config.WithPrefix("AUTH_"))
	if err != nil {
		log.Fatal(err)
	}
	v, err := jwtauth.New(ctx, cfg) // fetches the issuer's keys
	if err != nil {
		log.Fatal(err)
	}
	_ = shutdown.Register(graceful.CloseDeps, "jwks", v.Close)

	mux := http.NewServeMux()
	mux.Handle("GET /orders/{id}", httpserver.Chain(http.HandlerFunc(getOrder), jwtauth.RequireScope("orders:read")))
	handler := httpserver.Chain(mux, httpserver.Auth(jwtauth.HTTP(v)))
	_ = handler
}

func getOrder(w http.ResponseWriter, r *http.Request) {
	claims, _ := jwtauth.FromContext(r.Context())
	_ = json.NewEncoder(w).Encode(map[string]string{"id": r.PathValue("id"), "customer": claims.Subject})
}
