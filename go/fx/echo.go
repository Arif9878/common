package commonfx

import (
	"context"

	"github.com/labstack/echo/v4"
	"go.uber.org/fx"

	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/transport/http/echoadapter"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

// EchoOption configures [EchoServer].
type EchoOption func(*echoOptions)

type echoOptions struct {
	middleware []httpserver.Option
}

// EchoMiddlewareOptions configures the standard middleware, for example
// httpserver.WithFilter, WithMaxBodyBytes or WithTimeout.
func EchoMiddlewareOptions(opts ...httpserver.Option) EchoOption {
	return func(o *echoOptions) { o.middleware = append(o.middleware, opts...) }
}

// EchoServer provides an *echo.Echo set up the standard way and serves it
// with httpserver.Config:
//
//   - e.HTTPErrorHandler is echoadapter.ErrorHandler, so classified errors
//     become problem+json responses with the status of their kind;
//   - echoadapter.Middleware (request ID, tracing, metrics, access log,
//     panic recovery) runs with the graph's logger and telemetry, labeling
//     spans and metrics with the route template (/orders/:id).
//
// Register routes and further middleware in fx.Invoke:
//
//	commonfx.EchoServer(),
//	fx.Invoke(func(e *echo.Echo, orders *OrderHandler) {
//		e.GET("/orders/:id", orders.Get)
//	}),
//
// The listener opens on app start, after every fx.Invoke has registered its
// routes, so an address in use fails start; the server stops in
// graceful.StopIntake. Use EchoServer instead of [HTTPServer], not with it:
// both serve on httpserver.Config.Addr.
func EchoServer(opts ...EchoOption) fx.Option {
	var o echoOptions
	for _, opt := range opts {
		opt(&o)
	}
	return fx.Module("commonfx.echo",
		fx.Provide(func(t *telemetry) *echo.Echo {
			e := echo.New()
			e.HideBanner, e.HidePort = true, true // the access log and Serve report it
			e.HTTPErrorHandler = echoadapter.ErrorHandler
			e.Use(echoadapter.Middleware(append(t.httpServer(), o.middleware...)...))
			return e
		}),
		fx.Invoke(func(lc fx.Lifecycle, cfg httpserver.Config, e *echo.Echo, g *graceful.Manager, t *telemetry) {
			srv := httpserver.NewServer(cfg, e, t.logger)
			lc.Append(fx.Hook{OnStart: func(context.Context) error { return httpserver.Serve(g, srv) }})
		}),
	)
}
