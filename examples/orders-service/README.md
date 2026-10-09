# orders-service

This is the golden-path example: a service built the way the common library intends, small enough to read in one sitting. Copy it to start a new service.

```text
POST /orders ──► PostgreSQL: orders + kafka_outbox, one transaction
                       │
                outbox relay (one replica at a time)
                       ▼
                 Kafka: orders.created ──► consumer "orders-notifier"
                                            (idempotent: Redis + ON CONFLICT)
                                                  │
GET /orders/:id ◄── PostgreSQL: notifications ◄───┘
```

| File | What it shows |
|---|---|
| `main.go` | The fx application: configuration from the environment, observability, lifecycle, admin and Echo servers, PostgreSQL, Redis, Kafka, the outbox relay and Kafka idempotency. |
| `internal/orders/orders.go` | Writing the order and its event in one transaction with `outbox.Write`; errors classified with `errors` and `postgres.Classify`. |
| `internal/orders/http.go` | Echo handlers that return errors; `EchoServer` turns them into problem+json responses. |
| `internal/orders/notifier.go` | A Kafka handler with `kafka.Typed` that is safe to run twice. |
| `main_test.go` | The whole flow end to end, with in-process Kafka and Redis and a real PostgreSQL. |
| `Dockerfile`, `docker-compose.yml`, `k8s/orders.yaml` | Deployment: identity from `OTEL_*`, shared settings in a ConfigMap, probes on the admin port, a grace period longer than the shutdown timeout. |

## Run it

```sh
docker compose -f examples/orders-service/docker-compose.yml up --build   # from the repository root
curl -s -XPOST localhost:8080/orders -H 'Content-Type: application/json' -d '{"customer_id":"c-1","amount":1500}'
curl -s localhost:8080/orders/1    # "notified": true once the consumer ran
open http://localhost:3000         # Grafana: logs in Loki, traces in Tempo, metrics in Prometheus
```

## Test it

```sh
cd examples/orders-service
POSTGRES_TEST_URL=postgres://user:pass@localhost:5432/db go test -race ./...
```

CI builds and tests it with every change to the library.

## Using it as a template

1. Replace the module path, and drop the `replace` block in `go.mod`.
2. Require the released versions: `go get github.com/Arif9878/common/go@latest`, then the same version of `go/fx`, `go/datastore/postgres`, `go/datastore/redis`, `go/messaging/kafka` and `go/messaging/outbox`.
3. Run migrations with your migration tool instead of `orders.Migrate`.

Every environment variable is listed in [ENVIRONMENT.md](../../go/ENVIRONMENT.md).
