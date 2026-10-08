// Package grpcserver builds *grpc.Server values with the standard
// interceptors, error mapping, health service and graceful shutdown.
//
//	srv := grpcserver.New(grpcserver.WithLogger(logger), grpcserver.WithHealth(checks))
//	orderspb.RegisterOrdersServer(srv, ordersService)
//	if err := grpcserver.Serve(shutdown, srv, ":9090"); err != nil {
//		return err
//	}
//
// New returns the native *grpc.Server, so generated registration functions
// and every grpc.ServerOption (through [WithServerOptions]) work as usual.
//
// # What every RPC gets
//
// In order, outermost first:
//
//	otelgrpc stats handler  trace span (W3C context from metadata) and
//	                        rpc.server.call.duration metrics
//	observe                 request ID (x-request-id metadata: accepted if
//	                        valid, else generated; echoed in the response
//	                        trailer), one access-log line, and error mapping
//	recover                 panics become an Internal error
//	auth                    optional, see WithAuth
//
// All of them are on by default; there is nothing to forget. Logs carry
// grpc.method, grpc.code, duration_ms, request_id, trace_id and, for
// failures, the full error. Messages and metadata values are never logged.
//
// # Errors
//
// Handlers return errors classified with the errors package. They reach
// clients through grpcstatus.ToStatus: a status with the kind's code and
// only errors.PublicMessage as the message; the full error stays in the
// log. Errors that already carry a gRPC status (status.Error, or errors
// from downstream calls) keep it unless reclassified on top.
//
// # Server defaults
//
// Keepalive: connections are recycled after 30 minutes (plus 30s grace),
// which lets L4 load balancers rebalance; clients may ping every 10s.
// Maximum message size stays at gRPC's 4 MiB.
package grpcserver

import (
	"context"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/transport/grpc/grpcstatus"
)

// metadataRequestID is the request ID metadata key (gRPC keys are lower
// case).
const metadataRequestID = "x-request-id"

// Authenticator checks the credentials of a call (for example the
// "authorization" metadata) and returns a context with the caller's
// identity. Errors should have kind Unauthorized or Forbidden; unclassified
// errors are treated as Unauthorized.
type Authenticator func(ctx context.Context, fullMethod string) (context.Context, error)

// Option configures [New].
type Option func(*options)

type options struct {
	logger      *slog.Logger
	meterProv   metric.MeterProvider
	tracerProv  trace.TracerProvider
	propagators propagation.TextMapPropagator
	auth        Authenticator
	public      map[string]bool
	checker     *health.Checker
	serverOpts  []grpc.ServerOption
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.meterProv = mp } }

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// WithPropagators sets how trace context is read from metadata. The
// default is the global propagator.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *options) { o.propagators = p }
}

// WithAuth authenticates every call except those to publicMethods (full
// method names such as "/grpc.health.v1.Health/Check"; the health service
// is always public).
func WithAuth(auth Authenticator, publicMethods ...string) Option {
	return func(o *options) {
		o.auth = auth
		for _, m := range publicMethods {
			o.public[m] = true
		}
	}
}

// WithHealth registers the standard gRPC health service
// (grpc.health.v1.Health) answering from checker's readiness, for
// Kubernetes gRPC probes and load balancers.
func WithHealth(checker *health.Checker) Option { return func(o *options) { o.checker = checker } }

// WithServerOptions adds native grpc.ServerOption values, for example
// grpc.Creds for TLS or grpc.MaxRecvMsgSize. They are applied after the
// package's options, so they can override them.
func WithServerOptions(opts ...grpc.ServerOption) Option {
	return func(o *options) { o.serverOpts = append(o.serverOpts, opts...) }
}

// New returns a *grpc.Server with the standard interceptors.
func New(opts ...Option) *grpc.Server {
	o := options{
		logger:      slog.Default(),
		meterProv:   otel.GetMeterProvider(),
		tracerProv:  otel.GetTracerProvider(),
		propagators: otel.GetTextMapPropagator(),
		public:      map[string]bool{healthpb.Health_Check_FullMethodName: true, healthpb.Health_Watch_FullMethodName: true},
	}
	for _, opt := range opts {
		opt(&o)
	}

	ob := &observer{o: o}
	srv := grpc.NewServer(append([]grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(o.tracerProv),
			otelgrpc.WithMeterProvider(o.meterProv),
			otelgrpc.WithPropagators(o.propagators),
		)),
		grpc.ChainUnaryInterceptor(ob.unary),
		grpc.ChainStreamInterceptor(ob.stream),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 30 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}, o.serverOpts...)...)

	if o.checker != nil {
		healthpb.RegisterHealthServer(srv, &healthServer{checker: o.checker})
	}
	return srv
}

type observer struct{ o options }

// begin sets up the request ID and returns the context to use.
func (ob *observer) begin(ctx context.Context) context.Context {
	id := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(metadataRequestID); len(v) > 0 {
			id = v[0]
		}
	}
	if !requestid.Valid(id) {
		id = requestid.New()
	}
	// A trailer, not a header: gRPC only retries calls whose response has
	// not started, and a header would commit every call.
	_ = grpc.SetTrailer(ctx, metadata.Pairs(metadataRequestID, id))
	return requestid.NewContext(ctx, id)
}

// finish logs the call and converts its error for the client.
func (ob *observer) finish(ctx context.Context, method string, start time.Time, err error) error {
	code := grpcstatus.Code(err)
	level := slog.LevelInfo
	switch code {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable, codes.Unimplemented:
		level = slog.LevelError
	}
	ob.o.logger.LogAttrs(ctx, level, "grpc call",
		slog.String("grpc.method", method),
		slog.String("grpc.code", code.String()),
		logging.Duration(time.Since(start)),
		logging.Err(err),
	)
	return grpcstatus.ToStatus(err)
}

func (ob *observer) authenticate(ctx context.Context, method string) (context.Context, error) {
	if ob.o.auth == nil || ob.o.public[method] {
		return ctx, nil
	}
	actx, err := ob.o.auth(ctx, method)
	if err != nil {
		if errors.KindOf(err) == errors.Unknown {
			err = errors.Unauthorized.Wrap(err, "authenticate")
		}
		return ctx, err
	}
	if actx == nil {
		actx = ctx
	}
	return actx, nil
}

func (ob *observer) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	start := time.Now()
	ctx = ob.begin(ctx)
	defer func() { err = ob.finish(ctx, info.FullMethod, start, err) }()
	defer ob.recover(ctx, info.FullMethod, &err)

	if ctx, err = ob.authenticate(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (ob *observer) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	start := time.Now()
	ctx := ob.begin(ss.Context())
	defer func() { err = ob.finish(ctx, info.FullMethod, start, err) }()
	defer ob.recover(ctx, info.FullMethod, &err)

	if ctx, err = ob.authenticate(ctx, info.FullMethod); err != nil {
		return err
	}
	return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
}

func (ob *observer) recover(ctx context.Context, method string, errp *error) {
	if r := recover(); r != nil {
		*errp = errors.Internal.Errorf("panic: %v", r)
		ob.o.logger.ErrorContext(ctx, "grpc handler panicked", "grpc.method", method,
			logging.Err(*errp), "stack", string(debug.Stack()))
	}
}

// wrappedStream carries the enriched context to stream handlers.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// healthServer answers grpc.health.v1 checks from readiness.
type healthServer struct {
	healthpb.UnimplementedHealthServer
	checker *health.Checker
}

func (h *healthServer) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	st := healthpb.HealthCheckResponse_NOT_SERVING
	if h.checker.Ready(ctx).Status == health.StatusOK {
		st = healthpb.HealthCheckResponse_SERVING
	}
	return &healthpb.HealthCheckResponse{Status: st}, nil
}

// Serve listens on addr and serves srv in the background under g: a
// graceful stop is registered in graceful.StopIntake, and if serving
// fails, g shuts the service down. Listening happens before Serve returns,
// so an unusable address is reported immediately.
//
// The stop hook waits for in-flight RPCs (GracefulStop); when the shutdown
// context expires first, remaining RPCs and streams are cancelled (Stop).
func Serve(g *graceful.Manager, srv *grpc.Server, addr string) error {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	return ServeListener(g, srv, ln)
}

// ServeListener is like [Serve] with an existing listener.
func ServeListener(g *graceful.Manager, srv *grpc.Server, ln net.Listener) error {
	name := "grpc " + ln.Addr().String()
	if err := g.Register(graceful.StopIntake, name, func(ctx context.Context) error {
		return Stop(ctx, srv)
	}); err != nil {
		_ = ln.Close()
		return err
	}
	g.Go(name, func() error {
		if err := srv.Serve(ln); !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
		return nil
	})
	return nil
}

// Stop stops srv gracefully, waiting for in-flight RPCs, and forcibly when
// ctx ends first.
func Stop(ctx context.Context, srv *grpc.Server) error {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		srv.Stop()
		<-done
		return errors.Timeout.Wrap(ctx.Err(), "grpc: in-flight calls cancelled at shutdown deadline")
	}
}
