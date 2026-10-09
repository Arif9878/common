module github.com/Arif9878/common/go/lock/redislock

go 1.26.0

// Development: build against the modules in this repository. Consumers
// ignore replace directives; the release tool (internal/release) sets the
// requirements above to the released version.
replace (
	github.com/Arif9878/common/go => ../..
	github.com/Arif9878/common/go/datastore/postgres => ../../datastore/postgres
	github.com/Arif9878/common/go/datastore/redis => ../../datastore/redis
	github.com/Arif9878/common/go/fx => ../../fx
	github.com/Arif9878/common/go/idempotency/pgstore => ../../idempotency/pgstore
	github.com/Arif9878/common/go/idempotency/redisstore => ../../idempotency/redisstore
	github.com/Arif9878/common/go/lock/pglock => ../pglock
	github.com/Arif9878/common/go/messaging/kafka => ../../messaging/kafka
	github.com/Arif9878/common/go/messaging/outbox => ../../messaging/outbox
	github.com/Arif9878/common/go/secret/vault => ../../secret/vault
	github.com/Arif9878/common/go/testkit/pgtest => ../../testkit/pgtest
	github.com/Arif9878/common/go/transport/grpc => ../../transport/grpc
)

require (
	github.com/Arif9878/common/go v0.6.1
	github.com/Arif9878/common/go/datastore/redis v0.6.1
	github.com/alicebob/miniredis/v2 v2.39.0
	github.com/redis/go-redis/v9 v9.23.0
)

require (
	github.com/caarlos0/env/v11 v11.4.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.15 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.5 // indirect
	github.com/leodido/go-urn v1.5.0 // indirect
	github.com/redis/go-redis/extra/rediscmd/v9 v9.23.0 // indirect
	github.com/redis/go-redis/extra/redisotel/v9 v9.23.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/bridges/otelslog v0.21.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
