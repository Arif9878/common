package commonfx

import (
	"context"
	"net/http"

	"go.uber.org/fx"
	"google.golang.org/grpc"

	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/transport/grpc/grpcserver"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

// HTTPOption configures [HTTPServer].
type HTTPOption func(*httpOptions)

type httpOptions struct {
	asIs       bool
	middleware []httpserver.Option
}

// HTTPHandlerAsIs serves the handler without the standard middleware. Use
// it when the handler already applies it, as Echo apps with
// echoadapter.Middleware do; otherwise requests would be traced and logged
// twice.
func HTTPHandlerAsIs() HTTPOption { return func(o *httpOptions) { o.asIs = true } }

// HTTPMiddlewareOptions configures the standard middleware, for example
// httpserver.WithFilter, WithMaxBodyBytes or WithTimeout.
func HTTPMiddlewareOptions(opts ...httpserver.Option) HTTPOption {
	return func(o *httpOptions) { o.middleware = append(o.middleware, opts...) }
}

// HTTPServer serves the http.Handler in the graph with httpserver.Config,
// wrapped in the standard middleware (httpserver.Handler: request ID,
// tracing, metrics, access log, panic recovery, body limit, timeout) using
// the graph's logger and telemetry. For an *http.ServeMux, routes are
// resolved from the mux. Provide the handler as an http.Handler:
//
//	fx.Provide(fx.Annotate(NewMux, fx.As(new(http.Handler))))
//
// The listener opens on app start, so an address in use fails start; the
// server stops in graceful.StopIntake.
func HTTPServer(opts ...HTTPOption) fx.Option {
	var o httpOptions
	for _, opt := range opts {
		opt(&o)
	}
	return fx.Module("commonfx.http",
		fx.Invoke(func(lc fx.Lifecycle, cfg httpserver.Config, h http.Handler, g *graceful.Manager, t *telemetry) {
			if !o.asIs {
				h = httpserver.Handler(h, append(t.httpServer(), o.middleware...)...)
			}
			srv := httpserver.NewServer(cfg, h, t.logger)
			lc.Append(fx.Hook{OnStart: func(context.Context) error { return httpserver.Serve(g, srv) }})
		}),
	)
}

// AdminConfig configures the admin server. Environment variable names are
// relative; for example ADMIN_ADDR with prefix ADMIN_.
type AdminConfig struct {
	Addr string `env:"ADDR" envDefault:":9090"`
}

type adminIn struct {
	fx.In
	Config  AdminConfig `optional:"true"`
	Checks  *health.Checker
	Metrics *metrics.Provider `optional:"true"`
}

// AdminServer serves /live, /ready and /startup from the health.Checker and,
// with Observability, /metrics, on AdminConfig.Addr (":9090" by default),
// separate from the public API. It stops in graceful.Telemetry, the last
// phase, so readiness keeps answering 503 while the service drains and the
// last metrics can still be scraped.
func AdminServer() fx.Option {
	return fx.Module("commonfx.admin",
		fx.Invoke(func(lc fx.Lifecycle, in adminIn, g *graceful.Manager, t *telemetry) {
			mux := http.NewServeMux()
			mux.Handle("GET /live", in.Checks.LiveHandler())
			mux.Handle("GET /ready", in.Checks.ReadyHandler())
			mux.Handle("GET /startup", in.Checks.StartupHandler())
			if in.Metrics != nil {
				mux.Handle("GET /metrics", in.Metrics.Handler())
			}
			cfg := httpserver.Config{Addr: in.Config.Addr}
			if cfg.Addr == "" {
				cfg.Addr = ":9090"
			}
			srv := httpserver.NewServer(cfg, mux, t.logger)
			lc.Append(fx.Hook{OnStart: func(context.Context) error {
				// Serve registers in StopIntake; the admin server must outlive
				// draining, so it is served on its own and stopped last.
				adminGraceful := graceful.New(graceful.WithSignals(), graceful.WithLogger(t.logger))
				if err := httpserver.Serve(adminGraceful, srv); err != nil {
					return err
				}
				return g.Register(graceful.Telemetry, "admin http", adminGraceful.Shutdown)
			}})
		}),
	)
}

// GRPCConfig configures the gRPC server address.
type GRPCConfig struct {
	Addr string `env:"ADDR" envDefault:":9091"`
}

type grpcIn struct {
	fx.In
	Config GRPCConfig `optional:"true"`
	Checks *health.Checker
}

// GRPCServer provides a *grpc.Server from grpcserver.New, with the graph's
// logger and telemetry and the health service from the health.Checker, for
// you to register services on in fx.Invoke. It serves on GRPCConfig.Addr
// (":9091" by default) once the app starts, after all registrations, and
// stops in graceful.StopIntake.
func GRPCServer(opts ...grpcserver.Option) fx.Option {
	return fx.Module("commonfx.grpc",
		fx.Provide(func(in grpcIn, t *telemetry) *grpc.Server {
			return grpcserver.New(append(append(t.grpcServer(), grpcserver.WithHealth(in.Checks)), opts...)...)
		}),
		fx.Invoke(func(lc fx.Lifecycle, in grpcIn, srv *grpc.Server, g *graceful.Manager) {
			addr := in.Config.Addr
			if addr == "" {
				addr = ":9091"
			}
			lc.Append(fx.Hook{OnStart: func(context.Context) error { return grpcserver.Serve(g, srv, addr) }})
		}),
	)
}
