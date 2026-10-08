package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

var testKey = []byte("test-signing-key")

func sign(t *testing.T, method jwt.SigningMethod, key any, exp time.Time) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, jwt.RegisteredClaims{
		Subject:   "user-1",
		ExpiresAt: jwt.NewNumericDate(exp),
	})
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func serve(t *testing.T, req *http.Request) (status int, token *jwt.Token) {
	t.Helper()
	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		token, _ = c.Get("token").(*jwt.Token)
		return c.NoContent(http.StatusOK)
	}, ValidateBearerToken(testKey))

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code, token
}

func TestValidateBearerToken(t *testing.T) {
	future := time.Now().Add(time.Hour)

	tests := []struct {
		name   string
		header string
		query  string
		want   int
	}{
		{"valid", "Bearer " + sign(t, jwt.SigningMethodHS256, testKey, future), "", http.StatusOK},
		{"valid via query", "", sign(t, jwt.SigningMethodHS256, testKey, future), http.StatusOK},
		{"missing", "", "", http.StatusUnauthorized},
		{"not bearer", "Basic abc", "", http.StatusUnauthorized},
		{"malformed", "Bearer not-a-jwt", "", http.StatusUnauthorized},
		{"wrong key", "Bearer " + sign(t, jwt.SigningMethodHS256, []byte("other"), future), "", http.StatusUnauthorized},
		{"legacy hard-coded key", "Bearer " + sign(t, jwt.SigningMethodHS256, []byte("secret"), future), "", http.StatusUnauthorized},
		{"expired", "Bearer " + sign(t, jwt.SigningMethodHS256, testKey, time.Now().Add(-time.Minute)), "", http.StatusUnauthorized},
		{"alg none", "Bearer " + sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, future), "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := "/"
			if tt.query != "" {
				target += "?access_token=" + tt.query
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			status, token := serve(t, req)
			if status != tt.want {
				t.Fatalf("status = %d, want %d", status, tt.want)
			}
			if tt.want == http.StatusOK && (token == nil || !token.Valid) {
				t.Fatalf("token not stored in context: %v", token)
			}
		})
	}
}

func TestValidateBearerTokenIgnoresAppEnv(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	status, _ := serve(t, httptest.NewRequest(http.MethodGet, "/", nil))
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestValidateBearerTokenEmptyKeyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for empty key")
		}
	}()
	ValidateBearerToken(nil)
}
