# Migrating from the legacy packages

The packages below came from `go/v0.1.0`. They were deprecated in v0.2.0 and are **removed** in the release that follows `go/v0.6.x`. To keep using them, stay on `go/v0.6.x` while you migrate. Their replacements are in this module, so migrating needs no new dependency.

| Legacy package | Replacement |
|---|---|
| `logger` (logrus, `ILogger`) | `log/slog` built by `observability/logging` |
| `observability` (never exported spans) | `observability/tracing` (OTLP) and `observability/metrics` (Prometheus) |
| `http` (`NewContext`) | `lifecycle/graceful` |
| `http/echo/server` | `transport/http/httpserver` with `transport/http/echoadapter` |
| `http/echo/middleware` | `echoadapter.Middleware` and `httpserver.Auth` |
| `constant` | OpenTelemetry semantic conventions (`db.system.name`), set by `datastore/postgres` |
| `utils` (`IntOrNull`, …) | pgx types, or `database/sql` `Null*` types directly |

Echo is the organization's standard web framework, so `echoadapter` is permanent; only the old Echo setup code goes away.

Removing them also removed their dependencies from the core module: `logrus`, `satori/go.uuid`, `lib/pq` and `go-oauth2`.

## `logger` → `observability/logging`

```go
// Before
logger.Logger.Infof("order %s created", id)
logger.Logger.Errorf("charge failed: %v", err)

// After
log, err := logging.New(cfg.Log) // cfg.Log is logging.Config, loaded with config.Load
if err != nil {
	return err
}
log.InfoContext(ctx, "order created", "order_id", id)
log.ErrorContext(ctx, "charge failed", logging.Err(err))
```

- **Pass the logger.** There is no package-level `Logger`, so pass `*slog.Logger` to components instead. Use `slog.SetDefault(log)` only for code you cannot change.
- **No format strings.** Write a fixed message and put the values in attributes. Logs can then be searched by field.
- **Context matters.** The `...Context` methods add the trace ID and request ID from `ctx`.
- **No `Panic` or `Fatal`.** Return the error up to `main`, and exit there.
- **Redaction.** Keys such as `password`, `token` and `authorization` are redacted. `config.Secret` values never print.
- **Tests.** `testkit.NewLogger(t)` captures records instead of mocking `ILogger`.

## `observability` → `observability/tracing` and `observability/metrics`

The legacy package never exported spans: `Tracer.Start` did not create one. Replace it with:

```go
tp, err := tracing.Init(ctx, cfg.Tracing) // Exporter: "otlp-grpc" | "otlp-http" | "none"
if err != nil {
	return err
}
mp, err := metrics.Init(ctx, cfg.Metrics, metrics.WithGlobal())
if err != nil {
	return err
}
mux.Handle("/metrics", mp.Handler())
shutdown.Register(graceful.Telemetry, "tracing", tp.Shutdown)
shutdown.Register(graceful.Telemetry, "metrics", mp.Shutdown)
```

Every platform package (HTTP, gRPC, Kafka, PostgreSQL, Redis, …) creates its own spans and metrics. Pass them `tp.TracerProvider()` and `mp.MeterProvider()`, or rely on the global providers. You no longer need `DatastoreOperation` spans around queries, because `datastore/postgres` traces every query.

## `http.NewContext` → `lifecycle/graceful`

```go
// Before
ctx := http.NewContext() // cancelled on SIGINT/SIGTERM

// After
shutdown := graceful.New(graceful.WithLogger(log))
if err := httpserver.Serve(shutdown, srv); err != nil { // listens now, stops in graceful.StopIntake
	return err
}
shutdown.Go("kafka consumer", func() error { return consumer.Run(ctx) })
shutdown.Register(graceful.StopIntake, "kafka consumer", consumer.Close)
shutdown.Register(graceful.CloseDeps, "postgres", db.Close)
return shutdown.Wait(ctx) // handles SIGINT/SIGTERM, runs the phases in order, bounded by a timeout
```

## `http/echo/server` → `httpserver` + `echoadapter`

```go
// Before
e := server.NewEchoServer()
server.RunHttpServer(ctx, e, logger.Logger, &server.EchoConfig{Port: ":8080", Timeout: 30})

// After
e := echo.New()
e.HTTPErrorHandler = echoadapter.ErrorHandler          // problem+json errors from error kinds
e.Use(echoadapter.Middleware(httpserver.WithLogger(log))) // request ID, tracing, metrics, access log, recovery
// ... routes ...
srv := httpserver.NewServer(cfg.HTTP, e, log) // cfg.HTTP is httpserver.Config: ADDR, timeouts, header limit
if err := httpserver.Serve(shutdown, srv); err != nil {
	return err
}
```

- **Config.** `EchoConfig` (mapstructure tags) becomes `httpserver.Config`, which is loaded from the environment with the rest of the configuration (`config.Load`).
- **Shutdown.** `httpserver.Serve` listens immediately, serves in the background, and stops accepting requests and drains in-flight ones in the `graceful.StopIntake` phase.
- **Middleware order.** Register `echoadapter.Middleware` with `e.Use`, not `e.Pre`, so the route template (`/orders/:id`) labels spans and metrics.
- **API versioning.** `ApplyVersioningFromHeader` and `RegisterGroupFunc` are thin wrappers. Use `e.Group` directly, and keep any header-based versioning in the service.

## `http/echo/middleware` → `echoadapter` and `httpserver.Auth`

`CorrelationIdMiddleware` is replaced by `echoadapter.Middleware`, which reads or creates `X-Request-ID`. The ID is available with `requestid.FromContext(c.Request().Context())`.

`ValidateBearerToken(key)` becomes an `httpserver.Auth` authenticator. The authenticator owns the token check, so it can use the signing method, issuer and audience your service needs:

```go
auth := httpserver.Auth(func(r *http.Request) (context.Context, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, errors.Unauthorized.New("missing bearer token")
	}
	tok, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, errors.Unauthorized.Wrap(err, "invalid token")
	}
	return context.WithValue(r.Context(), tokenKey{}, tok), nil
})
e.Use(echo.WrapMiddleware(auth))
```

An `Unauthorized` error becomes a 401 problem+json response. The token is in the request context, not in `c.Get("token")`.

## `constant` → semantic conventions

`DatastoreProduct` labeled the legacy spans. `datastore/postgres` sets the standard OpenTelemetry attributes itself, so delete these references.

## `utils` → pgx or `database/sql`

- **pgx.** Use `*T` pointers (`nil` is NULL) or `pgtype` values. `PositiveIntOrNull(n)` becomes a pointer that is `nil` when `n <= 0`.
- **`database/sql`.** Build `sql.NullInt64{Int64: n, Valid: true}` directly. Use `sql.Null[time.Time]` instead of `pq.NullTime`, which drops the `lib/pq` dependency.
