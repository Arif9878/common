package jwtauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/testkit"
)

func TestAuthenticateAddsSubjectToTelemetry(t *testing.T) {
	key := newRSA(t, "rsa-1")
	p := newIDP(t, key)
	token := key.sign(t, p.URL, jwt.MapClaims{})

	for name, tc := range map[string]struct {
		opts []jwtauth.Option
		want bool
	}{
		"default":                   {nil, true},
		"WithoutSubjectInTelemetry": {[]jwtauth.Option{jwtauth.WithoutSubjectInTelemetry()}, false},
	} {
		t.Run(name, func(t *testing.T) {
			v := newVerifier(t, p, nil, tc.opts...)
			logger, logs := testkit.NewLogger(t)
			tp, spans := testkit.NewTracer(t)
			ctx, span := tp.Tracer("t").Start(context.Background(), "request")
			ctx, err := v.Authenticate(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			logger.InfoContext(ctx, "handled")
			span.End()

			rec := logs.Messages("handled")[0]
			if got := rec["user_id"] == "user-7"; got != tc.want {
				t.Errorf("user_id in logs = %v, want %v (%v)", got, tc.want, rec)
			}
			var inSpan bool
			for _, a := range spans.Named("request")[0].Attributes() {
				inSpan = inSpan || a == attribute.String("user.id", "user-7")
			}
			if inSpan != tc.want {
				t.Errorf("user.id on span = %v, want %v", inSpan, tc.want)
			}
		})
	}
}

func TestClaimValue(t *testing.T) {
	key := newRSA(t, "rsa-1")
	p := newIDP(t, key)
	v := newVerifier(t, p, nil)
	read := jwtauth.ClaimValue("tenant_id")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if _, ok := read(req); ok {
		t.Error("a claim was read without a verified token")
	}
	for claims, want := range map[string]jwt.MapClaims{
		"acme": {"tenant_id": "acme"},
		"":     {"tenant_id": 42},
	} {
		ctx, err := v.Authenticate(context.Background(), key.sign(t, p.URL, want))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := read(req.WithContext(ctx))
		if got != claims || ok != (claims != "") {
			t.Errorf("ClaimValue with %v = %q, %v", want, got, ok)
		}
	}
}
