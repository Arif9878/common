package jwtauth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Arif9878/common/go/auth/jwtauth"
)

// FuzzVerify mutates a valid token: a mutated token may only verify if it
// still carries exactly the original claims (base64 has a few equivalent
// encodings of the same bytes), so no mutation forges claims.
func FuzzVerify(f *testing.F) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	f.Cleanup(srv.Close)
	v, err := jwtauth.New(context.Background(), jwtauth.Config{
		Issuer: "https://issuer.example", Audience: []string{"api"}, JWKSURL: srv.URL,
		RefreshInterval: time.Hour, MinRefreshInterval: time.Hour,
	}, jwtauth.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = v.Close(context.Background()) })

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://issuer.example", "aud": "api", "sub": "user-7", "scope": "read",
		"exp": time.Now().Add(24 * time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	valid, _ := tok.SignedString(key)
	f.Add(valid)
	f.Add("eyJhbGciOiJub25lIiwia2lkIjoiazEifQ.eyJpc3MiOiJodHRwczovL2lzc3Vlci5leGFtcGxlIiwiYXVkIjoiYXBpIn0.")
	f.Add("a.b.c")
	f.Fuzz(func(t *testing.T, token string) {
		c, err := v.Verify(context.Background(), token)
		if err != nil {
			return
		}
		if c.Subject != "user-7" || c.Issuer != "https://issuer.example" || len(c.Scopes) != 1 || c.Scopes[0] != "read" {
			t.Fatalf("mutated token verified with different claims: %+v", c)
		}
	})
}
