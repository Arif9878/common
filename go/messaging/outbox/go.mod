module github.com/Arif9878/common/go/messaging/outbox

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
	github.com/Arif9878/common/go/messaging/kafka => ../kafka
	github.com/Arif9878/common/go/secret/vault => ../../secret/vault
	github.com/Arif9878/common/go/testkit/pgtest => ../../testkit/pgtest
	github.com/Arif9878/common/go/transport/grpc => ../../transport/grpc
)

require (
	github.com/Arif9878/common/go v0.6.0
	github.com/Arif9878/common/go/datastore/postgres v0.6.0
	github.com/Arif9878/common/go/messaging/kafka v0.6.0
	github.com/Arif9878/common/go/testkit/pgtest v0.6.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/twmb/franz-go v1.22.1
	github.com/twmb/franz-go/pkg/kadm v1.19.0
	github.com/twmb/franz-go/pkg/kfake v0.0.0-20261007040850-d3792b34935a
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
)

require (
	github.com/caarlos0/env/v11 v11.4.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/exaring/otelpgx v0.13.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.15 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.30.5 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.31.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/leodido/go-urn v1.5.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.30 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.14.0 // indirect
	github.com/twmb/franz-go/plugin/kotel v1.7.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/bridges/otelslog v0.21.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.1 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261005182115-fad411399dd8 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261005182115-fad411399dd8 // indirect
	google.golang.org/grpc v1.84.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
