package jwtauth

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are a verified token's claims.
type Claims struct {
	// Subject is the "sub" claim: the user or client the token was issued
	// to.
	Subject   string
	Issuer    string
	Audience  []string
	ExpiresAt time.Time
	// ID is the "jti" claim, if any.
	ID string
	// Scopes are the OAuth scopes, from the "scope" claim (space-separated)
	// or the "scp" claim (a string or a list), whichever the provider uses.
	Scopes []string

	raw jwt.MapClaims
}

func (c *Claims) fill() {
	c.Subject, _ = c.raw.GetSubject()
	c.Issuer, _ = c.raw.GetIssuer()
	c.Audience, _ = c.raw.GetAudience()
	if exp, _ := c.raw.GetExpirationTime(); exp != nil {
		c.ExpiresAt = exp.Time
	}
	c.ID, _ = c.raw["jti"].(string)
	switch s := c.raw["scope"].(type) {
	case string:
		c.Scopes = strings.Fields(s)
	}
	if len(c.Scopes) == 0 {
		switch s := c.raw["scp"].(type) {
		case string:
			c.Scopes = strings.Fields(s)
		case []any:
			for _, v := range s {
				if str, ok := v.(string); ok {
					c.Scopes = append(c.Scopes, str)
				}
			}
		}
	}
}

// HasScope reports whether the token grants scope.
func (c *Claims) HasScope(scope string) bool { return slices.Contains(c.Scopes, scope) }

// Get returns any claim, such as a provider-specific "realm_access" or
// "email", as decoded from JSON (string, float64, bool, []any or
// map[string]any).
func (c *Claims) Get(name string) (any, bool) {
	v, ok := c.raw[name]
	return v, ok
}

type claimsKey struct{}

// NewContext returns ctx carrying c.
func NewContext(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// FromContext returns the claims of the authenticated caller, if any.
func FromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(*Claims)
	return c, ok
}
