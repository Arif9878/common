package jwtauth_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/testkit"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

const audience = "orders-api"

// signer is a key the fake identity provider signs with.
type signer struct {
	kid    string
	method jwt.SigningMethod
	priv   crypto.Signer
	jwkAlg string // "alg" published in the JWKS, if any
}

func newRSA(t *testing.T, kid string) signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, method: jwt.SigningMethodRS256, priv: k}
}

func newEC(t *testing.T, kid string) signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, method: jwt.SigningMethodES256, priv: k}
}

func newEd(t *testing.T, kid string) signer {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, method: jwt.SigningMethodEdDSA, priv: k}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s signer) jwk() map[string]any {
	m := map[string]any{"kid": s.kid, "use": "sig"}
	if s.jwkAlg != "" {
		m["alg"] = s.jwkAlg
	}
	switch k := s.priv.Public().(type) {
	case *rsa.PublicKey:
		m["kty"], m["n"], m["e"] = "RSA", b64(k.N.Bytes()), b64(big.NewInt(int64(k.E)).Bytes())
	case *ecdsa.PublicKey:
		ecdh, _ := k.ECDH()
		raw := ecdh.Bytes() // 0x04 || X || Y
		m["kty"], m["crv"], m["x"], m["y"] = "EC", "P-256", b64(raw[1:33]), b64(raw[33:])
	case ed25519.PublicKey:
		m["kty"], m["crv"], m["x"] = "OKP", "Ed25519", b64(k)
	}
	return m
}

// sign makes a token; claims override the defaults (a valid token).
func (s signer) sign(t *testing.T, issuer string, claims jwt.MapClaims) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": issuer, "aud": audience, "sub": "user-7",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(24 * time.Hour).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	tok := jwt.NewWithClaims(s.method, c)
	if s.kid != "" {
		tok.Header["kid"] = s.kid
	}
	out, err := tok.SignedString(s.priv)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// idp is a fake identity provider: OpenID configuration and JWKS.
type idp struct {
	URL        string
	mu         sync.Mutex
	signers    []signer
	encryption []signer // published with "use": "enc"
	down       bool
	fetches    atomic.Int32
}

func newIDP(t *testing.T, signers ...signer) *idp {
	t.Helper()
	p := &idp{signers: signers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"): // for any issuer path
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.URL, "jwks_uri": p.URL + "/keys"})
		case r.URL.Path == "/keys":
			p.fetches.Add(1)
			keys := []map[string]any{{"kty": "RSA", "use": "enc", "kid": "malformed"}}
			for _, s := range p.signers {
				keys = append(keys, s.jwk())
			}
			for _, s := range p.encryption {
				k := s.jwk()
				k["use"] = "enc"
				keys = append(keys, k)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

func (p *idp) set(down bool, signers ...signer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if signers != nil {
		p.signers = signers
	}
}

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func newVerifier(t *testing.T, p *idp, mutate func(*jwtauth.Config), opts ...jwtauth.Option) *jwtauth.Verifier {
	t.Helper()
	cfg := jwtauth.Config{Issuer: p.URL, Audience: []string{"other-api", audience},
		Leeway: time.Second, RefreshInterval: time.Hour, MinRefreshInterval: 30 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	logger, _ := testkit.NewLogger(t)
	v, err := jwtauth.New(context.Background(), cfg, append([]jwtauth.Option{jwtauth.WithLogger(logger)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close(context.Background()) })
	return v
}

func TestVerifiesRSAECAndEdDSA(t *testing.T) {
	rsaKey, ecKey, edKey := newRSA(t, "rsa-1"), newEC(t, "ec-1"), newEd(t, "ed-1")
	p := newIDP(t, rsaKey, ecKey, edKey)
	v := newVerifier(t, p, nil)
	ctx := context.Background()

	for _, s := range []signer{rsaKey, ecKey, edKey} {
		c, err := v.Verify(ctx, s.sign(t, p.URL, jwt.MapClaims{"scope": "orders:read orders:write"}))
		if err != nil {
			t.Fatalf("%s: %v", s.method.Alg(), err)
		}
		if c.Subject != "user-7" || c.Issuer != p.URL || !c.HasScope("orders:write") || c.HasScope("admin") {
			t.Errorf("%s: claims %+v", s.method.Alg(), c)
		}
	}
	// Azure AD style "scp" list, and provider-specific claims.
	c, err := v.Verify(ctx, rsaKey.sign(t, p.URL, jwt.MapClaims{"scp": []string{"orders:read"}, "email": "a@b.c"}))
	if err != nil || !c.HasScope("orders:read") {
		t.Fatalf("scp: %+v %v", c, err)
	}
	if email, _ := c.Get("email"); email != "a@b.c" {
		t.Errorf("email = %v", email)
	}
}

func TestRejectsInvalidTokens(t *testing.T) {
	rsaKey, ecKey := newRSA(t, "rsa-1"), newEC(t, "ec-1")
	p := newIDP(t, rsaKey, ecKey)
	mp, metrics := testkit.NewMetrics(t)
	v := newVerifier(t, p, nil, jwtauth.WithMeterProvider(mp))
	ctx := context.Background()

	// An HMAC token keyed with the RSA public key: the classic algorithm
	// confusion attack.
	pubDER, _ := x509.MarshalPKIXPublicKey(rsaKey.priv.Public())
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": p.URL, "aud": audience, "exp": time.Now().Add(time.Hour).Unix()})
	hs.Header["kid"] = "rsa-1"
	hsToken, _ := hs.SignedString(pubPEM)
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": p.URL, "aud": audience, "exp": time.Now().Add(time.Hour).Unix()})
	none.Header["kid"] = "rsa-1"
	noneToken, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	// An EC signature presented under the RSA key's ID.
	wrongKey := ecKey
	wrongKey.kid = "rsa-1"
	good := rsaKey.sign(t, p.URL, nil)

	for name, token := range map[string]string{
		"expired":            rsaKey.sign(t, p.URL, jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}),
		"not yet valid":      rsaKey.sign(t, p.URL, jwt.MapClaims{"nbf": time.Now().Add(time.Hour).Unix()}),
		"issued in future":   rsaKey.sign(t, p.URL, jwt.MapClaims{"iat": time.Now().Add(time.Hour).Unix()}),
		"no expiry":          rsaKey.sign(t, p.URL, jwt.MapClaims{"exp": nil}),
		"wrong issuer":       rsaKey.sign(t, p.URL, jwt.MapClaims{"iss": "https://evil.example"}),
		"wrong audience":     rsaKey.sign(t, p.URL, jwt.MapClaims{"aud": "billing-api"}),
		"no audience":        rsaKey.sign(t, p.URL, jwt.MapClaims{"aud": nil}),
		"alg none":           noneToken,
		"HS256 with pub key": hsToken,
		"key type mismatch":  wrongKey.sign(t, p.URL, nil),
		"tampered":           good[:len(good)-4] + "AAAA",
		"garbage":            "not.a.token",
		"empty":              "",
	} {
		_, err := v.Verify(ctx, token)
		if errors.KindOf(err) != errors.Unauthorized {
			t.Errorf("%s: err = %v, want unauthorized", name, err)
			continue
		}
		if msg := errors.PublicMessage(err); msg != "invalid or expired token" {
			t.Errorf("%s: public message %q reveals details", name, msg)
		}
	}
	if n := metrics.Sum("auth.tokens", attribute.String("outcome", "expired")); n != 1 {
		t.Errorf("expired outcomes = %v", n)
	}
	if n := metrics.Sum("auth.tokens", attribute.String("outcome", "invalid")); n < 10 {
		t.Errorf("invalid outcomes = %v", n)
	}
}

func TestAlgorithmsAndJWKAlgRestrict(t *testing.T) {
	rsaKey, ecKey := newRSA(t, "rsa-1"), newEC(t, "ec-1")
	ecKey.jwkAlg = "ES384" // the JWKS says this key is for ES384 only
	p := newIDP(t, rsaKey, ecKey)
	v := newVerifier(t, p, func(c *jwtauth.Config) { c.Algorithms = []string{"ES256", "ES384"} })
	if _, err := v.Verify(context.Background(), rsaKey.sign(t, p.URL, nil)); err == nil {
		t.Error("RS256 accepted although only ES256 and ES384 are allowed")
	}
	if _, err := v.Verify(context.Background(), ecKey.sign(t, p.URL, nil)); err == nil {
		t.Error("ES256 accepted with a key published for ES384")
	}

	for _, alg := range []string{"HS256", "none", "XX1"} {
		_, err := jwtauth.New(context.Background(), jwtauth.Config{Issuer: p.URL, Audience: []string{audience}, Algorithms: []string{alg}})
		if errors.KindOf(err) != errors.InvalidArgument {
			t.Errorf("algorithm %s: err = %v, want invalid_argument", alg, err)
		}
	}
	if _, err := jwtauth.New(context.Background(), jwtauth.Config{Issuer: p.URL}); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("no audience: err = %v", err)
	}
}

func TestKeyRotationAndRefreshRateLimit(t *testing.T) {
	old, rotated := newRSA(t, "2026-01"), newRSA(t, "2026-02")
	p := newIDP(t, old)
	clk := &clock{now: time.Now()}
	v := newVerifier(t, p, nil, jwtauth.WithClock(clk.Now))
	ctx := context.Background()
	if p.fetches.Load() != 1 {
		t.Fatalf("fetches at start = %d", p.fetches.Load())
	}

	// Tokens with made-up key IDs refresh at most once per MinRefreshInterval.
	clk.Add(time.Minute)
	forged := newRSA(t, "")
	for i := range 20 {
		forged.kid = "forged-" + string(rune('a'+i))
		if _, err := v.Verify(ctx, forged.sign(t, p.URL, nil)); err == nil {
			t.Fatal("token with an unknown key accepted")
		}
	}
	if n := p.fetches.Load(); n != 2 {
		t.Errorf("fetches after 20 unknown key IDs = %d, want 2", n)
	}

	// The provider rotates its key; once the rate limit allows, a token
	// with the new key ID triggers a refresh and verifies.
	p.set(false, rotated, old)
	if _, err := v.Verify(ctx, rotated.sign(t, p.URL, nil)); err == nil {
		t.Error("new key accepted before a refresh was allowed")
	}
	clk.Add(31 * time.Second)
	if _, err := v.Verify(ctx, rotated.sign(t, p.URL, nil)); err != nil {
		t.Errorf("token with the rotated key: %v", err)
	}
	if _, err := v.Verify(ctx, old.sign(t, p.URL, nil)); err != nil {
		t.Errorf("token with the old key, still published: %v", err)
	}
}

func TestProviderDownKeepsCachedKeys(t *testing.T) {
	k := newRSA(t, "k1")
	p := newIDP(t, k)
	clk := &clock{now: time.Now()}
	mp, metrics := testkit.NewMetrics(t)
	v := newVerifier(t, p, nil, jwtauth.WithClock(clk.Now), jwtauth.WithMeterProvider(mp))
	ctx := context.Background()

	p.set(true)
	clk.Add(time.Hour)
	if _, err := v.Verify(ctx, k.sign(t, p.URL, nil)); err != nil {
		t.Errorf("cached key while the provider is down: %v", err)
	}
	unknown := newRSA(t, "k2")
	if _, err := v.Verify(ctx, unknown.sign(t, p.URL, nil)); errors.KindOf(err) != errors.Unauthorized {
		t.Errorf("unknown key while down: %v", err)
	}
	if n := metrics.Sum("auth.tokens", attribute.String("outcome", "keys_unavailable")); n != 1 {
		t.Errorf("keys_unavailable = %v", n)
	}
	if n := metrics.Sum("auth.jwks.refreshes", attribute.String("outcome", "failed")); n != 1 {
		t.Errorf("failed refreshes = %v", n)
	}

	// At startup, an unreachable provider fails New.
	_, err := jwtauth.New(ctx, jwtauth.Config{Issuer: p.URL, Audience: []string{audience}})
	if errors.KindOf(err) != errors.Unavailable {
		t.Errorf("New with the provider down: %v, want unavailable", err)
	}
}

func TestDiscovery(t *testing.T) {
	k := newRSA(t, "k1")
	p := newIDP(t, k)
	// Explicit JWKS URL, issuer not serving discovery.
	v := newVerifier(t, p, func(c *jwtauth.Config) { c.Issuer = "https://issuer.example"; c.JWKSURL = p.URL + "/keys" })
	if _, err := v.Verify(context.Background(), k.sign(t, "https://issuer.example", nil)); err != nil {
		t.Errorf("explicit JWKS URL: %v", err)
	}
	// Discovery must name the issuer it was fetched for: the fake serves
	// a document for p.URL at every path.
	_, err := jwtauth.New(context.Background(), jwtauth.Config{Issuer: p.URL + "/other", Audience: []string{audience}})
	if errors.KindOf(err) != errors.InvalidArgument || !strings.Contains(err.Error(), "is for issuer") {
		t.Errorf("discovery document for another issuer: %v", err)
	}
}

func TestEncryptionKeysAreNotForSignatures(t *testing.T) {
	sig, enc := newRSA(t, "sig-1"), newRSA(t, "enc-1")
	p := newIDP(t, sig)
	p.encryption = []signer{enc}
	v := newVerifier(t, p, nil)
	if _, err := v.Verify(context.Background(), enc.sign(t, p.URL, nil)); err == nil {
		t.Error("token signed with a key published for encryption accepted")
	}
}

func TestSingleKeyWithoutKeyID(t *testing.T) {
	k := newEd(t, "only")
	p := newIDP(t, k)
	v := newVerifier(t, p, nil)
	k.kid = ""
	if _, err := v.Verify(context.Background(), k.sign(t, p.URL, nil)); err != nil {
		t.Errorf("token without kid, JWKS with one key: %v", err)
	}
}

func TestHTTPAuthAndScopes(t *testing.T) {
	k := newRSA(t, "k1")
	p := newIDP(t, k)
	v := newVerifier(t, p, nil)
	var sub string
	h := httpserver.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := jwtauth.FromContext(r.Context())
		sub = c.Subject
		w.WriteHeader(http.StatusNoContent)
	}), httpserver.Auth(jwtauth.HTTP(v)), jwtauth.RequireScope("orders:write"))

	for name, tc := range map[string]struct {
		header string
		want   int
	}{
		"no header":     {"", http.StatusUnauthorized},
		"basic auth":    {"Basic dXNlcjpwdw==", http.StatusUnauthorized},
		"invalid token": {"Bearer nope", http.StatusUnauthorized},
		"missing scope": {"Bearer " + k.sign(t, p.URL, jwt.MapClaims{"scope": "orders:read"}), http.StatusForbidden},
		"ok":            {"bearer " + k.sign(t, p.URL, jwt.MapClaims{"scope": "orders:write"}), http.StatusNoContent},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/orders", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
		if strings.Contains(rec.Body.String(), "jwtauth:") {
			t.Errorf("%s: response reveals internal error: %s", name, rec.Body)
		}
	}
	if sub != "user-7" {
		t.Errorf("handler saw subject %q", sub)
	}
}

func TestBearerToken(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc", "Basic abc": "", "Bearer": "", "": ""} {
		got, ok := jwtauth.BearerToken(in)
		if got != want || ok != (want != "") {
			t.Errorf("BearerToken(%q) = %q, %v", in, got, ok)
		}
	}
}
