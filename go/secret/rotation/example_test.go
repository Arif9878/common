package rotation_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
)

// apiClient stands for anything built from a credential: a database pool,
// a Redis client, an API client.
type apiClient struct{ user string }

func Example() {
	ctx := context.Background()
	n := 0
	fetch := func(context.Context) (secret.Secret, error) { // in production: vault.Client.Fetcher
		n++
		return secret.New(map[string]string{"username": fmt.Sprintf("svc-%d", n)}), nil
	}

	r, err := rotation.New(ctx, "partner-api", rotation.Spec[*apiClient]{
		Fetch: fetch,
		Build: func(_ context.Context, s secret.Secret) (*apiClient, error) {
			return &apiClient{user: s.Field("username")}, nil
		},
	}, rotation.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close(ctx) }()

	fmt.Println(r.Current().user)
	_ = r.Rotate(ctx) // also happens before the credential's TTL runs out
	fmt.Println(r.Current().user)
	// Output:
	// svc-1
	// svc-2
}
