# Compatibility

This document says what the modules of this repository promise to keep stable, and how changes reach services. It applies to every module under `go/`: the core module `go`, the integration modules (`go/datastore/postgres`, `go/messaging/kafka`, …) and `go/fx`.

## Versions

All modules are released together with one version ([RELEASING.md](RELEASING.md)). A service uses the same version for every module of this repository that it requires; mixing versions is not tested.

The modules follow [semantic versioning](https://semver.org):

| Release | May contain |
|---|---|
| Patch (`v1.2.3` → `v1.2.4`) | Bug fixes and dependency updates. No new API. |
| Minor (`v1.2.x` → `v1.3.0`) | New API and new behavior that is off by default or doesn't change what existing code observes. |
| Major (`v1.x` → `v2.0.0`) | Incompatible changes. The module paths get the `/v2` suffix, so both versions can be used side by side while migrating. |

**Before v1.0** a minor version may also make incompatible changes. The release notes say what changed and how to upgrade.

The release tool runs `gorelease` and refuses a release whose version is too small for its API changes.

## What counts as the API

These are covered by the promise. Changing them incompatibly needs a major version:

- **Exported Go identifiers** of non-`internal` packages: names, signatures, struct fields, interface method sets, and the meaning of the error kinds (`errors.Kind`) returned.
- **Documented behavior.** If the Go documentation says it, it holds: defaults, retry and timeout behavior, ordering, which errors are returned when. Every exported identifier is documented (the `revive` `exported` rule enforces it).
- **Configuration.** Environment variable names, their defaults and accepted values ([ENVIRONMENT.md](ENVIRONMENT.md)), and the `config` struct tags behind them.
- **Telemetry.** Metric names, units and label names; span names and attributes the packages set; log field names (the `logging.Key…` constants: `trace_id`, `request_id`, `error`, …). Dashboards and alerts in [`observability/dashboards`](observability/dashboards) depend on them.
- **Wire formats.** The `application/problem+json` body (`type`, `title`, `status`, `detail`, `request_id`, `errors[]`), the gRPC status details, outbox and Kafka headers, and the idempotency, lock and cache keys stored in Redis and PostgreSQL. A replica on the old version and one on the new run side by side during a rollout, so both must understand the data the other writes.
- **Admin endpoints.** Paths and response codes of `/live`, `/ready`, `/startup` and `/metrics` served by `commonfx.AdminServer`.

These are **not** covered and may change in any release:

- Packages under `internal/`, and anything under `testkit` that is documented as test-only output (log text, golden formats).
- Log messages (the text, not the field names), error message strings, and the order of fields.
- Label *values* that come from third-party instrumentation (otelhttp, otelgrpc, redisotel, kotel). We keep their semantic convention version pinned and call out upgrades in the release notes.
- Performance characteristics, unless documented (for example, "no allocation per call").
- Behavior that the documentation calls experimental.

Adding a method to an exported interface is incompatible; we avoid interfaces that services implement, or keep them small and add new behavior through optional interfaces or options.

## Deprecation

1. An identifier is marked `// Deprecated: use X instead.` in a minor release, with the replacement already available. `staticcheck` reports every use (SA1019).
2. The release notes and [MIGRATION.md](MIGRATION.md) show the replacement.
3. It is removed no earlier than the next major version. Before v1.0, no earlier than the next minor version.

Environment variables, metrics and labels follow the same steps: the new name is added first, both work during the deprecation period, and the old one is removed in the next major version.

## Go versions

The modules support the two most recent Go releases, as the Go project does. The minimum is the `go` line in each `go.mod` (currently 1.26). Raising the minimum when Go drops support for a version is not a breaking change and happens in a minor release.

Dependencies are updated in patch or minor releases. When a dependency's own major version leaks into our API (for example, a pgx or go-redis type in a signature), upgrading it is a major change for us.

## API conventions

New packages follow these conventions, so the modules read alike. The review for v1.0 checked every exported package against them.

**Construction**

- `New(ctx, cfg, opts...)` when construction does I/O (connects, fetches keys), `New(cfg, opts...)` otherwise. `cfg` is a `Config` struct loaded with `config.Load`; options are `func` options named `WithX`.
- Constructors return a usable value or an error; there is no separate `Start` unless the value runs a loop (see below).
- Exceptions kept for v1.0: the OpenTelemetry providers use `tracing.Init`, `metrics.Init` and `logs.Init` because they also set the global provider; `httpserver.NewServer`; Kafka has `NewProducer`, `NewConsumer` and `NewBatchConsumer` in one package; `kafkaproto.NewRegistry`.

**Lifecycle**

All stop methods take a `context.Context` and return `error`, so they match `graceful.Hook`, and calling them again returns the first result.

| Method | Meaning | Used by |
|---|---|---|
| `Run(ctx)` | Blocks running a loop until `ctx` ends or `Stop` is called. | `kafka.Consumer`, `outbox.Relay`, `schedule.Scheduler` |
| `Stop(ctx)` | Stops taking new work and waits for work in flight. | `outbox.Relay`, `schedule.Scheduler`, `redis.Client` |
| `Close(ctx)` | Releases connections and background goroutines. | `postgres.DB`, `vault.Client`, `kafka.Producer`, `jwtauth.Verifier`, `rotation.Rotator`, `batch.Processor` |
| `Shutdown(ctx)` | Flushes buffered data, then releases. | telemetry providers, `workerpool.Pool`, `graceful.Manager` |

`redis.Client` uses `Stop` because it embeds `goredis.UniversalClient`, whose `Close() error` would otherwise collide. Use `Stop`; the embedded `Close` skips credential revocation.

**Errors**

- Functions return errors from the `errors` package with a `Kind`, so HTTP and gRPC handlers map them without inspecting messages. Errors from dependencies are classified at the boundary (for example, `postgres.Classify`).
- Validation errors carry field details (`errors.WithFields`), which become `errors[]` in problem+json and `BadRequest` in gRPC status.

**Telemetry**

- Every component takes `WithMeterProvider`, `WithTracerProvider` and `WithLogger` options and falls back to the global providers and `slog.Default()`.
- Metric names are dotted OpenTelemetry names (`cache.requests`); the Prometheus exporter turns them into `cache_requests_total`. Low-cardinality labels only: names given in code, outcomes, error kinds.

## Reporting a break

If an upgrade breaks a service in a way this document says it shouldn't, that's a bug: open an issue with the versions and what changed. The fix is a patch release that restores the old behavior.
