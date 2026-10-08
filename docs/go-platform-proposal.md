# Go Platform Library — Architecture Proposal

Status: **Draft for review**. This is a proposal, not an implementation. Nothing below
is implemented yet; the existing packages under `go/` are unchanged.

---

## 0. Where we are today

`github.com/Arif9878/common/go` currently contains:

| Package | Problems that block reuse in high-traffic services |
|---|---|
| `logger` | logrus + a 15-method interface + a mutable global `Logger`. `ILogger` has an unexported method (`getLevel`) while `AppLogger` defines `GetLevel`, so no type, `AppLogger` included, can satisfy it from outside the package. |
| `observability` | `New` calls `WithOTLPExporter(ctx)` but throws the returned option away, so the exporter is never set. `Start` never creates a span (it re-wraps the parent). `Shutdown` swallows the error. Misconfiguration `panic`s. |
| `http/echo/middleware` | **JWT verified with the hard-coded key `"secret"`**. Auth is skipped when `APP_ENV=test`. Request ID stored under a string context key. Uses `satori/go.uuid` (unmaintained). |
| `http/echo/server` | Shutdown uses the already-cancelled context, so in-flight requests are cut off rather than drained. No readiness flip. |
| `http/context.go` | Spawns a goroutine that does nothing useful. |
| `constant`, `observability` | `DatastoreMySQL/DatastorePostgres` defined twice. |
| `utils` | `lib/pq` null helpers. Fine, but `lib/pq` is in maintenance mode. |

Toolchain: `go.mod` says `go 1.21.0`; the local toolchain is 1.27.

**Recommendation:** treat the current code as `v0` legacy. Freeze it, build the new
packages next to it, and remove the old ones after services migrate (§9).

---

## 1. Guiding decisions

1. **Primitives depend on stable APIs, not on our observability packages.**
   Every package that emits telemetry takes `*slog.Logger`, `metric.MeterProvider`
   and `trace.TracerProvider` as options. The defaults are the global or no-op
   providers. That is the OpenTelemetry guidance for libraries. `retry` and
   `workerpool` therefore depend only on the stdlib and `go.opentelemetry.io/otel/{metric,trace}`
   (API only, no SDK). Our `observability/*` packages exist to **bootstrap** the
   SDK in `main()` and to define naming conventions. They are not a runtime
   dependency of other packages.
2. **Standardize on `log/slog`.** No custom logger interface. Consumers accept `*slog.Logger`.
3. **OTel API for metrics and traces. The exporter is a deployment choice.** Use the
   OTel metrics API everywhere. `observability/metrics` wires either the Prometheus
   exporter (pull, `/metrics`) or OTLP (push). Services do not import `client_golang` directly.
4. **Framework-agnostic transports.** HTTP middleware is `func(http.Handler) http.Handler`.
   Echo, chi and Gin adapt it in one line (`echo.WrapMiddleware`). A thin
   `transport/http/echoadapter` exists only for migration.
5. **Functional options and typed `Config` structs.** `New(cfg Config, opts ...Option)`.
   `Config` holds what the environment sets; `Option` holds code-level wiring such as
   the logger, providers, clock and hooks. No package-level mutable state.
6. **Everything bounded.** Every queue, retry loop, batch and goroutine set has a cap,
   with a non-zero default. "Unlimited" is never a default and is never representable by the zero value.
7. **No clock abstraction. Tests use `testing/synctest`** (stable since Go 1.25).
   Inside a `synctest.Test` bubble, `time.Now`, timers and tickers run on a fake clock
   that advances once every goroutine in the bubble is blocked. Tests are deterministic and instant,
   even for code we don't own (gobreaker, `x/time/rate`), so packages call the `time`
   package directly and expose no `Clock` option.

---

## 2. Module layout

> **Update (2026-10-08): `go/fx` is a separate module** (`github.com/Arif9878/common/go/fx`,
> package `commonfx`), so fx, dig and zap stay out of services that do not use fx. Local
> development and CI build it against the core in this repository through a `replace` to `../`
> in `go/fx/go.mod`, which consumers ignore (no `go.work` needed; the repository's `.gitignore`
> excludes it). **Release step:** tag the core (`go/vX.Y.Z`) first, set the core
> `require` in `go/fx/go.mod` to that tag, then tag `go/fx/vX.Y.Z`.
>
> **Decision (2026-10-08): single module until v1.0.** The layout below is the
> v1.0 target. Package paths are chosen so the split does not change any import path.
> Until the split, `depguard` keeps the core packages from importing the heavy integrations.

A single module would make a service that needs only `retry` resolve franz-go,
the Vault SDK, pgx and gRPC versions through MVS. That couples upgrade cycles
across the company. Use one **core module** with light dependencies plus
**separate modules** for heavy integrations:

```text
go/                          module github.com/Arif9878/common/go        (core)
├── errors/                  stdlib only
├── config/                  caarlos0/env, go-playground/validator
├── lifecycle/graceful/
├── health/
├── resilience/{retry,circuitbreaker,ratelimit}
├── concurrency/{workerpool,batch}
├── idempotency/             core + in-memory store
├── lock/                    interface + docs only
├── featureflag/             interface (OpenFeature-backed)
├── secret/                  SecretProvider interface + Secret type
├── secret/rotation/
├── requestid/               request/correlation ID context plumbing (see §6)
├── observability/{logging,metrics,tracing}   OTel SDK + exporters
├── transport/http/          net/http + otelhttp
└── testkit/                 (renamed from testing/, see §6)

go/transport/grpc/           separate module (google.golang.org/grpc, go-grpc-middleware/v2)
go/messaging/kafka/          separate module (twmb/franz-go)
go/datastore/postgres/       separate module (jackc/pgx/v5)
go/datastore/redis/          separate module (redis/go-redis/v9)
go/secret/vault/             separate module (hashicorp/vault/api)
go/featureflag/flipt/        separate module (OpenFeature Flipt provider)
go/idempotency/{pgstore,redisstore}, go/lock/{pglock,redislock}  separate modules
```

Cost: more release tags (`go/messaging/kafka/v0.3.0`) and a `go.work` file for
local development. Use `go.work` in the repo and release-please or a tag script in CI.
If the team does not want multi-module releases yet, start as one module and split
before v1. The package paths stay the same either way, so splitting later does not break importers.

---

## 3. Dependency graph

Arrows mean "imports". Lower layers never import higher ones.

```text
L4  messaging/kafka   datastore/postgres   datastore/redis   transport/grpc   secret/vault
        │   │               │   │                │                 │              │
        │   └───────┐       │   └──────┐         │                 │              │
L3      │      transport/http│   secret/rotation  idempotency/*store  lock/*impl   │
        │            │       │         │                                          │
L2  ────┴──── resilience/{retry,circuitbreaker,ratelimit}  concurrency/{workerpool,batch}
        │            health   lifecycle/graceful   featureflag   requestid   secret
L1  errors    config
L0  stdlib · otel API (metric, trace) · log/slog

observability/logging sits at L2: it depends only on slog, the otel trace API,
requestid and errors, so any package may import it for field names, Err/Duration
and ContextWithAttrs.

Bootstrap only (imported by main(), never by the layers above):
    observability/{metrics,tracing}  →  otel SDK, exporters
```

Forbidden edges are enforced in CI (§8). Examples: `retry → kafka`, `batch → kafka`,
`rotation → vault`, `observability → anything above L1`, any production package `→ testkit`.

---

## 4. Public API sketch (per package)

These signatures are for review, not final.

### errors (classification, not catalogue) — ✅ implemented
```go
type Kind uint8
const ( Unknown Kind = iota; InvalidArgument; NotFound; Conflict; Unauthorized; Forbidden
        Timeout; Unavailable; RateLimited; Canceled; Internal )

func (k Kind) New(msg string) error                  // errors.NotFound.New("order not found")
func (k Kind) Errorf(format string, args ...any) error // %w supported
func (k Kind) Wrap(err error, msg string) error      // nil in → nil out; keeps err for Is/As
func (k Kind) String() string                        // "not_found": log field / metric label
func KindOf(err error) Kind     // outermost classification wins; ctx.Canceled → Canceled,
                                // DeadlineExceeded / Timeout() → Timeout; else Unknown
func IsRetryable(err error) bool                     // Timeout, Unavailable, RateLimited
func WithPublicMessage(err error, msg string) error  // text safe to return to callers
func PublicMessage(err error) string                 // explicit message or generic per Kind; never err.Error()
// Re-exports New, Is, As, AsType, Unwrap, Join, ErrUnsupported from stdlib.
```
Constructors hang off `Kind` so that `errors.New` keeps its standard-library meaning.
`Canceled` was added beyond the original list, so client cancellations are neither
retried nor reported as server errors.
Mapping lives in the transports (`transport/http.StatusFor(err)`, `transport/grpc.StatusFor(err)`),
not in `errors`, so `errors` stays dependency-free.

### config — ✅ implemented
```go
func Load[T any](opts ...Option) (T, error)    // env → defaults → validate; every problem in one error
func WithPrefix(p string) Option
func WithEnvironment(map[string]string) Option // tests: no os.Setenv
func Validate(cfg any) error                   // validate:"" tags, then Validate() methods, outermost first
type Secret string                             // every fmt verb, JSON, text, slog → "[REDACTED]"; Reveal()
func LogValue(cfg any) slog.Value              // startup log; structs → groups so key redaction applies
```
Uses caarlos0/env v11 for parsing (`required`, `notEmpty`, `file` for mounted secrets,
`envPrefix`) and go-playground/validator for rules. Errors never contain values: env parse
errors are rewritten to name only the field and type. Platform `Config` types implement
`Validate()`, so `Load` catches their mistakes at startup.

### observability
```go
// tracing
func Init(ctx, Config, ...Option) (shutdown func(context.Context) error, err error)
// metrics
func Init(ctx, Config, ...Option) (mp metric.MeterProvider, handler http.Handler, shutdown func(context.Context) error, err error)
// logging
func New(Config, ...Option) *slog.Logger         // JSON in prod, redaction, trace/span/request IDs from ctx
const ( KeyService="service"; KeyTraceID="trace_id"; KeyRequestID="request_id"; KeyDurationMS="duration_ms"; … )
```
Logging wraps the `slog.Handler` once to (a) add `trace_id`, `span_id` and
`request_id` from the context and (b) redact keys on a deny-list such as
`password`, `token`, `authorization` and `secret`. That costs one handler wrapper and nothing per call site.

### resilience — ✅ implemented
```go
retry.Do(ctx, op, opts...) error / retry.DoValue[T](ctx, op, opts...) (T, error)
p := retry.New(opts...); p.Do(ctx, op); retry.DoValueWith(ctx, p, op)   // reusable, ~50ns overhead
//   defaults: 3 attempts, 100ms→5s exponential, full jitter, retry only errors.IsRetryable
//   never sleeps past ctx deadline / WithMaxElapsed; honours RetryAfter(); retry.Permanent(err)

cb := circuitbreaker.New("payments-api", opts...)   // wraps sony/gobreaker/v2 two-step breaker
circuitbreaker.Execute[T](ctx, cb, op) / cb.Execute(ctx, op)  // ErrOpen: kind Unavailable
//   default: 5 consecutive failures, 10s cooldown, 1 half-open probe; WithFailureRatio + WithWindow
//   failures = timeout/unavailable/rate_limited/internal/unknown; client errors = success;
//   canceled = not counted; panics recorded as failure and re-raised (half-open never wedges)

lim := ratelimit.New("partner-api", 50, 10)        // wraps golang.org/x/time/rate
lim.Allow() bool; lim.Wait(ctx) error               // Wait bounded (1s default); rejects up front
//   *LimitedError: kind RateLimited, RetryAfter() → used by retry, usable for HTTP Retry-After
```
Composition is explicit: retry outside, breaker inside. Distributed rate limiting is
deferred to the redis module.

### concurrency — ✅ implemented
```go
pool := workerpool.New("thumbnails", workerpool.WithWorkers(20), workerpool.WithQueueSize(1000))
pool.Submit(ctx, task) error     // blocks while full (backpressure); ctx error or ErrClosed
pool.TrySubmit(ctx, task) error  // ErrQueueFull immediately (kind Unavailable)
pool.Shutdown(ctx) error         // graceful.Hook: stop intake, drain; at deadline cancel + drop queued
//   task ctx = submitter's values, pool's cancellation; panics recovered; errors → WithOnError

p := batch.New("audit", handler, batch.WithSize(500), batch.WithFlushInterval(time.Second))
p.Add(ctx, item) error           // blocks at WithMaxPending (default 2×size): bounded memory
p.Flush(ctx) error; p.Close(ctx) error   // Close is a graceful.Hook
//   flush on size / oldest-item age / Flush / Close; handler errors or *PartialError{Failed}
//   → WithOnFailure hook (no automatic requeue); order preserved at flush concurrency 1
```
The handler is a required argument of `batch.New`, not a `WithHandler` option as in the
original spec, so a processor without one cannot be constructed. Fixed workers, never a
goroutine per task (tested); a Submit racing Shutdown is either rejected or run, never lost
(tested, including a mutation check that the test catches the bug).

### lifecycle / health — ✅ implemented
```go
g := graceful.New(graceful.WithTimeout(25*time.Second), graceful.WithUnreadyDelay(5*time.Second))
g.Register(phase graceful.Phase, name string, hook func(context.Context) error) error
// Phases: Unready → (delay) → StopIntake → Drain → CloseDeps → Telemetry
g.Go(name, serve func() error) // early exit of a component triggers shutdown and becomes the cause
g.Wait(ctx) error              // signal / ctx / failure → run phases once → errors.Join of everything
g.Shutdown(ctx) error          // idempotent; all callers get the same result

h := health.New(health.WithMaxConcurrency(4), health.WithCacheTTL(time.Second))
h.AddReadiness("postgres", check, health.WithTimeout(time.Second), health.NonCritical())
h.AddLiveness(name, inProcessCheck)  // never external dependencies
h.MarkStarted(); h.Drain(ctx)         // Drain is a graceful.Hook for the Unready phase
h.LiveHandler(), h.ReadyHandler(), h.StartupHandler()
```
Telemetry is the last phase (the original spec flushed it before closing dependencies),
so logs and spans emitted while closing dependencies are exported. Hooks within a phase
run concurrently. After the deadline, unfinished hooks are named in a timeout error and
remaining phases are skipped (also named); a second signal has the same effect.
Health checks: probes share one run and cache it; at most one invocation per check in
flight, so a hung check cannot leak goroutines; responses expose only the error kind.

### secret / rotation / vault — ✅ implemented
```go
// secret: provider-neutral
type Secret struct{ /* fields map, unexported */ Version string; ExpiresAt time.Time; LeaseID string }
s.Field("password"); s.Require("username", "password"); s.TTL()  // every fmt verb/JSON/slog → redacted
type Provider interface { Get(ctx, key string) (Secret, error) }  // + ProviderFunc, Static (tests/dev)

// secret/rotation: knows nothing about Vault
r, err := rotation.New(ctx, "orders-db", rotation.Spec[R]{Fetch, Build, Validate, Close}, opts...)
//   first rotation synchronous (fail fast); then at 70%±5% of credential lifetime, or
//   WithRefreshInterval for non-expiring secrets (unchanged Version = no-op)
res, release, err := r.Acquire()  // refcounted; old resource closed after last release / drain timeout
r.Current()                       // no refcount, for self-draining resources (pgxpool)
r.Rotate(ctx)                     // forced; concurrent calls share one rotation
r.Close(ctx)                      // graceful.Hook (CloseDeps)
```
`secret/vault` (hashicorp/vault/api): `vault.New(ctx, cfg, vault.WithAuth(m))` logs in up front
(any `api.AuthMethod`, or a static token), renews with `LifetimeWatcher`, re-logs-in with
jittered backoff when renewal ends. `Get` (implements `secret.Provider`) reads any path and
unwraps KV v2 (fields, version; deleted → NotFound); dynamic secrets carry LeaseID/ExpiresAt.
`Fetcher`/`Revoke` plug into rotation; `RenewLease`; `API()` escape hatch. Errors classified by
status, never containing response bodies (raw proxy bodies are dropped). `Close` does not
revoke the token, because that would revoke leases still draining.

Swap is lock-free: `atomic.Pointer` plus a refcount with a "retired" bit, so `Acquire` can
never take a use on a resource that is being drained (deterministic white-box test; a
stress test alone could not catch the mutation). Failures keep the current resource and
retry with 1s→1m jittered backoff indefinitely; logs escalate to ERROR once the credential
in use has expired; `rotation.credential.ttl` gauge for alerting.

### transports
**HTTP — ✅ implemented** (`transport/http/httpserver`, `httpclient`, `echoadapter`)
```go
h := httpserver.Handler(mux, opts...)  // RequestID → Observe → Recover → MaxBytes(1MiB) → Timeout(30s)
srv := httpserver.NewServer(cfg, h, logger)      // ReadHeader 5s, Read 30s, Write 35s, Idle 120s
httpserver.Serve(shutdown, srv)                  // listen now (bind errors returned), serve under graceful
httpserver.WriteError(w, r, err)                 // kind → status, RFC 9457 problem+json, PublicMessage only
httpserver.StatusFor(err) int                    // 400/401/403/404/409/413/429/499/500/503/504
httpserver.Auth(authenticate)                    // per route; unclassified auth errors → 401, never 500

client := httpclient.New(cfg, httpclient.WithRetry(...), httpclient.WithCircuitBreaker(cb))
//   per attempt: otelhttp span + http.client.request.duration, X-Request-ID propagated
//   retry only idempotent methods or Idempotency-Key, rewindable bodies; 429/502/503/504 + network
//   errors; Retry-After honoured; exhausted → last real response returned (nil error)

e.Use(echoadapter.Middleware(opts...)); e.HTTPErrorHandler = echoadapter.ErrorHandler
```
Routes are templates: resolved from `*http.ServeMux` via `mux.Handler(r)` (the mux only sets
`r.Pattern` on its inner request), from `c.Path()` in Echo, or `WithRoute`. Never logged:
query strings, headers, bodies. Server metrics use semconv names; otelhttp's server metrics
are disabled because it cannot see the route.

**gRPC — ✅ implemented** (`transport/grpc/grpcserver`, `grpcclient`, `grpcstatus`)
```go
srv := grpcserver.New(grpcserver.WithLogger(l), grpcserver.WithHealth(checks), grpcserver.WithAuth(fn, public...))
grpcserver.Serve(shutdown, srv, ":9090")   // native *grpc.Server; GracefulStop, Stop at deadline
conn, _ := grpcclient.New(target, grpcclient.WithRetry(grpcclient.RetryPolicy{MaxAttempts: 3}))
grpcstatus.ToStatus(err) / FromStatus(err) / CodeFor(kind) / KindFor(code)
```
All interceptors on by default (the spec's opt-in `WithRecovery/WithTracing/...` would make
forgetting recovery possible): otelgrpc stats handler → observe (request ID, access log,
error mapping) → recover → auth. Errors cross service boundaries with their kind: the server
sends the kind's code and only the public message; the client classifies by code while
keeping the original status (own error type with `GRPCStatus()`, because `status.FromError`
otherwise rewrites the message). The request ID is echoed as a **trailer**: a header commits
the call and silently disables gRPC retries. Client: TLS by default (`WithInsecure` explicit),
10s default deadline, native service-config retries on UNAVAILABLE only. Own interceptors
instead of go-grpc-middleware: a few dozen lines, consistent with the HTTP side, one fewer
dependency. Health service (`grpc.health.v1`) answers from readiness.

### datastore — ✅ implemented
**postgres** (pgx/v5 pgxpool): `postgres.New(ctx, cfg, opts...)` pings before returning;
`Exec/Query/QueryRow/Begin/SendBatch` delegate to the current pool, `Pool()` per call.
`WithCredentials(fetch, revoke)` rotates pools via secret/rotation (`Current()`: pgxpool's
Close waits for acquired connections; bounded by ctx). `Classify(err)`: ErrNoRows→NotFound,
23505→Conflict, FK/check/not-null→InvalidArgument, 40001/40P01→Unavailable (retryable),
57014→Timeout, auth→Unauthorized. sslmode defaults to verify-full; password never in a DSN
string. Spans via otelpgx (children of the caller's span; SQL text, never parameters); pool
metrics `db.client.connection.*` observed from the current pool (otelpgx.RecordStats would
leak a registration per rotated pool). Tested against PostgreSQL 18: 10k+ queries across 3
rotations with zero failures, an open transaction surviving rotation, revocation after it ends.

**redis** (go-redis/v9): `redis.New` returns `*Client` embedding `UniversalClient`
(single/cluster/sentinel); lifecycle method is `Stop(ctx)` because go-redis's `Shutdown` sends
the server SHUTDOWN command. Retries off by default (go-redis retries non-idempotent
commands). `WithCredentials` feeds new connections through `CredentialsProviderContext`;
replaced credentials are revoked after `ConnMaxLifetime` (or at Stop). redisotel tracing with
`WithDBStatement(false)`: arguments can hold personal data. `Classify`: redis.Nil→NotFound,
WRONGPASS/NOAUTH→Unauthorized, NOPERM→Forbidden, LOADING/BUSY/TRYAGAIN/CLUSTERDOWN→Unavailable.

### messaging/kafka (franz-go) — ✅ implemented
```go
p, _ := kafka.NewProducer(ctx, cfg)                 // acks=all + idempotent, bounded buffer, snappy
p.Publish(ctx, recs...) / p.PublishAsync(ctx, r, done) / p.Close(ctx)   // Close: flush then close
c, _ := kafka.NewConsumer(ctx, cfg, group, topics, handler, opts...)    // or NewBatchConsumer
c.Run(ctx); c.Close(ctx)                             // Run under graceful.Go; Close in StopIntake
kafka.Typed(kafka.JSON[T]{}, h) / kafka.Encode(topic, key, v, kafka.JSON[T]{})
```
Records are `*kgo.Record` throughout (no wrapper type hiding Kafka concepts). One worker
goroutine per assigned partition (ordering per partition), a shared semaphore capping
handler calls (`WithConcurrency`, default 8), bounded per-partition buffers so polling waits
(backpressure). At-least-once: offsets are marked only after success or DLQ, committed every
5s, synchronously on revoke and Close. Exhausted/non-retryable records go to `WithDLQ`
(headers: original topic/partition/offset, error kind, error); without a DLQ the **partition is
paused**, never silently skipped (`WithSkipOnFailure` opts in). On revoke, in-flight handlers
finish (bounded), retry loops are abandoned, offsets committed, workers that overrun the
timeout can no longer mark offsets. Batch handlers report partial progress with
`*BatchError{Processed}`. kotel for spans (producer → consumer trace continues) and client
metrics; `kafka.consumer.records`, `.process.duration`, `.batch.size`, `.lag` (per partition),
`.partitions.stopped`. Tests run on franz-go's in-process `kfake` cluster, with mutation
checks for ordering, the concurrency cap, and not committing failed records.
Real brokers: with `KAFKA_TEST_BROKERS` set (`make test-kafka KAFKA_TEST_BROKERS=host:9092`) the
same tests run against real brokers. Each test creates and deletes its own `commontest-*`
topics and groups, so a shared broker is safe to use. CI runs them against a Redpanda
container (`kafka` job). They also pass against the team's Redpanda at 192.168.11.196:9092,
which GitHub-hosted runners cannot reach (it is a private LAN address; a self-hosted runner
would be needed to test it in CI).

### coordination — ✅ implemented
**idempotency**: `idempotency.Do[T](ctx, store, key, fn, opts...)` / `DoOutcome` (reports
`Duplicate`). Claim with lease (5m) → run → store result (24h) / release on failure; in-progress
elsewhere → `ErrInProgress` (Unavailable, retried by Kafka consumers). Ownership tokens make a
stale owner's completion fail (`ErrLeaseLost`). Stores: `NewMemoryStore`, `pgstore` (with
`DoTx`: claim, side effects and completion in one transaction: exactly once for writes in that
database, tested with 10 concurrent calls → one row), `redisstore` (Lua). Documented limits:
not exactly-once across a crash between side effect and completion, or past the lease or TTL.
Shared conformance suite `idempotencytest`.

**lock**: `lock.Acquire(ctx, locker, key, ttl)` (polls with jittered backoff), `Lease` with
`Release`, `Extend`, `Fence`. `pglock` (DB clock, global fence sequence) and `redislock`
(single node, per-key fence; explicitly not Redlock). The docs cover process pauses,
partitions, clocks and fencing. Shared conformance suite `locktest`, including a
20-goroutine mutual-exclusion check.

**featureflag**: `Evaluator` (`Bool`/`String`/`Int` returning the default plus a classified
error) backed by OpenFeature (`NewOpenFeature`), so Flipt and other providers plug in without
a dependency here; `Static` for tests. Fail-open versus fail-closed is the per-call default
and is documented. `featureflag.evaluations` metric and `feature_flag.evaluation` span events,
never attribute values.

---

## 5. Configuration strategy

- Each package exports a `Config` struct with `env` and `envDefault` tags and **no prefix**.
  The service composes them and chooses prefixes:
  ```go
  type Config struct {
      App      AppConfig         `envPrefix:"APP_"`
      Postgres postgres.Config   `envPrefix:"PG_"`
      Kafka    kafka.ConsumerConfig `envPrefix:"KAFKA_"`
  }
  cfg, err := config.Load[Config]()
  ```
- Packages also validate their own `Config` in `New`, so a hand-built config fails as fast as a loaded one.
- Secrets in config are typed `config.Secret` and redacted by construction. Prefer pulling
  real credentials from `secret.Provider` at runtime over passing them in environment variables.

## 6. Naming deviations from the target tree (approved 2026-10-08)

| Target | Proposed | Why |
|---|---|---|
| `testing/` | `testkit/` | `package testing` collides with stdlib in every `_test.go`. |
| `errors/` | keep, but re-export `Is/As/Unwrap/Join/New` | Avoids the import-alias dance at every call site. |
| `middleware/` | `requestid/` (+ transport-specific middleware in `transport/*`) | The prompt itself asks to keep middleware in the transports. A top-level grab-bag is the "giant middleware package" it warns against. The only transport-neutral piece is the request/correlation ID context. |

## 7. Observability strategy

- **Metric names**: OpenTelemetry instrument names (dot-separated, unit in the unit
  field), translated by the Prometheus exporter: `http.server.request.duration` + unit `s`
  → `http_server_request_duration_seconds`; counters gain `_total`. Durations are float
  seconds; histograms default to 5ms–10s buckets, and non-duration histograms must
  declare boundaries. HTTP, gRPC, DB and messaging metrics use OTel semantic conventions.
  Service identity is in `target_info`, not on every series.
- **Allowed label values** are bounded enums or config-time names: `service`, `method`,
  `route` (the template, never the raw path), `status_class`, `topic`, `partition`, `outcome`,
  `error_kind`, `pool`, `breaker`. **Forbidden**: user IDs, request IDs, raw URLs, error
  messages, keys and offsets. Enforced at runtime by the SDK cardinality limit
  (2000 series per instrument; excess is folded into `otel_metric_overflow="true"`).
- **Traces**: W3C `traceparent` and baggage. Propagation goes through HTTP headers, gRPC
  metadata and Kafka record headers (kotel), so HTTP → A → Kafka → B → Postgres is one trace.
  The span attribute allow-list mirrors the logging deny-list. Bodies and payloads are never recorded.
- **Logs**: one JSON line per request or record at the edge, plus warn/error inside packages.
  Hot paths never log per item at info level.

## 8. Quality gates (CI)

`go vet`, `staticcheck`/`golangci-lint`, `go test -race -shuffle=on ./...` in every module,
`govulncheck`, benchmarks for hot paths (`workerpool`, `batch`, logging handler, retry) with
`benchstat` comparison on PRs, and integration tests against real services: CI runs a PostgreSQL service container
(`POSTGRES_TEST_URL`); Vault uses `-tags integration` with a dev server. Import rules are enforced with `depguard`. Additive changes
are checked with `gorelease` / `apidiff`.

---

## 9. Implementation order

Each step is one PR, reviewable on its own.

| # | Scope | Depends on |
|---|---|---|
| 0 | ✅ `go 1.26`, dependency upgrade (clears 7 of 8 reachable vulns), CI (`.github/workflows/go.yml`), `go/Makefile`, `go/.golangci.yml` (legacy paths excluded, depguard rules) | — |
| 0b | ✅ Legacy JWT middleware: `jwt` v3 → v5 (GO-2025-3553, no v3 fix) and signing key via parameter instead of `"secret"`, `APP_ENV=test` auth bypass removed | 0 |
| 1 | ✅ `errors` | 0 |
| 2 | ✅ `observability/logging`, `requestid` | 1 |
| 3 | ✅ `observability/tracing`, `observability/metrics` (legacy `logger`, `observability` marked Deprecated) | 2 |
| 4 | ✅ `config` | 1 |
| 5 | ✅ `lifecycle/graceful`, `health` | 2 |
| 6 | ✅ `resilience/retry`, `circuitbreaker`, `ratelimit` | 1 |
| 7 | ✅ `concurrency/workerpool`, `batch` | 1 |
| 8 | ✅ `transport/http` (server + client), `echoadapter` | 2–7 |
| 9 | ✅ `secret`, `secret/rotation` | 6 |
| 10 | ✅ `secret/vault` | 9 |
| 11 | ✅ `datastore/postgres`, `datastore/redis` | 9 |
| 12 | ✅ `transport/grpc` | 2–6 |
| 13 | ✅ `messaging/kafka` | 6, 7 |
| 14 | ✅ `idempotency` (+ pg/redis stores), `lock`, `featureflag` | 11 |

Steps 1–7 alone are enough for a service to adopt logging, config, shutdown and
resilience before any infrastructure module exists.

## 10. Migration and compatibility

- **Versioning**: the repo is pre-1.0 and has no tags. Tag the current `main` as
  `go/v0.1.0` so existing consumers can pin it. The new packages ship as `v0.x`,
  minor bumps may break until v1.0 per package, and the API is frozen at v1.0 after two services have adopted it.
- **Legacy packages** (`logger`, `observability`, `http/*`, `constant`, `utils`): mark
  `// Deprecated:` with a pointer to the replacement in the first release. Fix only the
  security issue now (hard-coded JWT key → key supplied via option), and delete the packages at v1.0.
- **Per-service migration**: (1) swap logrus for slog through `observability/logging`,
  (2) replace `http.NewContext` with `graceful`, (3) mount `transport/http` middleware into
  Echo with `echo.WrapMiddleware`, (4) adopt datastore modules as services touch them.
- **After v1.0**: only additive changes. New behaviour goes behind options that default to
  current behaviour. Removals go through one minor release with `Deprecated:` and are
  checked with `apidiff` in CI.

## 11. Build vs. adopt

| Concern | Decision |
|---|---|
| Logging | **Adopt** `log/slog` (stdlib); we write only a handler wrapper |
| Tracing / metrics | **Adopt** OTel SDK, `otelhttp`, `otelgrpc`, `otelpgx`, `redisotel`, `kotel`, `kprom` |
| Config parsing | **Adopt** `caarlos0/env/v11` + `go-playground/validator/v10` |
| Retry | **Build** (~200 LOC; needs our error kinds, clock and hooks). `cenkalti/backoff/v5` is the fallback |
| Circuit breaker | **Wrap** `sony/gobreaker/v2` (add metrics, ctx, error-kind-aware failure counting) |
| Local rate limit | **Wrap** `golang.org/x/time/rate` (add bounded wait and metrics) |
| Distributed rate limit | **Adopt** `go-redis/redis_rate` in the redis module |
| Worker pool / batch | **Build** (small, and needs our metrics and shutdown semantics) |
| gRPC middleware | **Adopt** `grpc-ecosystem/go-grpc-middleware/v2` |
| Kafka | **Adopt** `twmb/franz-go`: pure Go, explicit rebalance hooks, per-partition pause, no cgo. segmentio/kafka-go lacks fine commit control, and confluent-kafka-go needs cgo |
| Postgres | **Adopt** `jackc/pgx/v5` (`pgxpool`); replaces `lib/pq` |
| Redis | **Adopt** `redis/go-redis/v9` |
| Vault | **Adopt** `hashicorp/vault/api` (+ `LifetimeWatcher`) |
| Feature flags | **Adopt** OpenFeature Go SDK + Flipt provider |
| UUID | **Adopt** `google/uuid`; drop `satori/go.uuid` |
| Health, graceful, idempotency, lock | **Build** (small, and their semantics are ours to define) |
| Test assertions | **Adopt** `stretchr/testify` or stdlib. `testkit` holds only the clock, fakes and log capture |

### fx integration — ✅ implemented (`go/fx`, separate module)
```go
fx.New(
    commonfx.Config[AppConfig](), commonfx.ConfigFields[AppConfig](), // fields as their own types
    commonfx.Observability(), commonfx.Lifecycle(),
    commonfx.AdminServer(), commonfx.HTTPServer(), commonfx.GRPCServer(),
    commonfx.Postgres(), commonfx.Redis(), commonfx.Vault(),
    commonfx.KafkaProducer(), commonfx.KafkaConsumer("billing", topics, NewBillingHandler),
    commonfx.GRPCClient("inventory"), commonfx.HTTPClient("payments", httpclient.WithRetry()),
    fx.Provide(...), commonfx.Ready(),                               // Ready always last
    fx.StopTimeout(45*time.Second),
).Run()
```
fx handles signals and construction; shutdown order stays with `graceful` phases, because fx's
reverse-dependency order cannot express "stop intake everywhere before draining" across
unrelated components. `Ready()` must be last: its OnStart marks the service ready after every
other start, and its OnStop runs the graceful phases before every other fx stop hook (fx's
start loop does not run hooks appended during start, so this cannot be automated). `Lifecycle`
fails app start if `Ready` is missing. Each module calls the core constructor, registers its
stop function in the right phase and adds readiness checks. Options that depend on the graph
(for example Vault-backed credential rotation) are contributed through value groups
(`commonfx.PostgresOptions`, ...). The admin server (probes, `/metrics`) stops in the last
phase so readiness keeps answering 503 during the drain.

Telemetry is wired automatically and explicitly: `Observability()` puts the logger, tracer
provider, meter provider and propagator in the graph, and every module passes them to the
package it builds. Nothing depends on OpenTelemetry globals or on option order. `HTTPServer`
applies the standard middleware itself (`HTTPHandlerAsIs()` for Echo apps that already use
echoadapter). Named clients (`GRPCClient`, `HTTPClient`, configured from `GRPCClientConfig` /
`httpclient.Config` with the same name tag) are closed in CloseDeps. `KafkaConsumer` builds the
consumer with telemetry from a handler constructor. An end-to-end test checks one trace across
HTTP client → HTTP server → gRPC client → gRPC server, and access logs for both servers, with no
manual wiring. A mutation check (telemetry not passed to the HTTP middleware) makes it fail.

## 12. Not yet done

- `testkit` (spec §25: test logger, metric helpers). The planned clock became `testing/synctest`,
  and the fakes landed with their packages (`secret.Static`, `featureflag.Static`,
  `idempotency.NewMemoryStore`, `idempotencytest`, `locktest`). Log-capture and metric-collection
  boilerplate is repeated across test files and would be worth extracting.
- Module split of the heavy integrations before v1.0 (§2); `go/fx` is already separate.

## 13. Open questions for the team

1. ~~Multi-module now, or single module until v1 (§2)?~~ Single module.
2. ~~Prometheus pull (`/metrics`) or OTLP push as the default metrics exporter?~~ Prometheus pull.
3. Is Echo the organizational standard? That decides whether `echoadapter` is permanent or migration-only.
4. Minimum Go version consumers must be on.
5. ~~Approve the renames in §6.~~ Approved.
