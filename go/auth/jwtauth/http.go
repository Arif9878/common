package jwtauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

var errNoToken = errors.WithPublicMessage(errors.Unauthorized.New("jwtauth: no bearer token"), "missing bearer token")

// HTTP returns an httpserver.Authenticator that verifies the request's
// "Authorization: Bearer" token with v. Use it with httpserver.Auth, or
// with echo.WrapMiddleware(httpserver.Auth(jwtauth.HTTP(v))) for Echo.
func HTTP(v *Verifier) httpserver.Authenticator {
	return func(r *http.Request) (context.Context, error) {
		token, ok := BearerToken(r.Header.Get("Authorization"))
		if !ok {
			return nil, errNoToken
		}
		return v.Authenticate(r.Context(), token)
	}
}

// ClaimValue returns a function reading the string claim name of the
// request's verified token, such as "tenant_id", for tenant.Middleware.
// Put that middleware after httpserver.Auth.
func ClaimValue(name string) func(*http.Request) (string, bool) {
	return func(r *http.Request) (string, bool) {
		c, ok := FromContext(r.Context())
		if !ok {
			return "", false
		}
		v, ok := c.Get(name)
		s, isString := v.(string)
		return s, ok && isString
	}
}

// BearerToken returns the token of an "Authorization: Bearer <token>"
// header value. The scheme is case-insensitive.
func BearerToken(authorization string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// RequireScope rejects requests whose token does not grant every scope,
// with 403 (or 401 without an authenticated caller). Put it after
// httpserver.Auth, for example on a route group.
func RequireScope(scopes ...string) httpserver.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := CheckScopes(r.Context(), scopes...); err != nil {
				httpserver.WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CheckScopes returns an error of kind Forbidden unless the caller in ctx
// has every scope, or Unauthorized without a caller. Use it in handlers and
// gRPC methods.
func CheckScopes(ctx context.Context, scopes ...string) error {
	c, ok := FromContext(ctx)
	if !ok {
		return errNoToken
	}
	for _, s := range scopes {
		if !c.HasScope(s) {
			return errors.WithPublicMessage(errors.Forbidden.Errorf("jwtauth: missing scope %s", s), "insufficient scope")
		}
	}
	return nil
}
