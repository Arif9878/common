package secret_test

import (
	"context"
	"fmt"

	"github.com/Arif9878/common/go/secret"
)

func Example() {
	// In production: a *vault.Client. Tests use secret.Static.
	var provider secret.Provider = secret.Static{
		"kv/data/payments/api": secret.New(map[string]string{"key": "sk_live_123"}),
	}

	s, err := provider.Get(context.Background(), "kv/data/payments/api")
	if err != nil {
		panic(err)
	}
	if err := s.Require("key"); err != nil {
		panic(err)
	}
	fmt.Println(len(s.Field("key")) > 0) // use the value
	fmt.Println(s)                       // printing shows field names, never values
	// Output:
	// true
	// secret(fields=[key], version="") [REDACTED]
}
