package oauth2client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/auth/oauth2client"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/testkit"
)

// provider is a fake token endpoint.
type provider struct {
	*httptest.Server
	requests  atomic.Int32
	expiresIn atomic.Int32
	status    atomic.Int32 // 0: issue tokens
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{}
	p.expiresIn.Store(3600)
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := p.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if st := p.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			code := map[int32]string{400: "invalid_scope", 401: "invalid_client"}[st]
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
			return
		}
		user, pass, ok := r.BasicAuth()
		_ = r.ParseForm()
		if !ok || user != "orders" || pass != "s3cret" || r.Form.Get("grant_type") != "client_credentials" ||
			r.Form.Get("scope") != "payments:write payments:read" || r.Form.Get("audience") != "payments-api" {
			t.Errorf("token request: user %q ok %v form %v", user, ok, r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-" + string(rune('0'+n)), "token_type": "Bearer", "expires_in": p.expiresIn.Load(),
		})
	}))
	t.Cleanup(p.Close)
	return p
}

func newSource(p *provider, early time.Duration, opts ...oauth2client.Option) *oauth2client.Source {
	return oauth2client.New(oauth2client.Config{
		TokenURL: p.URL, ClientID: "orders", ClientSecret: "s3cret",
		Scopes: []string{"payments:write", "payments:read"}, Audience: "payments-api", EarlyExpiry: early,
	}, opts...)
}

func TestCachesAndRefreshesEarly(t *testing.T) {
	p := newProvider(t)
	mp, metrics := testkit.NewMetrics(t)
	src := newSource(p, time.Second, oauth2client.WithMeterProvider(mp), oauth2client.WithName("payments"))
	for range 5 {
		tok, err := src.Token()
		if err != nil || tok.AccessToken != "tok-1" {
			t.Fatalf("token = %v, %v", tok, err)
		}
	}
	if n := p.requests.Load(); n != 1 {
		t.Errorf("%d token requests for 5 calls, want 1", n)
	}

	// A token expiring within EarlyExpiry is replaced before use.
	p.expiresIn.Store(2)
	src = newSource(p, time.Second)
	first, _ := src.Token()
	time.Sleep(1100 * time.Millisecond)
	second, _ := src.Token()
	if first.AccessToken == second.AccessToken {
		t.Errorf("token %s reused within EarlyExpiry of its expiry", first.AccessToken)
	}
	if n := metrics.Sum("oauth2.token.requests", attribute.String("client", "payments"), attribute.String("outcome", "ok")); n != 1 {
		t.Errorf("oauth2.token.requests{payments,ok} = %v", n)
	}
}

func TestAttachesTokens(t *testing.T) {
	p := newProvider(t)
	src := newSource(p, time.Second)
	var got string
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
	defer api.Close()

	client := &http.Client{Transport: src.Transport(nil)}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, api.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "Bearer tok-1" {
		t.Errorf("Authorization = %q", got)
	}

	md, err := src.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer tok-1" {
		t.Errorf("gRPC metadata = %v, %v", md, err)
	}
	if !src.RequireTransportSecurity() || newSource(p, 0, oauth2client.WithInsecureTransport()).RequireTransportSecurity() {
		t.Error("RequireTransportSecurity: TLS must be required unless WithInsecureTransport")
	}
}

func TestErrors(t *testing.T) {
	for status, want := range map[int32]errors.Kind{401: errors.Unauthorized, 400: errors.InvalidArgument, 503: errors.Unavailable} {
		p := newProvider(t)
		p.status.Store(status)
		src := newSource(p, time.Second)
		if _, err := src.Token(); errors.KindOf(err) != want {
			t.Errorf("status %d: err = %v, want %v", status, err, want)
		}
	}

	// A request that cannot get a token is not sent.
	p := newProvider(t)
	p.status.Store(401)
	sent := false
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent = true }))
	defer api.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, api.URL, strings.NewReader("body"))
	resp, err := (&http.Client{Transport: newSource(p, 0).Transport(nil)}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if err == nil || sent {
		t.Errorf("request without a token: err %v, sent %v", err, sent)
	}
}
