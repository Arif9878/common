# common

The organization's shared Go library for microservices. It covers configuration, observability, lifecycle, resilience, concurrency, transports, secrets, datastores, messaging and coordination, so every service handles these concerns the same, production-tested way.

```sh
go get github.com/Arif9878/common/go@latest                  # core
go get github.com/Arif9878/common/go/messaging/kafka@latest  # integrations are separate modules, see below
go get github.com/Arif9878/common/go/fx@latest               # optional Uber fx integration
```

It requires **Go 1.26** or newer. Every package emits OpenTelemetry traces and metrics, classifies its errors with `errors.Kind`, and never logs secrets, request bodies or query parameters.

## Packages

| Area | Package | What it does |
|---|---|---|
| Errors | [`errors`](go/errors) | Error kinds (`NotFound`, `Unavailable`, …), `KindOf`, `IsRetryable`, public-safe messages |
| Configuration | [`config`](go/config) | Typed `Load[T]` from the environment, validation, `Secret` values that never print |
| Observability | [`observability/logging`](go/observability/logging) | `log/slog` logger with trace and request IDs and key redaction |
| | [`observability/tracing`](go/observability/tracing) | OpenTelemetry tracing over OTLP |
| | [`observability/metrics`](go/observability/metrics) | OpenTelemetry metrics scraped as Prometheus `/metrics` |
| | [`requestid`](go/requestid) | Request ID carried through `context.Context` |
| Lifecycle | [`lifecycle/graceful`](go/lifecycle/graceful) | Phased, bounded shutdown on SIGTERM |
| | [`health`](go/health) | Liveness, readiness and startup probes |
| Resilience | [`resilience/retry`](go/resilience/retry), [`circuitbreaker`](go/resilience/circuitbreaker), [`ratelimit`](go/resilience/ratelimit) | Retries for transient errors, failing fast, rate limits |
| Concurrency | [`concurrency/workerpool`](go/concurrency/workerpool), [`batch`](go/concurrency/batch) | Bounded worker pools; batching by size and time |
| HTTP | [`transport/http/httpserver`](go/transport/http/httpserver) | Standard middleware, problem+json errors, graceful serving |
| | [`transport/http/httpclient`](go/transport/http/httpclient) | Pooling, safe retries, circuit breaker |
| | [`transport/http/echoadapter`](go/transport/http/echoadapter) | The same middleware and errors for Echo (the organization's standard framework) |
| gRPC | [`transport/grpc`](go/transport/grpc) | `grpcserver`, `grpcclient`, `grpcstatus` (error kinds ↔ status codes) |
| Secrets | [`secret`](go/secret), [`secret/rotation`](go/secret/rotation), [`secret/vault`](go/secret/vault) | Provider-neutral secrets, zero-downtime credential rotation, Vault |
| Datastores | [`datastore/postgres`](go/datastore/postgres), [`datastore/redis`](go/datastore/redis) | pgx and go-redis clients with telemetry, health and credential rotation |
| Messaging | [`messaging/kafka`](go/messaging/kafka) | franz-go producer and consumer: at-least-once, per-partition order, bounded concurrency, batches, DLQ, idempotency; [`kafkaproto`](go/messaging/kafka/kafkaproto) for Protobuf with a Schema Registry |
| | [`messaging/outbox`](go/messaging/outbox) | Transactional outbox: publish Kafka records if and only if a PostgreSQL transaction commits |
| Coordination | [`idempotency`](go/idempotency) | Run once per key; PostgreSQL and Redis stores |
| | [`lock`](go/lock) | Leases with fencing tokens; PostgreSQL and Redis |
| | [`featureflag`](go/featureflag) | Feature flags through OpenFeature |
| Testing | [`testkit`](go/testkit) | Capture logs, metrics and spans in tests; `Eventually`; PostgreSQL test databases |
| Uber fx | [`fx`](go/fx) (separate module) | Provides all of the above to an fx application, with telemetry and shutdown wired in |

Each package's Go documentation describes its guarantees, failure behavior and limits: `go doc github.com/Arif9878/common/go/<package>`. The design notes and conventions are in [`docs/go-platform-proposal.md`](docs/go-platform-proposal.md).

## A service in a few lines

```go
type Config struct {
	Log     logging.Config    `envPrefix:"LOG_"`
	Tracing tracing.Config    `envPrefix:"TRACING_"`
	Metrics metrics.Config    `envPrefix:"METRICS_"`
	HTTP    httpserver.Config `envPrefix:"HTTP_"`
}

func run(ctx context.Context) error {
	cfg, err := config.Load[Config]()
	if err != nil {
		return err
	}
	log, err := logging.New(cfg.Log)
	if err != nil {
		return err
	}
	tp, err := tracing.Init(ctx, cfg.Tracing, tracing.WithGlobal())
	if err != nil {
		return err
	}
	mp, err := metrics.Init(ctx, cfg.Metrics, metrics.WithGlobal())
	if err != nil {
		return err
	}

	shutdown := graceful.New(graceful.WithLogger(log))
	checks := health.New()

	e := echo.New()
	e.HTTPErrorHandler = echoadapter.ErrorHandler
	e.Use(echoadapter.Middleware(httpserver.WithLogger(log)))
	e.GET("/live", echo.WrapHandler(checks.LiveHandler()))
	e.GET("/ready", echo.WrapHandler(checks.ReadyHandler()))
	e.GET("/metrics", echo.WrapHandler(mp.Handler()))
	// ... routes ...

	if err := httpserver.Serve(shutdown, httpserver.NewServer(cfg.HTTP, e, log)); err != nil {
		return err
	}
	_ = shutdown.Register(graceful.Unready, "readiness", checks.Drain)
	_ = shutdown.Register(graceful.Telemetry, "tracing", tp.Shutdown)
	_ = shutdown.Register(graceful.Telemetry, "metrics", mp.Shutdown)
	checks.MarkStarted()
	return shutdown.Wait(ctx)
}
```

With Uber fx, [`go/fx`](go/fx) does this wiring for you.

## Versions and modules

The core module holds the light packages. Each heavy integration is its own module, so a service only downloads and builds the dependencies it uses (pgx, go-redis, franz-go, the Vault API, gRPC, Uber fx):

| Module (`github.com/Arif9878/common/go/…`) | Brings in |
|---|---|
| `go` (core) | errors, config, observability, lifecycle, health, resilience, concurrency, HTTP, secret, idempotency and lock interfaces, featureflag, testkit |
| `go/datastore/postgres` | pgx |
| `go/datastore/redis` | go-redis |
| `go/idempotency/pgstore`, `go/lock/pglock` | PostgreSQL implementations |
| `go/idempotency/redisstore`, `go/lock/redislock` | Redis implementations |
| `go/messaging/kafka` | franz-go, including `kafkaproto` (Protobuf + Schema Registry) |
| `go/messaging/outbox` | Transactional outbox (PostgreSQL → Kafka) |
| `go/secret/vault` | HashiCorp Vault API |
| `go/transport/grpc` | gRPC (`grpcserver`, `grpcclient`, `grpcstatus`) |
| `go/testkit/pgtest` | PostgreSQL test databases |
| `go/fx` | Uber fx integration for all of the above |

Import paths are the same as before the split: `go get` the module that holds the package, for example `go get github.com/Arif9878/common/go/messaging/kafka@latest`.

All modules are released together with one version, and each is tagged with its folder: `go/v0.5.0`, `go/messaging/kafka/v0.5.0`, `go/fx/v0.5.0`. Use the same version for every module of this repository that a service requires. Before v1.0, minor versions may change behavior; release notes call out what changed and how to upgrade. See the [releases](https://github.com/Arif9878/common/releases) and [`go/RELEASING.md`](go/RELEASING.md).

The packages from `go/v0.1.0` (`logger`, `observability`, `http`, `http/echo/*`, `constant`, `utils`) are deprecated and will be removed in v1.0. [`go/MIGRATION.md`](go/MIGRATION.md) shows the replacement for each.

## Development

```sh
cd go
make check                         # tidy, vet, lint, test (race), govulncheck in every module: what CI runs
make test MODULES=messaging/kafka  # one module
```

Integration tests run against real infrastructure when it is configured, and are skipped otherwise:

| Variable | Used by |
|---|---|
| `POSTGRES_TEST_URL=postgres://user:pass@host:5432/db` | `datastore/postgres`, `idempotency/pgstore`, `lock/pglock`, `testkit/pgtest` |
| `KAFKA_TEST_BROKERS=host:9092` (`make test-kafka`) | `messaging/kafka`. Without it, the tests use an in-process fake. Tests create and delete their own `commontest-*` topics, so a shared broker is safe to use. |

CI runs both modules on Go 1.26 and 1.27, with PostgreSQL, and runs the Kafka tests against a Redpanda container. Production packages must not import `testkit`; a lint rule enforces this.
