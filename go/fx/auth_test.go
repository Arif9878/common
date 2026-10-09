package commonfx_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/Arif9878/common/go/auth/jwtauth"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/testkit"
	"github.com/Arif9878/common/go/transport/grpc/grpcclient"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

// fakeIssuer serves one RSA key as a JWKS and signs tokens with it.
func fakeIssuer(t *testing.T) (issuer string, sign func(scope string) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "use": "sig",
				"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
			}}})
		}
	}))
	t.Cleanup(srv.Close)
	issuer = srv.URL
	return issuer, func(scope string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": issuer, "aud": "orders-api", "sub": "user-7", "scope": scope,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
		tok.Header["kid"] = "k1"
		s, _ := tok.SignedString(key)
		return s
	}
}

// whoService is a one-method gRPC service returning the caller's subject.
var whoService = grpc.ServiceDesc{
	ServiceName: "test.Who",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Get",
		Handler: func(_ any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, _ any) (any, error) {
				if err := jwtauth.CheckScopes(ctx, "who:read"); err != nil {
					return nil, err
				}
				c, _ := jwtauth.FromContext(ctx)
				return wrapperspb.String(c.Subject), nil
			}
			return ic(ctx, in, &grpc.UnaryServerInfo{FullMethod: "/test.Who/Get"}, h)
		},
	}},
}

func TestJWTAuthForEchoAndGRPC(t *testing.T) {
	issuer, sign := fakeIssuer(t)
	httpAddr, grpcAddr := testkit.FreeAddr(t), testkit.FreeAddr(t)
	logger, _ := testkit.NewLogger(t)

	app := fxtest.New(t,
		fx.Supply(logger, httpserver.Config{Addr: httpAddr}, commonfx.GRPCConfig{Addr: grpcAddr},
			jwtauth.Config{Issuer: issuer, Audience: []string{"orders-api"}}),
		commonfx.Lifecycle(),
		commonfx.JWTAuth(),
		commonfx.EchoServer(),
		commonfx.GRPCServer(),
		commonfx.GRPCJWTAuth(),
		fx.Invoke(func(e *echo.Echo, v *jwtauth.Verifier, srv *grpc.Server) {
			e.GET("/public", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
			api := e.Group("/orders", echo.WrapMiddleware(httpserver.Auth(jwtauth.HTTP(v))))
			api.GET("", func(c echo.Context) error {
				cl, _ := jwtauth.FromContext(c.Request().Context())
				return c.String(http.StatusOK, cl.Subject)
			}, echo.WrapMiddleware(jwtauth.RequireScope("orders:read")))
			srv.RegisterService(&whoService, struct{}{})
		}),
		commonfx.Ready(),
	)
	app.RequireStart()
	defer app.RequireStop()

	httpGet := func(path, token string) int {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+httpAddr+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for name, tc := range map[string]struct {
		path, token string
		want        int
	}{
		"public":        {"/public", "", http.StatusNoContent},
		"no token":      {"/orders", "", http.StatusUnauthorized},
		"missing scope": {"/orders", sign("other"), http.StatusForbidden},
		"ok":            {"/orders", sign("orders:read"), http.StatusOK},
	} {
		if got := httpGet(tc.path, tc.token); got != tc.want {
			t.Errorf("HTTP %s: %d, want %d", name, got, tc.want)
		}
	}

	conn, err := grpcclient.New(grpcAddr, grpcclient.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	who := func(token string) (string, codes.Code) {
		ctx := context.Background()
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}
		out := new(wrapperspb.StringValue)
		err := conn.Invoke(ctx, "/test.Who/Get", &emptypb.Empty{}, out)
		return out.GetValue(), status.Code(err)
	}
	if _, code := who(""); code != codes.Unauthenticated {
		t.Errorf("gRPC without token: %v", code)
	}
	if _, code := who(sign("other")); code != codes.PermissionDenied {
		t.Errorf("gRPC missing scope: %v", code)
	}
	if sub, code := who(sign("who:read")); code != codes.OK || sub != "user-7" {
		t.Errorf("gRPC ok: %q %v", sub, code)
	}
}
