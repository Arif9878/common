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

### secret / rotation / vault
```go
// secret
type Secret struct{ value []byte; Version string; ExpiresAt time.Time; Renewable bool } // value unexported; String() redacts
func (s Secret) Reveal() []byte
type Provider interface { Get(ctx context.Context, key string) (Secret, error) }

// secret/rotation: knows nothing about Vault
type Builder[R any] interface {
    Fetch(ctx) (secret.Secret, error)
    Build(ctx, secret.Secret) (R, error)
    Validate(ctx, R) error
    Drain(ctx, R) error     // wait for in-flight users
    Close(ctx, R) error     // close / revoke
}
r, err := rotation.New[R](b, rotation.WithRefreshBefore(0.2) /* of TTL */, ...)
res, release := r.Acquire()   // refcounted; release() lets old resources drain
r.Current() R                 // for resources that self-drain (pgxpool)
r.Rotate(ctx) error           // forced; serialized by a per-rotator mutex/singleflight
r.Run(ctx) error              // background loop; register with graceful
```
The swap uses `atomic.Pointer`. The old resource is drained in the background,
bounded by `WithDrainTimeout`. If validation fails, the old resource is kept and
the next attempt is scheduled with backoff from `retry`.

`secret/vault`: `vault.New(cfg, auth, ...)` returns a `secret.Provider` and exposes
`Client() *api.Client`. It uses Vault's `LifetimeWatcher` for token and lease renewal,
and supports KV v2 plus dynamic `database/creds/*`. Errors are classified with
`errors.Kind`. Error messages never include response bodies, because those can contain secret data.

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

**gRPC** (step 12):
- **grpc**: `grpcserver.New(opts...) *grpc.Server` returns the native server; nothing is hidden.
  Interceptors come from go-grpc-middleware v2 (recovery, logging, auth, selector) plus
  otelgrpc stats handlers. `grpcclient.Dial(target, opts...) (*grpc.ClientConn, error)`
  uses gRPC's native service-config retry policy rather than a custom loop.

### datastore
- **postgres**: `postgres.New(ctx, cfg, opts...) (*DB, error)`, built on pgxpool.
  `db.Pool() *pgxpool.Pool` is fetched per call, so do not cache it. Rotation is integrated
  through `rotation.Rotator[*pgxpool.Pool]`. `pgxpool.Close` already waits for acquired
  connections, which gives draining for free. Tracing uses `otelpgx` and metrics come from `pool.Stat()`.
  Static (non-lease) passwords can use the lighter `BeforeConnect` hook instead of a pool swap.
- **redis**: `redis.New(ctx, cfg, opts...) (redis.UniversalClient, error)`, which covers single,
  cluster and sentinel. Credential rotation uses go-redis's `CredentialsProviderContext`, so
  new connections pick up new credentials without swapping the client. Tracing and metrics use `redisotel`.

### messaging/kafka (franz-go)
```go
c, _ := kafka.NewConsumer(cfg, kafka.HandleBatch(h), kafka.WithConcurrency(8), kafka.WithDLQ(producer, "x.dlq"))
type Handler      func(ctx, *kgo.Record) error
type BatchHandler func(ctx, []*kgo.Record) error   // per-partition batch
```
- **Delivery**: at-least-once. Offsets are committed only after the handler succeeds, or
  after a record is sent to the DLQ, using `kgo.DisableAutoCommit` and marked-offset commits.
- **Ordering**: preserved per partition. Each partition is handled by at most one worker at a
  time, and concurrency is across partitions. Optional key-hash sub-partitioning gives
  per-key ordering with more parallelism.
- **Backpressure**: polling pauses (`PauseFetchPartitions`) when a partition's in-flight buffer is full.
- **Rebalance**: on revoke, stop dispatching, wait up to `RevokeTimeout` for in-flight work,
  commit what is done, then release. A record whose work was cut off is redelivered.
- **Partial batch failure**: the commit advances only to the last contiguous success. Later
  records in that batch are retried (and may be redelivered); this is documented.
- **Retry**: bounded in-process retry for retryable kinds, then the DLQ (if configured) or
  stopping that partition. Partitions are never skipped silently.
- Producer: `Produce(ctx, topic, key, value)` (sync) and `ProduceAsync(..., cb)`, with a
  `Serializer[T]` abstraction, `acks` from config, and trace context injected into headers through `kotel`.
- `Client() *kgo.Client` is exposed for advanced use.

### coordination
- **idempotency**: `Do[T](ctx, store, key, fn, opts...)`.
  `Store` interface: `Begin(key, ttl) (State, error)`, `Complete(key, result)`, `Fail(key)`.
  States are `InProgress`, `Completed` and `Failed`. **Documented guarantee: at-most-once start
  per key within the TTL, not exactly-once.** A crash between the side effect and `Complete`
  leaves an expired in-progress record, so the operation must be safe to re-run or must commit
  in the same transaction as the store. The Postgres store supports that same-transaction case.
- **lock**: `Locker.Acquire(ctx, key, ttl) (Lease, error)`. A `Lease` carries `Token` (owner)
  and `Fence uint64`. The Postgres implementation (advisory lock plus a sequence for fencing)
  is recommended for correctness. The Redis implementation is documented as best-effort
  (efficiency only, not safety), following Kleppmann's critique of Redlock.
- **featureflag**: adopt **OpenFeature** (`github.com/open-feature/go-sdk`). It already is the
  vendor-neutral evaluator abstraction, and Flipt ships a provider. We add a small
  `Evaluator` interface for consumers, plus metrics and tracing hooks and a documented fail-mode per call (`featureflag.FailOpen` / `FailClosed`).

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
`benchstat` comparison on PRs, and integration tests with testcontainers-go behind a
`//go:build integration` tag. Import rules are enforced with `depguard`. Additive changes
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
| 9 | `secret`, `secret/rotation` | 6 |
| 10 | `secret/vault` | 9 |
| 11 | `datastore/postgres`, `datastore/redis` | 9 |
| 12 | `transport/grpc` | 2–6 |
| 13 | `messaging/kafka` | 6, 7 |
| 14 | `idempotency` (+ pg/redis stores), `lock`, `featureflag` | 11 |

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

## 12. Open questions for the team

1. ~~Multi-module now, or single module until v1 (§2)?~~ Single module.
2. ~~Prometheus pull (`/metrics`) or OTLP push as the default metrics exporter?~~ Prometheus pull.
3. Is Echo the organizational standard? That decides whether `echoadapter` is permanent or migration-only.
4. Minimum Go version consumers must be on.
5. ~~Approve the renames in §6.~~ Approved.
