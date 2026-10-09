package commonfx

import (
	"go.uber.org/fx"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/transport/grpc/grpcserver"
)

// JWTAuth provides *jwtauth.Verifier from the graph's jwtauth.Config,
// fetching the issuer's keys while the graph is built (so a wrong issuer or
// an unreachable provider fails startup). Its background key refresh stops
// in graceful.CloseDeps. Protect Echo routes with it:
//
//	commonfx.JWTAuth(),
//	fx.Invoke(func(e *echo.Echo, v *jwtauth.Verifier, h *OrderHandler) {
//		api := e.Group("/orders", echo.WrapMiddleware(httpserver.Auth(jwtauth.HTTP(v))))
//		api.POST("", h.Create, echo.WrapMiddleware(jwtauth.RequireScope("orders:write")))
//	}),
func JWTAuth(opts ...jwtauth.Option) fx.Option {
	return fx.Module("commonfx.jwtauth",
		fx.Provide(func(cfg jwtauth.Config, g *graceful.Manager, t *telemetry) (*jwtauth.Verifier, error) {
			ctx, cancel := connectCtx()
			defer cancel()
			base := []jwtauth.Option{jwtauth.WithLogger(t.logger)}
			if t.mp != nil {
				base = append(base, jwtauth.WithMeterProvider(t.mp))
			}
			v, err := jwtauth.New(ctx, cfg, append(base, opts...)...)
			if err != nil {
				return nil, err
			}
			return v, g.Register(graceful.CloseDeps, "jwtauth", v.Close)
		}),
	)
}

// GRPCJWTAuth makes [GRPCServer] require a valid bearer token on every call
// except publicMethods (the health service is always public), verified by
// the graph's *jwtauth.Verifier ([JWTAuth]). Handlers read the caller with
// jwtauth.FromContext and check scopes with jwtauth.CheckScopes.
func GRPCJWTAuth(publicMethods ...string) fx.Option {
	return fx.Provide(fx.Annotate(
		func(v *jwtauth.Verifier) grpcserver.Option {
			return grpcserver.WithAuth(grpcserver.Bearer(v.Authenticate), publicMethods...)
		},
		fx.ResultTags(GRPCServerOptions),
	))
}
