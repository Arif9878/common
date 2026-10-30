package jwtauth_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/errors"
)

func TestAudiencePerCall(t *testing.T) {
	k := newRSA(t, "rsa-1")
	p := newIDP(t, k)
	ctx := context.Background()

	// Without the option an empty audience is still a configuration error.
	if _, err := jwtauth.New(ctx, jwtauth.Config{Issuer: p.URL, JWKSURL: p.URL + "/keys"}); errors.KindOf(err) != errors.InvalidArgument {
		t.Fatalf("empty audience without WithAudiencePerCall: err = %v", err)
	}
	v := newVerifier(t, p, func(c *jwtauth.Config) { c.Audience = nil }, jwtauth.WithAudiencePerCall())
	tok := k.sign(t, p.URL, jwt.MapClaims{"aud": []string{"client-a"}})

	if _, err := v.VerifyAudience(ctx, tok, []string{"client-b", "client-a"}); err != nil {
		t.Errorf("accepted audience: %v", err)
	}
	for name, auds := range map[string][]string{"other audiences": {"client-b"}, "none": nil, "empty string": {""}} {
		if _, err := v.VerifyAudience(ctx, tok, auds); errors.KindOf(err) != errors.Unauthorized {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Verify uses Config.Audience, empty here: nothing passes.
	if _, err := v.Verify(ctx, tok); errors.KindOf(err) != errors.Unauthorized {
		t.Errorf("Verify with no configured audience: err = %v", err)
	}
	// A token without an audience never passes.
	if _, err := v.VerifyAudience(ctx, k.sign(t, p.URL, jwt.MapClaims{"aud": nil}), []string{""}); err == nil {
		t.Error("token without aud accepted")
	}
	// The other checks still apply.
	if _, err := v.VerifyAudience(ctx, k.sign(t, "https://evil.example", jwt.MapClaims{"aud": "client-a"}), []string{"client-a"}); err == nil {
		t.Error("wrong issuer accepted")
	}

	// AuthenticateAudience: the same check, and the claims in the context.
	actx, err := v.AuthenticateAudience(ctx, tok, []string{"client-a"})
	if err != nil {
		t.Fatalf("AuthenticateAudience: %v", err)
	}
	if c, ok := jwtauth.FromContext(actx); !ok || c.Subject != "user-7" {
		t.Errorf("claims in context = %+v, %v", c, ok)
	}
	if _, err := v.AuthenticateAudience(ctx, tok, []string{"client-b"}); errors.KindOf(err) != errors.Unauthorized {
		t.Errorf("AuthenticateAudience with another audience: err = %v", err)
	}
}

func TestIssuerAliases(t *testing.T) {
	k := newRSA(t, "rsa-1")
	p := newIDP(t, k)
	ctx := context.Background()
	v := newVerifier(t, p, func(c *jwtauth.Config) { c.IssuerAliases = []string{"idp.example"} })
	for _, iss := range []string{p.URL, "idp.example"} {
		if _, err := v.Verify(ctx, k.sign(t, iss, nil)); err != nil {
			t.Errorf("issuer %s: %v", iss, err)
		}
	}
	for _, iss := range []string{"other.example", ""} {
		if _, err := v.Verify(ctx, k.sign(t, iss, nil)); errors.KindOf(err) != errors.Unauthorized {
			t.Errorf("issuer %q accepted: %v", iss, err)
		}
	}
	// Without aliases, the alias is just another issuer.
	plain := newVerifier(t, p, nil)
	if _, err := plain.Verify(ctx, k.sign(t, "idp.example", nil)); err == nil {
		t.Error("alias accepted without IssuerAliases")
	}
}

func TestLazyStart(t *testing.T) {
	k := newRSA(t, "rsa-1")
	p := newIDP(t, k)
	ctx := context.Background()

	// Needs a JWKS URL: discovery would need the provider at start.
	if _, err := jwtauth.New(ctx, jwtauth.Config{Issuer: p.URL, Audience: []string{audience}}, jwtauth.WithLazyStart()); errors.KindOf(err) != errors.InvalidArgument {
		t.Fatalf("lazy without JWKSURL: err = %v", err)
	}
	// The provider is down at start: New still succeeds and fetches nothing.
	p.set(true)
	clk := &clock{now: time.Now()}
	v := newVerifier(t, p, func(c *jwtauth.Config) { c.JWKSURL = p.URL + "/keys" }, jwtauth.WithLazyStart(), jwtauth.WithClock(clk.Now))
	if n := p.fetches.Load(); n != 0 {
		t.Errorf("fetches at start = %d", n)
	}
	if _, err := v.Verify(ctx, k.sign(t, p.URL, nil)); errors.KindOf(err) != errors.Unauthorized {
		t.Errorf("provider down: err = %v", err)
	}
	// Once it is up (and the refresh rate limit allows), the next token
	// fetches the keys.
	p.set(false)
	clk.Add(31 * time.Second)
	if _, err := v.Verify(ctx, k.sign(t, p.URL, nil)); err != nil {
		t.Errorf("after the provider is up: %v", err)
	}
}
