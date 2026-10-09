package vault_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/testkit"

	"github.com/hashicorp/vault/api"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
	"github.com/Arif9878/common/go/secret/vault"
)

// fakeVault implements the parts of the Vault HTTP API the client uses.
type fakeVault struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	token        string
	tokenTTL     int // seconds
	loginStatus  int
	renewFails   bool
	logins       atomic.Int32
	renewals     atomic.Int32
	leaseSeq     atomic.Int32
	revoked      []string
	lookupTTL    int
	lookupRenews bool
	sealed       bool
}

func newFakeVault(t *testing.T) *fakeVault {
	f := &fakeVault{t: t, tokenTTL: 3600, loginStatus: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")

	if path == "sys/health" {
		status := 200
		if f.sealed {
			status = 503
		}
		f.writeJSON(w, status, map[string]any{"initialized": true, "sealed": f.sealed})
		return
	}
	if path == "auth/test/login" {
		f.logins.Add(1)
		if f.loginStatus != 200 {
			f.writeJSON(w, f.loginStatus, map[string]any{"errors": []string{"permission denied"}})
			return
		}
		f.token = "tok-" + strconv.Itoa(int(f.logins.Load()))
		f.writeJSON(w, 200, map[string]any{"auth": map[string]any{
			"client_token": f.token, "lease_duration": f.tokenTTL, "renewable": true,
		}})
		return
	}
	if r.Header.Get("X-Vault-Token") != f.token {
		f.writeJSON(w, 403, map[string]any{"errors": []string{"permission denied"}})
		return
	}

	switch path {
	case "auth/token/lookup-self":
		f.writeJSON(w, 200, map[string]any{"data": map[string]any{"ttl": f.lookupTTL, "renewable": f.lookupRenews}})
	case "auth/token/renew-self":
		f.renewals.Add(1)
		if f.renewFails {
			f.writeJSON(w, 403, map[string]any{"errors": []string{"token expired"}})
			return
		}
		f.writeJSON(w, 200, map[string]any{"auth": map[string]any{
			"client_token": f.token, "lease_duration": f.tokenTTL, "renewable": true,
		}})
	case "secret/data/app":
		f.writeJSON(w, 200, map[string]any{"data": map[string]any{
			"data":     map[string]any{"api_key": "sk-live-123", "limits": map[string]any{"rps": 10}},
			"metadata": map[string]any{"version": 3},
		}})
	case "secret/data/deleted":
		f.writeJSON(w, 200, map[string]any{"data": map[string]any{
			"data": nil, "metadata": map[string]any{"version": 2, "deletion_time": "2026-01-01T00:00:00Z"},
		}})
	case "database/creds/orders":
		n := f.leaseSeq.Add(1)
		f.writeJSON(w, 200, map[string]any{
			"lease_id":       "database/creds/orders/lease" + strconv.Itoa(int(n)),
			"lease_duration": 60,
			"renewable":      true,
			"data":           map[string]any{"username": "v-orders-" + strconv.Itoa(int(n)), "password": "pw-" + strconv.Itoa(int(n))},
		})
	case "sys/leases/renew":
		var body struct {
			LeaseID string `json:"lease_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.writeJSON(w, 200, map[string]any{"lease_id": body.LeaseID, "lease_duration": 120, "renewable": true})
	case "sys/leases/revoke":
		var body struct {
			LeaseID string `json:"lease_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.revoked = append(f.revoked, body.LeaseID)
		w.WriteHeader(204)
	case "secret/forbidden":
		f.writeJSON(w, 403, map[string]any{"errors": []string{"1 error occurred: permission denied"}})
	case "secret/sealed":
		f.writeJSON(w, 503, map[string]any{"errors": []string{"Vault is sealed"}})
	case "secret/proxy":
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "<html>upstream error, debug token=LEAKED-BODY</html>")
	default:
		f.writeJSON(w, 404, map[string]any{"errors": []string{}})
	}
}

type testAuth struct{}

func (testAuth) Login(ctx context.Context, c *api.Client) (*api.Secret, error) {
	return c.Logical().WriteWithContext(ctx, "auth/test/login", nil)
}

var quiet = vault.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

func newClient(t *testing.T, f *fakeVault, opts ...vault.Option) *vault.Client {
	t.Helper()
	c, err := vault.New(context.Background(), vault.Config{Address: f.srv.URL, MaxRetries: 0},
		append([]vault.Option{quiet, vault.WithAuth(testAuth{})}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestReadKVv2(t *testing.T) {
	f := newFakeVault(t)
	c := newClient(t, f)

	s, err := c.Get(context.Background(), "secret/data/app")
	if err != nil {
		t.Fatal(err)
	}
	if s.Field("api_key") != "sk-live-123" || s.Field("limits") != `{"rps":10}` || s.Version != "3" {
		t.Errorf("secret fields %v version %q", s.Fields(), s.Version)
	}
	if !s.ExpiresAt.IsZero() || s.LeaseID != "" {
		t.Error("KV secret has a lease")
	}

	if _, err := c.Get(context.Background(), "secret/data/deleted"); errors.KindOf(err) != errors.NotFound {
		t.Errorf("deleted version: %v", err)
	}
	if _, err := c.Get(context.Background(), "secret/data/nope"); errors.KindOf(err) != errors.NotFound {
		t.Errorf("missing path: %v", err)
	}

	var p secret.Provider = c // implements the provider interface
	_ = p
}

func TestReadDynamicSecret(t *testing.T) {
	f := newFakeVault(t)
	c := newClient(t, f)

	s, err := c.Get(context.Background(), "database/creds/orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Require("username", "password"); err != nil {
		t.Fatal(err)
	}
	if s.LeaseID != "database/creds/orders/lease1" || s.TTL() <= 55*time.Second || s.TTL() > 60*time.Second {
		t.Errorf("lease %q ttl %v", s.LeaseID, s.TTL())
	}

	renewed, err := c.RenewLease(context.Background(), s, 2*time.Minute)
	if err != nil || renewed.TTL() <= 115*time.Second {
		t.Errorf("RenewLease = %v ttl, %v", renewed.TTL(), err)
	}
	if _, err := c.RenewLease(context.Background(), secret.Secret{}, time.Minute); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("renew without lease: %v", err)
	}

	if err := c.Revoke(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(context.Background(), secret.Secret{}); err != nil {
		t.Errorf("revoke without lease: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.revoked) != 1 || f.revoked[0] != s.LeaseID {
		t.Errorf("revoked = %v", f.revoked)
	}
}

func TestErrorClassification(t *testing.T) {
	f := newFakeVault(t)
	c := newClient(t, f)

	tests := map[string]struct {
		kind    errors.Kind
		message string
	}{
		"secret/forbidden": {errors.Forbidden, "permission denied"},
		"secret/sealed":    {errors.Unavailable, "Vault is sealed"},
		"secret/proxy":     {errors.Unavailable, "status 502"},
	}
	for path, want := range tests {
		_, err := c.Get(context.Background(), path)
		if errors.KindOf(err) != want.kind || !strings.Contains(err.Error(), want.message) {
			t.Errorf("%s: %v (kind %v)", path, err, errors.KindOf(err))
		}
		if strings.Contains(err.Error(), "LEAKED-BODY") {
			t.Errorf("%s: response body in error: %v", path, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "secret/data/app"); errors.KindOf(err) != errors.Canceled {
		t.Errorf("canceled: %v (kind %v)", err, errors.KindOf(err))
	}

	f.srv.Close()
	if _, err := c.Get(context.Background(), "secret/data/app"); errors.KindOf(err) != errors.Unavailable {
		t.Errorf("unreachable: %v (kind %v)", err, errors.KindOf(err))
	}
}

func TestNewFailures(t *testing.T) {
	f := newFakeVault(t)
	f.loginStatus = 403
	_, err := vault.New(context.Background(), vault.Config{Address: f.srv.URL}, quiet, vault.WithAuth(testAuth{}))
	if errors.KindOf(err) != errors.Forbidden {
		t.Errorf("failed login: %v", err)
	}
	_, err = vault.New(context.Background(), vault.Config{Address: f.srv.URL}, quiet)
	if errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("no credentials: %v", err)
	}
}

func TestStaticToken(t *testing.T) {
	f := newFakeVault(t)
	f.token = "static-root"
	c, err := vault.New(context.Background(), vault.Config{Address: f.srv.URL, Token: config.Secret("static-root")}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if _, err := c.Get(context.Background(), "secret/data/app"); err != nil {
		t.Fatal(err)
	}
	if f.logins.Load() != 0 {
		t.Error("static token client logged in")
	}
}

func TestTokenRenewalAndRelogin(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real time")
	}
	f := newFakeVault(t)
	f.tokenTTL = 2
	c := newClient(t, f, vault.WithReloginBackoff(100*time.Millisecond, 100*time.Millisecond))

	testkit.Eventually(t, 5*time.Second, "a token renewal", func() bool { return f.renewals.Load() > 0 })

	f.mu.Lock()
	f.renewFails = true
	f.mu.Unlock()
	testkit.Eventually(t, 10*time.Second, "a new login after renewal stopped working",
		func() bool { return f.logins.Load() >= 2 })
	// The new token works. The fake replaces its token when the login
	// request arrives, before the client has read the response, so wait
	// for the client to switch.
	testkit.Eventually(t, 5*time.Second, "a read with the new token", func() bool {
		_, err := c.Get(context.Background(), "secret/data/app")
		return err == nil
	})
}

func TestRotationWithDynamicCredentials(t *testing.T) {
	f := newFakeVault(t)
	c := newClient(t, f)

	type db struct{ user string }
	r, err := rotation.New(context.Background(), "orders-db", rotation.Spec[*db]{
		Fetch: c.Fetcher("database/creds/orders"),
		Build: func(_ context.Context, s secret.Secret) (*db, error) { return &db{user: s.Field("username")}, nil },
		Close: func(ctx context.Context, _ *db, s secret.Secret) error { return c.Revoke(ctx, s) },
	}, rotation.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if r.Current().user != "v-orders-1" {
		t.Fatalf("user = %s", r.Current().user)
	}
	if err := r.Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.revoked, ",") != "database/creds/orders/lease1,database/creds/orders/lease2" {
		t.Errorf("revoked = %v", f.revoked)
	}
}

func TestObservability(t *testing.T) {
	f := newFakeVault(t)
	logger, logs := testkit.NewLogger(t)
	mp, m := testkit.NewMetrics(t)
	c := newClient(t, f, vault.WithLogger(logger), vault.WithMeterProvider(mp))

	s, _ := c.Get(context.Background(), "database/creds/orders")
	_, _ = c.Get(context.Background(), "secret/forbidden")
	logger.Info("debug", "secret", s)
	if logs.Contains("pw-1") || logs.Contains("tok-") {
		t.Fatalf("secret or token logged:\n%s", logs)
	}

	calls := func(op, outcome string) float64 {
		return m.Sum("vault.requests", attribute.String("operation", op), attribute.String("outcome", outcome))
	}
	if calls("login", "ok") != 1 || calls("read", "ok") != 1 || calls("read", "forbidden") != 1 {
		t.Errorf("vault.requests: login/ok %v, read/ok %v, read/forbidden %v",
			calls("login", "ok"), calls("read", "ok"), calls("read", "forbidden"))
	}
	if ttl := m.Gauge("vault.token.ttl"); ttl < 3500 || ttl > 3600 {
		t.Errorf("vault.token.ttl = %v", ttl)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newFakeVault(t)
	c := newClient(t, f)
	for range 2 {
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c.API() == nil {
		t.Fatal("API() returned nil")
	}
}

func TestHealthCheck(t *testing.T) {
	f := newFakeVault(t)
	c := newClient(t, f)
	if err := c.HealthCheck(context.Background()); err != nil {
		t.Fatalf("healthy: %v", err)
	}
	f.mu.Lock()
	f.sealed = true
	f.mu.Unlock()
	if err := c.HealthCheck(context.Background()); errors.KindOf(err) != errors.Unavailable {
		t.Errorf("sealed: err = %v, want unavailable", err)
	}
}
