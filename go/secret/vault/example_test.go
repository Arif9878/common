package vault_test

import (
	"context"
	"log"

	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
	"github.com/Arif9878/common/go/secret/vault"
)

type dbPool struct{}

func (*dbPool) Close() {}

func connect(context.Context, secret.Secret) (*dbPool, error) { return &dbPool{}, nil }

func Example() {
	ctx := context.Background()
	shutdown := graceful.New()

	// In Kubernetes: vault.WithAuth(kubernetesAuth) from
	// github.com/hashicorp/vault/api/auth/kubernetes.
	v, err := vault.New(ctx, vault.Config{Address: "https://vault.internal:8200"})
	if err != nil {
		log.Fatal(err)
	}

	// Dynamic database credentials, rotated before the lease expires.
	pools, err := rotation.New(ctx, "orders-db", rotation.Spec[*dbPool]{
		Fetch: v.Fetcher("database/creds/orders"),
		Build: connect,
		Close: func(ctx context.Context, p *dbPool, s secret.Secret) error {
			p.Close()
			return v.Revoke(ctx, s)
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Both close in CloseDeps; the Vault client does not revoke its token,
	// so leases stay valid while pools drain.
	_ = shutdown.Register(graceful.CloseDeps, "orders-db", pools.Close)
	_ = shutdown.Register(graceful.CloseDeps, "vault", v.Close)
}
