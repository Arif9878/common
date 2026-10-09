module github.com/Arif9878/common/go/transport/grpc

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
	github.com/Arif9878/common/go/lock/pglock => ../../lock/pglock
	github.com/Arif9878/common/go/lock/redislock => ../../lock/redislock
	github.com/Arif9878/common/go/messaging/kafka => ../../messaging/kafka
	github.com/Arif9878/common/go/messaging/outbox => ../../messaging/outbox
	github.com/Arif9878/common/go/secret/vault => ../../secret/vault
	github.com/Arif9878/common/go/testkit/pgtest => ../../testkit/pgtest
)

require (
	github.com/Arif9878/common/go v0.6.0
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.72.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/bridges/otelslog v0.21.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.47.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261005182115-fad411399dd8 // indirect
)
