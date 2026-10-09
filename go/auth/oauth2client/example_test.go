package oauth2client_test

import (
	"log"
	"net/http"

	"github.com/Arif9878/common/go/auth/oauth2client"
	"github.com/Arif9878/common/go/config"
)

func Example() {
	// PAYMENTS_AUTH_TOKEN_URL=https://keycloak.example.com/realms/main/protocol/openid-connect/token
	// PAYMENTS_AUTH_CLIENT_ID=orders
	// PAYMENTS_AUTH_CLIENT_SECRET=…
	// PAYMENTS_AUTH_SCOPES=payments:charge
	cfg, err := config.Load[oauth2client.Config](config.WithPrefix("PAYMENTS_AUTH_"))
	if err != nil {
		log.Fatal(err)
	}
	tokens := oauth2client.New(cfg, oauth2client.WithName("payments"))

	// HTTP: every request carries a cached token, refreshed before it expires.
	payments := &http.Client{Transport: tokens.Transport(http.DefaultTransport)}
	_ = payments

	// gRPC: Source is a credentials.PerRPCCredentials, so pass
	// grpc.WithPerRPCCredentials(tokens) to grpcclient.WithDialOptions.
}
