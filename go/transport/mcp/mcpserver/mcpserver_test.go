package mcpserver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/mcp/mcpserver"
)

// issuer is a fake authorization server publishing one Ed25519 key.
type issuer struct {
	URL  string
	priv ed25519.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss := &issuer{priv: priv}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "OKP", "crv": "Ed25519", "kid": "k1", "alg": "EdDSA", "use": "sig",
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	}))
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	return iss
}

func (i *issuer) token(t *testing.T, audience, scope string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": i.URL, "sub": "user-1", "aud": audience, "scope": scope,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(i.priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type orderIn struct {
	ID string `json:"id" jsonschema:"the order ID"`
}

type order struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Owner  string `json:"owner"`
}

// fixture is a server with three tools behind an httptest server, its
// resource being that server's /mcp.
type fixture struct {
	iss      *issuer
	http     *httptest.Server
	srv      *mcpserver.Server
	resource string
}

func newFixture(t *testing.T, opts ...mcpserver.Option) *fixture {
	t.Helper()
	f := &fixture{iss: newIssuer(t)}
	mux := http.NewServeMux()
	f.http = httptest.NewServer(mux)
	t.Cleanup(f.http.Close)
	f.resource = f.http.URL + "/mcp"

	v, err := jwtauth.New(context.Background(), jwtauth.Config{Issuer: f.iss.URL, JWKSURL: f.iss.URL},
		jwtauth.WithAudiencePerCall())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close(context.Background()) })
	f.srv, err = mcpserver.New(mcpserver.Config{
		Name: "orders", Version: "1.0.0", Resource: f.resource, AuthorizationServers: []string{f.iss.URL},
	}, append([]mcpserver.Option{mcpserver.WithJWT(v), mcpserver.WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	mcpserver.AddTool(f.srv, mcpserver.Tool[orderIn, order]{
		Name: "get_order", Description: "Get one order.", Scopes: []string{"orders:read"}, ReadOnly: true,
		Handle: func(ctx context.Context, in orderIn) (order, error) {
			if in.ID != "o-1" {
				return order{}, errors.WithPublicMessage(errors.NotFound.New("order "+in.ID+" not in table orders"), "no such order")
			}
			c, _ := jwtauth.FromContext(ctx)
			return order{ID: in.ID, Status: "paid", Owner: c.Subject}, nil
		},
	})
	mcpserver.AddTool(f.srv, mcpserver.Tool[orderIn, order]{
		Name: "cancel_order", Description: "Cancel an order.", Scopes: []string{"orders:write"}, Destructive: true,
		Handle: func(context.Context, orderIn) (order, error) { panic("database exploded at 10.0.0.7") },
	})
	mux.Handle("/mcp", f.srv.Handler())
	mux.Handle(f.srv.MetadataPath(), f.srv.MetadataHandler())
	return f
}

// bearer adds a token to every request of an MCP client.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fixture) connect(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.resource, HTTPClient: &http.Client{Transport: bearer{token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolCall(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(t, f.iss.token(t, f.resource, "orders:read orders:write"))
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools.Tools))
	}
	for _, tl := range tools.Tools {
		if tl.Name == "get_order" && !tl.Annotations.ReadOnlyHint {
			t.Error("get_order is not marked read-only")
		}
		if tl.Name == "cancel_order" && (tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint) {
			t.Error("cancel_order is not marked destructive")
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_order", Arguments: map[string]any{"id": "o-1"}})
	if err != nil || res.IsError {
		t.Fatalf("get_order: %v %s", err, text(res))
	}
	var got order
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &got)
	if got != (order{ID: "o-1", Status: "paid", Owner: "user-1"}) {
		t.Errorf("get_order = %+v; the handler must see the caller", got)
	}

	// Errors carry the kind and the public message only.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_order", Arguments: map[string]any{"id": "o-2"}})
	if err != nil || !res.IsError || text(res) != "not_found: no such order" {
		t.Errorf("missing order: err = %v, result = %q", err, text(res))
	}
	// A panic is an internal error, without its text.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "cancel_order", Arguments: map[string]any{"id": "o-1"}})
	if err != nil || !res.IsError || strings.Contains(text(res), "10.0.0.7") || !strings.HasPrefix(text(res), "internal: ") {
		t.Errorf("panic: err = %v, result = %q", err, text(res))
	}
	// Input is validated against the schema before the handler runs.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_order", Arguments: map[string]any{"id": 7}})
	if err != nil || !res.IsError {
		t.Errorf("invalid input: err = %v, result = %q", err, text(res))
	}
}

// post sends one JSON-RPC message as the 2026-07-28 protocol does; the
// caller closes the body.
func (f *fixture) post(t *testing.T, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, f.resource, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthorization(t *testing.T) {
	f := newFixture(t)
	metadata := f.http.URL + "/.well-known/oauth-protected-resource/mcp"
	call := func(tool string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"id":"o-1"}}}`, tool)
	}

	for _, tc := range []struct {
		name, token, body string
		status            int
		challenge         string
	}{
		{"no token", "", call("get_order"), 401, `Bearer resource_metadata="` + metadata + `"`},
		{"garbage token", "x.y.z", call("get_order"), 401, `error="invalid_token"`},
		{"token for another resource", f.iss.token(t, "https://other.example/mcp", "orders:read"), call("get_order"), 401, `error="invalid_token"`},
		{"missing scope", f.iss.token(t, f.resource, "orders:read"), call("cancel_order"), 403, `error="insufficient_scope", scope="orders:write"`},
		{"granted", f.iss.token(t, f.resource, "orders:read"), call("get_order"), 200, ""},
		{"unknown tool is the SDK's to answer", f.iss.token(t, f.resource, ""), call("nope"), 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.post(t, tc.token, tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.status {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, b)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, tc.challenge) {
				t.Errorf("WWW-Authenticate = %q, want it to contain %q", got, tc.challenge)
			}
		})
	}
}

func TestEnabled(t *testing.T) {
	var on atomic.Bool
	f := newFixture(t, mcpserver.WithEnabled(func(context.Context) bool { return on.Load() }))
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_order","arguments":{"id":"o-1"}}}`
	status := func(token string) (int, string) {
		resp := f.post(t, token, call)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, resp.Header.Get("Retry-After")
	}
	// Off: refused before the token is looked at, valid or not.
	if code, retry := status(f.iss.token(t, f.resource, "orders:read")); code != http.StatusServiceUnavailable || retry != "60" {
		t.Errorf("off, with a token: %d, Retry-After %q", code, retry)
	}
	if code, _ := status(""); code != http.StatusServiceUnavailable {
		t.Errorf("off, without a token: %d", code)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.http.URL+f.srv.MetadataPath(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("metadata while off: %d", resp.StatusCode)
	}
	// On again, at once.
	on.Store(true)
	if code, _ := status(f.iss.token(t, f.resource, "orders:read")); code != http.StatusOK {
		t.Errorf("on: %d", code)
	}
	if code, _ := status(""); code != http.StatusUnauthorized {
		t.Errorf("on, without a token: %d", code)
	}
}

func TestMetadata(t *testing.T) {
	f := newFixture(t)
	if got := f.srv.MetadataPath(); got != "/.well-known/oauth-protected-resource/mcp" {
		t.Errorf("MetadataPath = %q", got)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.http.URL+f.srv.MetadataPath(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
		ResourceName         string   `json:"resource_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Resource != f.resource || len(m.AuthorizationServers) != 1 || m.AuthorizationServers[0] != f.iss.URL ||
		strings.Join(m.ScopesSupported, " ") != "orders:read orders:write" || m.ResourceName != "orders" {
		t.Errorf("metadata = %+v", m)
	}
}

func TestNewChecksConfig(t *testing.T) {
	auth := mcpserver.WithAuth(func(ctx context.Context, _ string) (context.Context, error) { return ctx, nil })
	for name, tc := range map[string]struct {
		cfg  mcpserver.Config
		opts []mcpserver.Option
	}{
		"no name":                {cfg: mcpserver.Config{Version: "1", Resource: "https://api.example.com/mcp"}},
		"relative resource":      {cfg: mcpserver.Config{Name: "a", Version: "1", Resource: "/mcp"}},
		"http resource":          {cfg: mcpserver.Config{Name: "a", Version: "1", Resource: "http://api.example.com/mcp"}},
		"resource with fragment": {cfg: mcpserver.Config{Name: "a", Version: "1", Resource: "https://api.example.com/mcp#x"}},
		"auth without AS":        {cfg: mcpserver.Config{Name: "a", Version: "1", Resource: "https://api.example.com/mcp"}, opts: []mcpserver.Option{auth}},
		"AS that is not a URL":   {cfg: mcpserver.Config{Name: "a", Version: "1", Resource: "https://api.example.com/mcp", AuthorizationServers: []string{"auth"}}, opts: []mcpserver.Option{auth}},
	} {
		if _, err := mcpserver.New(tc.cfg, tc.opts...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for _, resource := range []string{"https://api.example.com/mcp", "http://localhost:8080/mcp", "http://127.0.0.1/mcp"} {
		if _, err := mcpserver.New(mcpserver.Config{Name: "a", Version: "1", Resource: resource}); err != nil {
			t.Errorf("%s: %v", resource, err)
		}
	}
}

func TestAddToolPanics(t *testing.T) {
	srv, err := mcpserver.New(mcpserver.Config{Name: "a", Version: "1", Resource: "https://api.example.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	handle := func(context.Context, orderIn) (order, error) { return order{}, nil }
	mcpserver.AddTool(srv, mcpserver.Tool[orderIn, order]{Name: "get_order", Handle: handle})
	for name, tool := range map[string]mcpserver.Tool[orderIn, order]{
		"no name":     {Handle: handle},
		"no handler":  {Name: "x"},
		"duplicate":   {Name: "get_order", Handle: handle},
		"space scope": {Name: "y", Handle: handle, Scopes: []string{"orders:read orders:write"}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			mcpserver.AddTool(srv, tool)
		}()
	}
}

func TestWithoutAuth(t *testing.T) {
	srv, err := mcpserver.New(mcpserver.Config{Name: "a", Version: "1", Resource: "http://localhost/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	mcpserver.AddTool(srv, mcpserver.Tool[orderIn, order]{Name: "scoped", Scopes: []string{"orders:read"},
		Handle: func(context.Context, orderIn) (order, error) { return order{ID: "leak"}, nil }})
	h := httptest.NewServer(srv.Handler())
	defer h.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	// A scoped tool fails closed when nothing authenticates.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "scoped", Arguments: map[string]any{"id": "1"}})
	if err != nil || !res.IsError || !strings.HasPrefix(text(res), "unauthorized: ") {
		t.Errorf("scoped tool without auth: err = %v, result = %q", err, text(res))
	}
	// And there is no metadata to serve.
	rec := httptest.NewRecorder()
	srv.MetadataHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, srv.MetadataPath(), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("metadata without auth: %d", rec.Code)
	}
}
