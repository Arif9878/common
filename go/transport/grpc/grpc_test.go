package grpc_test

import (
	"context"
	stderrors "errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Arif9878/common/go/testkit"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/transport/grpc/grpcclient"
	"github.com/Arif9878/common/go/transport/grpc/grpcserver"
	"github.com/Arif9878/common/go/transport/grpc/grpcstatus"
)

const secretDetail = "pq: password authentication failed for user svc"

// echoServer implements a hand-written test service; behavior is chosen by
// the request string.
type echoServer struct {
	flaky atomic.Int32
}

func (s *echoServer) echo(ctx context.Context, in string) (string, error) {
	switch in {
	case "notfound":
		return "", errors.NotFound.Wrap(stderrors.New(secretDetail), "load order")
	case "status":
		return "", status.Error(codes.FailedPrecondition, "explicit status message")
	case "reclassified":
		downstream := grpcstatus.FromStatus(status.Error(codes.NotFound, "downstream missing"))
		return "", errors.Unavailable.Wrap(downstream, "inventory unavailable")
	case "passthrough":
		return "", grpcstatus.FromStatus(status.Error(codes.NotFound, "downstream missing"))
	case "panic":
		panic("nil map write")
	case "slow":
		<-ctx.Done()
		return "", ctx.Err()
	case "sleep":
		time.Sleep(200 * time.Millisecond)
		return "slept", nil
	case "flaky":
		if s.flaky.Add(1) <= 2 {
			return "", errors.Unavailable.New("warming up")
		}
		return "ok", nil
	case "reqid":
		id, _ := requestid.FromContext(ctx)
		return id, nil
	case "user":
		u, _ := ctx.Value(userKey{}).(string)
		return u, nil
	}
	return in, nil
}

func (s *echoServer) count(n string, stream grpc.ServerStream) error {
	for i := range 3 {
		if n == "fail" && i == 1 {
			return errors.RateLimited.New("slow down")
		}
		if n == "forever" {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		if err := stream.SendMsg(wrapperspb.String(strings.Repeat("x", i+1))); err != nil {
			return err
		}
	}
	return nil
}

var echoDesc = grpc.ServiceDesc{
	ServiceName: "test.Echo",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Echo",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(wrapperspb.StringValue)
			if err := dec(in); err != nil {
				return nil, err
			}
			h := func(ctx context.Context, req any) (any, error) {
				out, err := srv.(*echoServer).echo(ctx, req.(*wrapperspb.StringValue).GetValue())
				if err != nil {
					return nil, err
				}
				return wrapperspb.String(out), nil
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Echo/Echo"}, h)
		},
	}},
	Streams: []grpc.StreamDesc{{
		StreamName:    "Count",
		ServerStreams: true,
		Handler: func(srv any, stream grpc.ServerStream) error {
			in := new(wrapperspb.StringValue)
			if err := stream.RecvMsg(in); err != nil {
				return err
			}
			return srv.(*echoServer).count(in.GetValue(), stream)
		},
	}},
}

type userKey struct{}

type env struct {
	conn    *grpc.ClientConn
	logs    *testkit.Logs
	spans   *testkit.Spans
	metrics *testkit.Metrics
	server  *echoServer
}

func setup(t *testing.T, serverOpts []grpcserver.Option, clientOpts ...grpcclient.Option) *env {
	t.Helper()
	logger, logs := testkit.NewLogger(t)
	mp, metrics := testkit.NewMetrics(t)
	tp, spans := testkit.NewTracer(t)
	e := &env{logs: logs, spans: spans, metrics: metrics, server: &echoServer{}}
	prop := propagation.TraceContext{}

	srv := grpcserver.New(append([]grpcserver.Option{
		grpcserver.WithLogger(logger), grpcserver.WithTracerProvider(tp),
		grpcserver.WithMeterProvider(mp), grpcserver.WithPropagators(prop),
	}, serverOpts...)...)
	srv.RegisterService(&echoDesc, e.server)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpcclient.New("passthrough:///bufnet", append([]grpcclient.Option{
		grpcclient.WithInsecure(),
		grpcclient.WithTracerProvider(tp), grpcclient.WithMeterProvider(mp), grpcclient.WithPropagators(prop),
		grpcclient.WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
	}, clientOpts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	e.conn = conn
	return e
}

func (e *env) echo(ctx context.Context, in string, opts ...grpc.CallOption) (string, error) {
	out := new(wrapperspb.StringValue)
	err := e.conn.Invoke(ctx, "/test.Echo/Echo", wrapperspb.String(in), out, opts...)
	return out.GetValue(), err
}

func (e *env) accessLogs(*testing.T) []testkit.Record { return e.logs.Messages("grpc call") }

func TestErrorMapping(t *testing.T) {
	e := setup(t, nil)
	tests := []struct {
		in      string
		code    codes.Code
		kind    errors.Kind
		message string
	}{
		{"notfound", codes.NotFound, errors.NotFound, "not found"},
		{"status", codes.FailedPrecondition, errors.InvalidArgument, "explicit status message"},
		{"reclassified", codes.Unavailable, errors.Unavailable, "service unavailable"},
		{"passthrough", codes.NotFound, errors.NotFound, "downstream missing"},
		{"panic", codes.Internal, errors.Internal, "internal error"},
	}
	for _, tt := range tests {
		_, err := e.echo(context.Background(), tt.in)
		s, _ := status.FromError(err)
		if status.Code(err) != tt.code || s.Message() != tt.message || errors.KindOf(err) != tt.kind {
			t.Errorf("%s: code %v message %q kind %v; want %v %q %v",
				tt.in, status.Code(err), s.Message(), errors.KindOf(err), tt.code, tt.message, tt.kind)
		}
	}
	if !e.logs.Contains(secretDetail) {
		t.Error("full error missing from the server log")
	}
	if out, err := e.echo(context.Background(), "after-panic"); err != nil || out != "after-panic" {
		t.Errorf("server unusable after a panic: %v", err)
	}
}

func TestAccessLogAndRequestID(t *testing.T) {
	e := setup(t, nil)

	ctx := requestid.NewContext(context.Background(), "req-from-client")
	if got, err := e.echo(ctx, "reqid"); err != nil || got != "req-from-client" {
		t.Fatalf("propagated request ID = %q, %v", got, err)
	}

	var trailer metadata.MD
	got, err := e.echo(context.Background(), "reqid", grpc.Trailer(&trailer))
	if err != nil || !requestid.Valid(got) || len(trailer.Get("x-request-id")) == 0 || trailer.Get("x-request-id")[0] != got {
		t.Fatalf("generated request ID %q, trailer %v, %v", got, trailer, err)
	}

	_, _ = e.echo(context.Background(), "panic")
	logs := e.accessLogs(t)
	if len(logs) != 3 {
		t.Fatalf("got %d access logs", len(logs))
	}
	first, last := logs[0], logs[2]
	if first["grpc.method"] != "/test.Echo/Echo" || first["grpc.code"] != "OK" || first["level"] != "INFO" ||
		first["request_id"] != "req-from-client" || first["trace_id"] == nil {
		t.Errorf("access log = %v", first)
	}
	if last["grpc.code"] != "Internal" || last["level"] != "ERROR" || last["error_type"] != "internal" {
		t.Errorf("panic access log = %v", last)
	}
	if !e.logs.Contains(`"stack"`) {
		t.Error("panic logged without stack")
	}
}

func TestAuth(t *testing.T) {
	checks := health.New(health.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	checks.MarkStarted()
	auth := func(ctx context.Context, _ string) (context.Context, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		switch strings.Join(md.Get("authorization"), "") {
		case "Bearer good":
			return context.WithValue(ctx, userKey{}, "u1"), nil
		case "Bearer readonly":
			return nil, errors.Forbidden.New("read-only token")
		case "":
			return nil, errors.Unauthorized.New("missing token")
		default:
			return nil, stderrors.New("jwt: bad signature for key k-7")
		}
	}
	e := setup(t, []grpcserver.Option{grpcserver.WithAuth(auth), grpcserver.WithHealth(checks)})

	withToken := func(tok string) context.Context {
		return metadata.AppendToOutgoingContext(context.Background(), "authorization", tok)
	}
	if got, err := e.echo(withToken("Bearer good"), "user"); err != nil || got != "u1" {
		t.Errorf("good token: %q, %v", got, err)
	}
	for tok, want := range map[string]codes.Code{
		"Bearer readonly": codes.PermissionDenied, "Bearer forged": codes.Unauthenticated,
	} {
		_, err := e.echo(withToken(tok), "user")
		if status.Code(err) != want || strings.Contains(err.Error(), "k-7") {
			t.Errorf("%s: %v", tok, err)
		}
	}
	if _, err := e.echo(context.Background(), "user"); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no token: %v", err)
	}
	// The health service is public.
	resp, err := healthpb.NewHealthClient(e.conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health without token: %v, %v", resp, err)
	}
}

func TestTracingAndMetrics(t *testing.T) {
	e := setup(t, nil)
	if _, err := e.echo(context.Background(), "ok"); err != nil {
		t.Fatal(err)
	}

	var client, server sdktrace.ReadOnlySpan
	for _, s := range e.spans.Ended() {
		switch s.SpanKind().String() {
		case "client":
			client = s
		case "server":
			server = s
		}
	}
	if client == nil || server == nil {
		t.Fatalf("spans: %v", e.spans.Ended())
	}
	if server.Parent().SpanID() != client.SpanContext().SpanID() || server.SpanContext().TraceID() != client.SpanContext().TraceID() {
		t.Error("server span is not a child of the client span")
	}

	if !e.metrics.Has("rpc.server.call.duration") || !e.metrics.Has("rpc.client.call.duration") {
		t.Error("rpc.server.call.duration or rpc.client.call.duration not recorded")
	}
}

func TestDefaultDeadline(t *testing.T) {
	e := setup(t, nil, grpcclient.WithTimeout(100*time.Millisecond))
	start := time.Now()
	_, err := e.echo(context.Background(), "slow")
	if errors.KindOf(err) != errors.Timeout || time.Since(start) > time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	// An explicit deadline wins.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, _ = e.echo(ctx, "slow")
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("explicit 300ms deadline cut short at %v", d)
	}
}

func TestRetry(t *testing.T) {
	e := setup(t, nil)
	if _, err := e.echo(context.Background(), "flaky"); errors.KindOf(err) != errors.Unavailable {
		t.Fatalf("without retry: %v", err)
	}

	e = setup(t, nil, grpcclient.WithRetry(grpcclient.RetryPolicy{MaxAttempts: 3, InitialBackoff: 10 * time.Millisecond}))
	if got, err := e.echo(context.Background(), "flaky"); err != nil || got != "ok" {
		t.Fatalf("with retry: %q, %v", got, err)
	}
	if n := e.server.flaky.Load(); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}

	if _, err := grpcclient.New("passthrough:///x", grpcclient.WithRetry(grpcclient.RetryPolicy{MaxAttempts: 9})); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("invalid policy: %v", err)
	}
}

func (e *env) count(t *testing.T, ctx context.Context, arg string) ([]string, error) {
	t.Helper()
	stream, err := e.conn.NewStream(ctx, &echoDesc.Streams[0], "/test.Echo/Count")
	if err != nil {
		return nil, err
	}
	if err := stream.SendMsg(wrapperspb.String(arg)); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	var got []string
	for {
		m := new(wrapperspb.StringValue)
		err := stream.RecvMsg(m)
		if err == io.EOF { //nolint:errorlint // io.EOF must be returned unwrapped by streams
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, m.GetValue())
	}
}

func TestStreams(t *testing.T) {
	e := setup(t, nil)
	got, err := e.count(t, context.Background(), "ok")
	if err != nil || strings.Join(got, ",") != "x,xx,xxx" {
		t.Fatalf("stream = %v, %v", got, err)
	}
	got, err = e.count(t, context.Background(), "fail")
	if len(got) != 1 || status.Code(err) != codes.ResourceExhausted || errors.KindOf(err) != errors.RateLimited {
		t.Fatalf("failing stream = %v, %v", got, err)
	}
}

func TestHealthService(t *testing.T) {
	checks := health.New(health.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), health.WithCacheTTL(0))
	e := setup(t, []grpcserver.Option{grpcserver.WithHealth(checks)})
	hc := healthpb.NewHealthClient(e.conn)
	state := func() healthpb.HealthCheckResponse_ServingStatus {
		resp, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetStatus()
	}
	if state() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Error("serving before start")
	}
	checks.MarkStarted()
	if state() != healthpb.HealthCheckResponse_SERVING {
		t.Error("not serving after start")
	}
	_ = checks.Drain(context.Background())
	if state() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Error("serving while draining")
	}
}

func TestServeGracefulStop(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := grpcserver.New(grpcserver.WithLogger(quiet))
	srv.RegisterService(&echoDesc, &echoServer{})
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := graceful.New(graceful.WithSignals(), graceful.WithLogger(quiet), graceful.WithTimeout(time.Second))
	if err := grpcserver.ServeListener(g, srv, ln); err != nil {
		t.Fatal(err)
	}
	conn, err := grpcclient.New(ln.Addr().String(), grpcclient.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	e := &env{conn: conn}

	slow := make(chan error, 1)
	go func() {
		out, err := e.echo(context.Background(), "sleep")
		if err == nil && out != "slept" {
			err = stderrors.New("wrong reply " + out)
		}
		slow <- err
	}()
	forever := make(chan error, 1)
	go func() {
		_, err := e.count(t, context.Background(), "forever")
		forever <- err
	}()
	time.Sleep(50 * time.Millisecond) // both calls in flight

	start := time.Now()
	err = g.Shutdown(context.Background())
	if errors.KindOf(err) != errors.Timeout {
		t.Errorf("shutdown with a never-ending stream = %v, want timeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("shutdown took %v", d)
	}
	if err := <-slow; err != nil {
		t.Errorf("in-flight unary call failed during graceful stop: %v", err)
	}
	if err := <-forever; status.Code(err) != codes.Unavailable && status.Code(err) != codes.Canceled {
		t.Errorf("stream at forced stop: %v", err)
	}
}

func TestTLSIsDefault(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := grpcserver.New(grpcserver.WithLogger(quiet)) // plaintext server
	srv.RegisterService(&echoDesc, &echoServer{})
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpcclient.New("passthrough:///bufnet", grpcclient.WithDialOptions(
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/test.Echo/Echo", wrapperspb.String("x"), new(wrapperspb.StringValue))
	if err == nil {
		t.Fatal("TLS client talked to a plaintext server")
	}
}

func TestGRPCStatusRoundTrip(t *testing.T) {
	for k := errors.Unknown; k <= errors.Internal; k++ {
		code := grpcstatus.CodeFor(k)
		back := grpcstatus.KindFor(code)
		if k != errors.Unknown && back != k {
			t.Errorf("kind %v -> %v -> %v", k, code, back)
		}
	}
	if grpcstatus.ToStatus(nil) != nil || grpcstatus.FromStatus(nil) != nil || grpcstatus.Code(nil) != codes.OK {
		t.Error("nil handling")
	}
	orig := status.Error(codes.NotFound, "x")
	classified := grpcstatus.FromStatus(orig)
	if !errors.Is(classified, orig) || status.Code(classified) != codes.NotFound {
		t.Error("FromStatus lost the original status")
	}
	if errors.KindOf(grpcstatus.FromStatus(context.DeadlineExceeded)) != errors.Timeout {
		t.Error("context error not classified")
	}
}
