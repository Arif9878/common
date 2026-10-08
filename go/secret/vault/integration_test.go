//go:build integration

package vault_test

import (
	"context"
	"os"
	"testing"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/secret/vault"
)

// TestDevServer runs against a real Vault dev server:
//
//	vault server -dev -dev-root-token-id=root &
//	VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root vault kv put secret/it key=value
//	VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root go test -tags integration ./secret/vault/
func TestDevServer(t *testing.T) {
	addr, token := os.Getenv("VAULT_ADDR"), os.Getenv("VAULT_TOKEN")
	if addr == "" || token == "" {
		t.Skip("VAULT_ADDR and VAULT_TOKEN not set")
	}
	c, err := vault.New(context.Background(), vault.Config{Address: addr, Token: config.Secret(token)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()

	s, err := c.Get(context.Background(), "secret/data/it")
	if err != nil {
		t.Fatal(err)
	}
	if s.Field("key") != "value" || s.Version == "" {
		t.Errorf("fields %v version %q", s.Fields(), s.Version)
	}
}
