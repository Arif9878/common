# Changelog

All modules of this repository are released together with one version
(see [RELEASING.md](RELEASING.md)). The release tool writes each section
from the Conventional Commits merged since the previous release.

## [v0.7.0](https://github.com/Arif9878/common/releases/tag/go/v0.7.0) - 2026-10-09

### Breaking changes

- remove the legacy v0.1 packages ([#45](https://github.com/Arif9878/common/pull/45))

  Services importing these packages must migrate (see go/MIGRATION.md) or stay on go/v0.6.x.

### Features

- **observability:** ship a Grafana dashboard, alert rules and pprof ([#36](https://github.com/Arif9878/common/pull/36))
- **auth:** verify OAuth/OIDC JWTs with jwtauth ([#35](https://github.com/Arif9878/common/pull/35))
- cover auth, Redis and Kafka client in dashboards; Vault in fx ([#38](https://github.com/Arif9878/common/pull/38))
- **auth:** add oauth2client for service-to-service tokens ([#40](https://github.com/Arif9878/common/pull/40))
- **postgres:** add migrate, goose migrations one replica at a time ([#41](https://github.com/Arif9878/common/pull/41))
- **schedule:** run cron and interval jobs on one replica ([#42](https://github.com/Arif9878/common/pull/42))
- **redis:** add cache, a read-through cache with stampede protection ([#43](https://github.com/Arif9878/common/pull/43))
- **validation:** report every invalid field over HTTP and gRPC ([#44](https://github.com/Arif9878/common/pull/44))
- **release:** write CHANGELOG.md and release notes from conventional commits ([#47](https://github.com/Arif9878/common/pull/47))

### Fixes

- **release:** don't fail the API check on API this release adds ([#48](https://github.com/Arif9878/common/pull/48))

## [v0.6.1](https://github.com/Arif9878/common/releases/tag/go/v0.6.1) - 2026-10-09

### Features

- **logging:** take the service identity from OTEL_* variables ([#32](https://github.com/Arif9878/common/pull/32))

## [v0.6.0](https://github.com/Arif9878/common/releases/tag/go/v0.6.0) - 2026-10-09

### Features

- **fx:** add EchoServer ([#30](https://github.com/Arif9878/common/pull/30))
- **observability:** export traces, metrics and logs to any OTLP backend ([#31](https://github.com/Arif9878/common/pull/31))

## [v0.5.0](https://github.com/Arif9878/common/releases/tag/go/v0.5.0) - 2026-10-09

### Breaking changes

- split the heavy integrations into their own modules ([3614e8c](https://github.com/Arif9878/common/commit/3614e8cc5c4f44c9c49b2a9fc8b32cb9666ca51d))

  Services must require the integration modules they import, at the same version as the core module.

### Features

- **kafka:** add kafkaproto, Protobuf with a Schema Registry ([#24](https://github.com/Arif9878/common/pull/24))
- **outbox:** add the transactional outbox for Kafka ([#25](https://github.com/Arif9878/common/pull/25))
- **fx:** add batch consumers and Kafka idempotency ([#26](https://github.com/Arif9878/common/pull/26))
- **fx:** add the outbox relay and the Schema Registry client ([1703cb3](https://github.com/Arif9878/common/commit/1703cb3e88beb080897ab7f58ab4f706bca47a7b))

### Fixes

- **kafka:** fail only the record a BatchError names; count duplicates once ([#22](https://github.com/Arif9878/common/pull/22))

## [v0.4.0](https://github.com/Arif9878/common/releases/tag/go/v0.4.0) - 2026-10-08

### Features

- **kafka:** add WithIdempotency to skip records already processed ([#21](https://github.com/Arif9878/common/pull/21))

## [v0.3.0](https://github.com/Arif9878/common/releases/tag/go/v0.3.0) - 2026-10-08

### Features

- **testkit:** add test helpers and use them across the tests ([#20](https://github.com/Arif9878/common/pull/20))

## [v0.2.0](https://github.com/Arif9878/common/releases/tag/go/v0.2.0) - 2026-10-08

### Breaking changes

- **middleware:** require JWT signing key, move to jwt/v5 ([aa46a47](https://github.com/Arif9878/common/commit/aa46a4761a60c1a716fc5e22fb5814849fc71d1c))

  ValidateBearerToken(key []byte). The value stored under c.Get("token") is now *github.com/golang-jwt/jwt/v5.Token; type assertions against the v3 type will fail at runtime. Tests that relied on APP_ENV=test should not install the middleware instead.

### Features

- **errors:** add error classification package ([8769bb9](https://github.com/Arif9878/common/commit/8769bb9b0b26f235cc770cf211d8189095554174))
- **logging:** add slog-based logging and requestid packages ([3098ba3](https://github.com/Arif9878/common/commit/3098ba300b474261bb209516cd36c6c2b03b4bc3))
- **observability:** add tracing and metrics setup ([f670fad](https://github.com/Arif9878/common/commit/f670fadb203f7ad282d52326fe9b10394c8f57ee))
- **config:** add typed environment configuration loading ([4ccbe01](https://github.com/Arif9878/common/commit/4ccbe01e70fac408e52f1c006c75dbe0991b1281))
- **lifecycle:** add graceful shutdown and health probes ([7b3f15e](https://github.com/Arif9878/common/commit/7b3f15e8a0f686a599725ce4c63b6fe4761c7f25))
- **resilience:** add retry, circuit breaker and rate limiter ([6945294](https://github.com/Arif9878/common/commit/6945294b9db8851aaf27374821b87411f5c40bcb))
- **concurrency:** add bounded worker pool and batch processor ([32fa0fd](https://github.com/Arif9878/common/commit/32fa0fd070ff32710658145792a5d1b270e22f43))
- **transport/http:** add HTTP server middleware, client and Echo adapter ([01294b6](https://github.com/Arif9878/common/commit/01294b6af802b2480a425c034c7add43aa5c1889))
- **secret:** add secret abstraction and zero-downtime rotation ([17d553c](https://github.com/Arif9878/common/commit/17d553c6e16b6b6a59c7ada2da31d67b5d1c05d2))
- **secret/vault:** add Vault client implementing secret.Provider ([cfbbc9e](https://github.com/Arif9878/common/commit/cfbbc9e9a59017ae5fa047e7bc7dd29e96e7b15a))
- **datastore:** add PostgreSQL and Redis clients with credential rotation ([aae6ed8](https://github.com/Arif9878/common/commit/aae6ed8a3d7cad08469d5e3e69b635d242fbaeb6))
- **transport/grpc:** add gRPC server, client and status mapping ([50d83f0](https://github.com/Arif9878/common/commit/50d83f041fa22393f87916ad6583999aca6f04c0))
- **messaging/kafka:** add franz-go producer and consumer ([6cb267d](https://github.com/Arif9878/common/commit/6cb267dc8ff5d831a1f08bf62eb4a7ad9654d7ae))
- add idempotency, distributed locks and feature flags ([1cefbe7](https://github.com/Arif9878/common/commit/1cefbe7a709a122a1cbfef549a1743a965352d0e))
- **fx:** add Uber fx integration as a separate module ([16ab552](https://github.com/Arif9878/common/commit/16ab5521328888d25d96c5e6056b9d8acb33ac10))
- **fx:** wire telemetry explicitly, apply HTTP middleware, add clients ([293f7cf](https://github.com/Arif9878/common/commit/293f7cf2072ba7fe5cb6f383df072008589d7b56))

### Fixes

- **postgres:** retry operations racing a credential rotation ([12ad24c](https://github.com/Arif9878/common/commit/12ad24c82412cfe239028fec07ff8696101effac))
- **postgres:** also retry connections cancelled by a rotation ([947dd83](https://github.com/Arif9878/common/commit/947dd830e1e1d607d70311f31744dfd8b3ba6023))

## [v0.1.0](https://github.com/Arif9878/common/releases/tag/go/v0.1.0) - 2024-01-24

### Features

- added logger and http echo ([7e0d948](https://github.com/Arif9878/common/commit/7e0d9480345cd7f398be0cc6235842826e91c08b))
- update package http context ([6591988](https://github.com/Arif9878/common/commit/6591988b79976df93dd81c849302f95f746ae4f2))
- update logger ([b2f06d5](https://github.com/Arif9878/common/commit/b2f06d5cd00e2761ce466e9483f8e74e5ecff200))
- update logger ([8b69902](https://github.com/Arif9878/common/commit/8b699027949c141ff5882faa488dcb4897d95855))
- added utils sql ([#1](https://github.com/Arif9878/common/pull/1))
- update go.mod ([64bb32a](https://github.com/Arif9878/common/commit/64bb32a276d52caaf170a7df17f12f77bd654b08))
- update common middleware ([505b2ab](https://github.com/Arif9878/common/commit/505b2ab730fbe33e8b860acc6adb21a0989771ab))
- update go mod ([9cf6d80](https://github.com/Arif9878/common/commit/9cf6d80315f316543e5c56c41b4848df36260a3e))
