package commonfx_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Arif9878/common/go/testkit"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func TestTelemetryIsWiredAutomatically(t *testing.T) {
	httpAddr, grpcAddr := testkit.FreeAddr(t), testkit.FreeAddr(t)
	tp, spans := testkit.NewTracer(t)
	logger, logs := testkit.NewLogger(t)

	var client *http.Client
	var conn *grpc.ClientConn
	app := fxtest.New(t,
		// What Observability() provides, supplied directly so the test can
		// inspect spans and logs.
		fx.Supply(logger),
		fx.Supply(fx.Annotate(tp, fx.As(new(trace.TracerProvider)))),
		fx.Supply(fx.Annotate(propagation.TraceContext{}, fx.As(new(propagation.TextMapPropagator)))),

		fx.Supply(httpserver.Config{Addr: httpAddr}, commonfx.GRPCConfig{Addr: grpcAddr}),
		fx.Supply(fx.Annotate(commonfx.GRPCClientConfig{Target: grpcAddr, Insecure: true}, fx.ResultTags(`name:"self-grpc"`))),
		commonfx.Lifecycle(),
		commonfx.GRPCServer(),
		commonfx.HTTPServer(), // standard middleware applied automatically
		commonfx.GRPCClient("self-grpc"),
		commonfx.HTTPClient("self-http"),

		// The service: GET /check calls the gRPC health service.
		fx.Provide(fx.Annotate(func(conn *grpc.ClientConn) *http.ServeMux {
			mux := http.NewServeMux()
			hc := healthpb.NewHealthClient(conn)
			mux.HandleFunc("GET /check/{svc}", func(w http.ResponseWriter, r *http.Request) {
				resp, err := hc.Check(r.Context(), &healthpb.HealthCheckRequest{})
				if err != nil {
					httpserver.WriteError(w, r, err)
					return
				}
				_, _ = io.WriteString(w, resp.GetStatus().String())
			})
			return mux
		}, fx.ParamTags(`name:"self-grpc"`), fx.As(new(http.Handler)))),

		fx.Invoke(fx.Annotate(func(c *http.Client, cc *grpc.ClientConn) { client, conn = c, cc },
			fx.ParamTags(`name:"self-http"`, `name:"self-grpc"`))),
		commonfx.Ready(),
	)
	app.RequireStart()

	ctx, root := tp.Tracer("test").Start(context.Background(), "test")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+"/check/orders-123", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	root.End()
	if resp.StatusCode != 200 || string(body) != "SERVING" {
		t.Fatalf("GET /check = %d %q", resp.StatusCode, body)
	}

	// One trace: test -> HTTP client -> HTTP server -> gRPC client -> gRPC server.
	byKind := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans.Ended() {
		key := s.SpanKind().String()
		if strings.Contains(s.Name(), "Health") {
			key = "grpc-" + key
		}
		byKind[key] = s
	}
	prev := ""
	for _, k := range []string{"internal", "client", "server", "grpc-client", "grpc-server"} {
		s, ok := byKind[k]
		if !ok {
			t.Fatalf("no %s span; spans: %v", k, spans.Ended())
		}
		if s.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("%s span %q is in another trace", k, s.Name())
		}
		if prev != "" && s.Parent().SpanID() != byKind[prev].SpanContext().SpanID() {
			t.Errorf("%s span %q is not a child of the %s span", k, s.Name(), prev)
		}
		prev = k
	}
	if name := byKind["server"].Name(); name != "GET /check/{svc}" {
		t.Errorf("HTTP server span named %q, want the route template", name)
	}

	out := logs.String()
	for _, want := range []string{`"msg":"http request"`, `"route":"/check/{svc}"`, `"msg":"grpc call"`, `"grpc.method":"/grpc.health.v1.Health/Check"`} {
		if !strings.Contains(out, want) {
			t.Errorf("logs missing %s", want)
		}
	}
	if strings.Contains(out, "orders-123") {
		t.Error("raw path logged")
	}

	app.RequireStop()
	if st := conn.GetState(); st != connectivity.Shutdown {
		t.Errorf("gRPC client connection state after stop = %v", st)
	}
}

func TestHTTPHandlerAsIs(t *testing.T) {
	addr := testkit.FreeAddr(t)
	logger, logs := testkit.NewLogger(t)
	app := fxtest.New(t,
		fx.Supply(logger, httpserver.Config{Addr: addr}),
		fx.Provide(fx.Annotate(func() *http.ServeMux {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
			return mux
		}, fx.As(new(http.Handler)))),
		commonfx.Lifecycle(),
		commonfx.HTTPServer(commonfx.HTTPHandlerAsIs()),
		commonfx.Ready(),
	)
	app.RequireStart()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	app.RequireStop()
	if resp.Header.Get("X-Request-ID") != "" || strings.Contains(logs.String(), `"msg":"http request"`) {
		t.Error("middleware applied despite HTTPHandlerAsIs")
	}
}
