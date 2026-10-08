package commonfx_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/redis"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/tracing"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/vault"
	"github.com/Arif9878/common/go/transport/grpc/grpcclient"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// AppConfig is a service configuration composed of platform configs.
type AppConfig struct {
	Log     logging.Config       `envPrefix:"LOG_"`
	Tracing tracing.Config       `envPrefix:"TRACING_"`
	Metrics metrics.Config       `envPrefix:"METRICS_"`
	HTTP    httpserver.Config    `envPrefix:"HTTP_"`
	Admin   commonfx.AdminConfig `envPrefix:"ADMIN_"`
	Redis   redis.Config         `envPrefix:"REDIS_"`
	Kafka   kafka.Config         `envPrefix:"KAFKA_"`
}

// events records the shutdown order across graceful phases and fx hooks.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *events) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.log...)
}

func TestFullApplication(t *testing.T) {
	m := miniredis.RunT(t)
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "orders"))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	httpAddr, adminAddr := freeAddr(t), freeAddr(t)

	env := map[string]string{
		"LOG_LEVEL":     "error",
		"HTTP_ADDR":     httpAddr,
		"ADMIN_ADDR":    adminAddr,
		"REDIS_ADDRS":   m.Addr(),
		"KAFKA_BROKERS": strings.Join(cluster.ListenAddrs(), ","),
	}
	ev := &events{}
	consumed := make(chan string, 1)
	var readyDuringStart health.Status
	var producer *kafka.Producer

	app := fxtest.New(t,
		commonfx.Config[AppConfig](config.WithEnvironment(env)),
		commonfx.ConfigFields[AppConfig](),
		commonfx.Observability(),
		commonfx.Lifecycle(commonfx.WithShutdownTimeout(10*time.Second)),
		commonfx.AdminServer(),
		commonfx.HTTPServer(),
		commonfx.Redis(),
		commonfx.KafkaProducer(),
		commonfx.KafkaConsumer("billing", []string{"orders"}, func() kafka.Handler {
			return func(_ context.Context, r *kgo.Record) error {
				consumed <- string(r.Value)
				return nil
			}
		}),
		fx.Provide(fx.Annotate(func(rdb *redis.Client) *http.ServeMux {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /hits", func(w http.ResponseWriter, r *http.Request) {
				n, err := rdb.Incr(r.Context(), "hits").Result()
				if err != nil {
					httpserver.WriteError(w, r, err)
					return
				}
				_ = json.NewEncoder(w).Encode(n)
			})
			return mux
		}, fx.As(new(http.Handler)))),
		fx.Invoke(func(lc fx.Lifecycle, g *graceful.Manager, checks *health.Checker) {
			// An fx stop hook registered before Ready must run after the graceful phases.
			lc.Append(fx.Hook{
				OnStart: func(ctx context.Context) error {
					readyDuringStart = checks.Ready(ctx).Status
					return nil
				},
				OnStop: func(context.Context) error { ev.add("fx-stop-hook"); return nil },
			})
			for _, p := range []graceful.Phase{graceful.Unready, graceful.StopIntake, graceful.Drain, graceful.CloseDeps, graceful.Telemetry} {
				_ = g.Register(p, "recorder", func(context.Context) error { ev.add(p.String()); return nil })
			}
		}),
		fx.Populate(&producer),
		commonfx.Ready(),
		fx.StopTimeout(15*time.Second),
	)
	app.RequireStart()

	if readyDuringStart != health.StatusFail {
		t.Error("service was ready before every component had started")
	}
	if code, _ := get(t, "http://"+adminAddr+"/ready"); code != 200 {
		t.Fatalf("/ready = %d after start", code)
	}
	if code, body := get(t, "http://"+adminAddr+"/metrics"); code != 200 || !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics = %d", code)
	}
	if code, body := get(t, "http://"+httpAddr+"/hits"); code != 200 || strings.TrimSpace(body) != "1" {
		t.Fatalf("/hits = %d %q", code, body)
	}

	// The producer from the graph publishes; the consumer run by Lifecycle receives.
	if err := producer.Publish(context.Background(), &kgo.Record{Topic: "orders", Value: []byte("o-1")}); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-consumed:
		if v != "o-1" {
			t.Errorf("consumed %q", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("consumer did not receive the record")
	}

	app.RequireStop()
	got := ev.list()
	want := []string{"unready", "stop_intake", "drain", "close_deps", "telemetry", "fx-stop-hook"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("shutdown order = %v, want %v", got, want)
	}
}

func TestReadyIsRequired(t *testing.T) {
	app := fx.New(commonfx.Lifecycle(), fx.NopLogger)
	err := app.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "commonfx.Ready()") {
		t.Fatalf("Start without Ready = %v", err)
	}
}

func TestConfigFields(t *testing.T) {
	type Dup struct {
		A logging.Config
		B logging.Config
	}
	app := fx.New(fx.NopLogger,
		fx.Supply(Dup{}),
		commonfx.ConfigFields[Dup](),
		fx.Invoke(func(logging.Config) {}),
	)
	if app.Err() == nil {
		t.Error("duplicate field types accepted")
	}

	app = fx.New(fx.NopLogger, commonfx.ConfigFields[int]())
	if err := app.Err(); err == nil || !strings.Contains(err.Error(), "not a struct") {
		t.Errorf("non-struct: %v", err)
	}

	var got logging.Config
	app = fx.New(fx.NopLogger,
		commonfx.Config[AppConfig](config.WithEnvironment(map[string]string{"LOG_LEVEL": "warn", "KAFKA_BROKERS": "x:1"})),
		commonfx.ConfigFields[AppConfig](),
		fx.Populate(&got),
	)
	if err := app.Err(); err != nil || got.Level != "warn" {
		t.Fatalf("field = %+v, %v", got, err)
	}
}

func TestConfigErrorsFailConstruction(t *testing.T) {
	app := fx.New(fx.NopLogger,
		commonfx.Config[AppConfig](config.WithEnvironment(map[string]string{})), // KAFKA_BROKERS required
		fx.Invoke(func(AppConfig) {}),
	)
	if err := app.Err(); err == nil || !strings.Contains(err.Error(), "KAFKA_BROKERS") {
		t.Fatalf("Err = %v", err)
	}
}

func TestVault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/token/lookup-self":
			_, _ = io.WriteString(w, `{"data":{"ttl":0,"renewable":false}}`)
		case "/v1/secret/data/app":
			_, _ = io.WriteString(w, `{"data":{"data":{"key":"v"},"metadata":{"version":1}}}`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"errors":[]}`)
		}
	}))
	defer srv.Close()

	var provider secret.Provider
	app := fxtest.New(t,
		fx.Supply(vault.Config{Address: srv.URL, Token: "root", MaxRetries: 0}),
		commonfx.Lifecycle(),
		commonfx.Vault(),
		fx.Populate(&provider),
		commonfx.Ready(),
	)
	app.RequireStart()
	s, err := provider.Get(context.Background(), "secret/data/app")
	if err != nil || s.Field("key") != "v" {
		t.Fatalf("Get = %v, %v", s, err)
	}
	app.RequireStop()
}

func TestGRPCServer(t *testing.T) {
	addr := freeAddr(t)
	var g *graceful.Manager
	app := fxtest.New(t,
		fx.Supply(commonfx.GRPCConfig{Addr: addr}),
		commonfx.Lifecycle(),
		commonfx.GRPCServer(),
		fx.Invoke(func(*grpc.Server) {}), // register services here
		fx.Populate(&g),
		commonfx.Ready(),
	)
	app.RequireStart()

	conn, err := grpcclient.New(addr, grpcclient.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	hc := healthpb.NewHealthClient(conn)
	resp, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health after start = %v, %v", resp, err)
	}
	app.RequireStop()
	if _, err := hc.Check(context.Background(), &healthpb.HealthCheckRequest{}); err == nil {
		t.Error("gRPC server still serving after stop")
	}
}

func TestPostgres(t *testing.T) {
	raw := os.Getenv("POSTGRES_TEST_URL")
	if raw == "" {
		t.Skip("POSTGRES_TEST_URL not set")
	}
	u, _ := url.Parse(raw)
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	var db *postgres.DB
	var checks *health.Checker
	app := fxtest.New(t,
		fx.Supply(postgres.Config{Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
			User: u.User.Username(), Password: config.Secret(pw), SSLMode: "disable"}),
		commonfx.Lifecycle(),
		commonfx.Postgres(),
		fx.Populate(&db, &checks),
		commonfx.Ready(),
	)
	app.RequireStart()
	if rep := checks.Ready(context.Background()); rep.Checks["postgres"].Status != health.StatusOK {
		t.Errorf("readiness = %+v", rep)
	}
	var one int
	if err := db.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatal(err)
	}
	app.RequireStop()
	if err := db.Ping(context.Background()); err == nil {
		t.Error("pool still open after stop")
	}
}
