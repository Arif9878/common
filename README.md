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

**Start here:** [`examples/orders-service`](examples/orders-service) is a complete service built the intended way: fx, Echo, PostgreSQL with the transactional outbox, an idempotent Kafka consumer, OTLP observability, a Dockerfile, docker-compose with Grafana, and a Kubernetes manifest. [`go/ENVIRONMENT.md`](go/ENVIRONMENT.md) lists every configuration variable.

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

With Uber fx, [`go/fx`](go/fx) does this wiring for you:

```go
fx.New(
	commonfx.Config[Config](),
	commonfx.ConfigFields[Config](),
	commonfx.Observability(),
	commonfx.Lifecycle(),
	commonfx.AdminServer(), // /live, /ready, /startup, /metrics on :9090
	commonfx.EchoServer(),  // *echo.Echo with the standard middleware and errors
	fx.Invoke(func(e *echo.Echo) {
		e.GET("/orders/:id", getOrder)
	}),
	commonfx.Ready(), // always last
).Run()
```

## Observability backends

Traces, metrics and logs use OpenTelemetry, and every exporter is chosen by configuration. Moving between Grafana, New Relic, Datadog or another backend therefore changes environment variables, not code. Put the shared OTLP settings in your configuration next to the per-signal ones:

```go
type Config struct {
	OTLP    otlp.Config    `envPrefix:"OTLP_"`    // endpoint, headers (API key), protocol, TLS
	Log     logging.Config `envPrefix:"LOG_"`     // OUTPUT: stdout | otlp | both
	Logs    logs.Config    `envPrefix:"LOGS_"`    // EXPORTER: none | otlp
	Tracing tracing.Config `envPrefix:"TRACING_"` // EXPORTER: none | otlp
	Metrics metrics.Config `envPrefix:"METRICS_"` // EXPORTER: prometheus | otlp | prometheus,otlp
}
```

The fx `Observability()` module picks these up when they're in the graph. Without fx, pass `tracing.WithOTLP(cfg.OTLP)`, `metrics.WithOTLP(cfg.OTLP)` and `logs.WithOTLP(cfg.OTLP)`, then `logging.WithLoggerProvider(lp.LoggerProvider())`. `OTLP_HEADERS` is a secret and never appears in logs. Name the service with `*_SERVICE`, `*_ENVIRONMENT` and `*_VERSION`, or with `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`.

**Grafana stack (self-hosted, through Grafana Alloy or an OpenTelemetry Collector)**

```sh
OTLP_ENDPOINT=alloy:4317  OTLP_INSECURE=true
TRACING_EXPORTER=otlp                 # to Tempo
METRICS_EXPORTER=prometheus           # Alloy scrapes /metrics into Mimir (or: otlp)
LOG_OUTPUT=stdout                     # Alloy tails container logs into Loki (or: both, with LOGS_EXPORTER=otlp)
```

**Grafana Cloud**

```sh
OTLP_PROTOCOL=http
OTLP_ENDPOINT=https://otlp-gateway-prod-<region>.grafana.net/otlp
OTLP_HEADERS=Authorization=Basic%20<base64 of instanceID:token>
TRACING_EXPORTER=otlp  METRICS_EXPORTER=otlp  LOGS_EXPORTER=otlp  LOG_OUTPUT=both
```

**New Relic**

```sh
OTLP_PROTOCOL=http
OTLP_ENDPOINT=https://otlp.nr-data.net:4318          # EU accounts: https://otlp.eu01.nr-data.net:4318
OTLP_HEADERS=api-key=<license key>
TRACING_EXPORTER=otlp  METRICS_EXPORTER=otlp  LOGS_EXPORTER=otlp  LOG_OUTPUT=both
METRICS_TEMPORALITY=delta                             # New Relic's preferred temporality
LOG_TRACE_ID_KEY=trace.id  LOG_SPAN_ID_KEY=span.id    # logs-in-context for stdout logs shipped by the NR agent
```

**Datadog (through the Datadog Agent's OTLP intake)**

```sh
OTLP_ENDPOINT=datadog-agent:4317  OTLP_INSECURE=true
TRACING_EXPORTER=otlp  METRICS_EXPORTER=otlp  METRICS_TEMPORALITY=delta
LOG_OUTPUT=stdout                                     # the Agent collects container logs
```

**Any other OTLP backend** (Honeycomb, Elastic, Dynatrace, a Collector): set `OTLP_ENDPOINT` and the `OTLP_HEADERS` it requires, then choose the exporters. Settings you leave empty fall back to the standard `OTEL_EXPORTER_OTLP_*` variables.

**Docker and Kubernetes**

Everything above is read from the environment, so it belongs in the deployment, not in the image. Set the service's identity once, with the standard OpenTelemetry variables. Traces, metrics, OTLP logs and the `service`, `environment` and `version` fields of stdout logs all read them; `*_SERVICE`, `*_ENVIRONMENT` and `*_VERSION` still override them:

| Variable | Example |
|---|---|
| `OTEL_SERVICE_NAME` | `orders` |
| `OTEL_RESOURCE_ATTRIBUTES` | `service.version=1.4.2,deployment.environment.name=production,k8s.pod.name=…` |

```yaml
# docker-compose.yml
services:
  orders:
    image: registry.example/orders:1.4.2
    environment:
      OTEL_SERVICE_NAME: orders
      OTEL_RESOURCE_ATTRIBUTES: service.version=1.4.2,deployment.environment.name=staging
      OTLP_ENDPOINT: otel-collector:4317
      OTLP_INSECURE: "true"
      TRACING_EXPORTER: otlp
      METRICS_EXPORTER: prometheus,otlp
```

```yaml
# Kubernetes: the name and version come from the Pod's labels, set once by the chart.
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: orders
        app.kubernetes.io/version: "1.4.2"
    spec:
      containers:
        - name: orders
          image: registry.example/orders:1.4.2
          envFrom:
            - configMapRef: { name: observability }     # OTLP_ENDPOINT, *_EXPORTER, LOG_OUTPUT: shared by every service
            - secretRef: { name: otlp-credentials }     # OTLP_HEADERS=api-key=…
          env:
            - name: OTEL_SERVICE_NAME
              valueFrom: { fieldRef: { fieldPath: "metadata.labels['app.kubernetes.io/name']" } }
            - name: SERVICE_VERSION
              valueFrom: { fieldRef: { fieldPath: "metadata.labels['app.kubernetes.io/version']" } }
            - name: POD_NAME
              valueFrom: { fieldRef: { fieldPath: metadata.name } }
            - name: POD_NAMESPACE
              valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
            - name: OTEL_RESOURCE_ATTRIBUTES
              value: service.version=$(SERVICE_VERSION),deployment.environment.name=production,k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE)
```

**Notes**

- **Endpoint URLs:** with the HTTP protocol, a base URL such as `https://otlp.nr-data.net:4318` or `…/otlp` gets `/v1/traces`, `/v1/metrics` or `/v1/logs` appended.
- **Per-signal overrides:** `TRACING_ENDPOINT`, `METRICS_ENDPOINT` and `LOGS_ENDPOINT` override the shared endpoint for one signal. A per-signal URL is used as is.
- **Push interval:** metrics are pushed every `METRICS_EXPORT_INTERVAL` (60s by default).
- **Moving between backends:** `METRICS_EXPORTER=prometheus,otlp` serves `/metrics` and pushes at the same time, so you can run both while you switch.

## Versions and modules

The core module holds the light packages. Each heavy integration is its own module, so a service only downloads and builds the dependencies it uses: pgx, go-redis, franz-go, the Vault API, the gRPC server and client packages, and Uber fx. Core still depends on the gRPC library itself, because its OTLP exporters can send over gRPC.

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
make test COVER=1 && make cover-summary   # coverage per module, counting every package a test exercises
make envdoc                        # regenerate ENVIRONMENT.md after changing a configuration struct
make examples                      # build, lint and test examples/ against this checkout
```

Integration tests run against real infrastructure when it is configured, and are skipped otherwise:

| Variable | Used by |
|---|---|
| `POSTGRES_TEST_URL=postgres://user:pass@host:5432/db` | `datastore/postgres`, `idempotency/pgstore`, `lock/pglock`, `testkit/pgtest` |
| `REDIS_TEST_ADDR=host:6379` | `datastore/redis`, `idempotency/redisstore`, `lock/redislock`. These also run on miniredis without it; with it, the Lua scripts run on real Redis. Keys get a random `commontest:` prefix. |
| `KAFKA_TEST_BROKERS=host:9092` (`make test-kafka`) | `messaging/kafka`. Without it, the tests use an in-process fake. Tests create and delete their own `commontest-*` topics, so a shared broker is safe to use. |

CI runs every module on Go 1.26 and 1.27, with PostgreSQL and Redis, reports coverage per module in the job summary, and runs the Kafka tests against a Redpanda container. A nightly job runs govulncheck on `main` with the newest Go and opens (or updates) an issue when it finds a vulnerability. Production packages must not import `testkit`; a lint rule enforces this.
