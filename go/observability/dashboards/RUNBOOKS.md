# Alert runbooks

One section per rule in [`prometheus/alerts.yaml`](prometheus/alerts.yaml); each rule's `runbook_url` points here. Every section says what fired, what to look at first, and how to stop the bleeding. The service's own runbook covers its business logic; these cover what the common library does.

Start every investigation the same way:

- **Dashboard.** Open *Service overview* for the `job` in the alert; its rows match the groups below.
- **Logs.** Filter by `service` and the time of the alert. Errors carry `error` and `error_type` (the error kind); requests carry `request_id` and `trace_id`.
- **Recent changes.** A deploy, a config change, or a dependency's deploy explains most alerts.

Configuration variables are written as `…NAME`: the service chooses the prefix (such as `POSTGRES_`), and [ENVIRONMENT.md](../../ENVIRONMENT.md) lists them all.

## HTTP and gRPC

### HTTPServerErrorRateHigh

More than 5% of HTTP requests answered 5xx for 10 minutes.

- **Check:** which routes fail (`http_route` on the dashboard), and the error logs for those requests. 503 with `error_type=unavailable` points at a dependency; 500 with `internal` at a bug.
- **Fix:** roll back a bad deploy. If a dependency is down, its own alert (PostgreSQL, Redis, circuit breaker) usually fires too; fix that first.

### HTTPServerLatencyHigh

p99 HTTP latency above 1 s for 15 minutes.

- **Check:** the slow routes, then their traces: the span that takes the time is usually a query, a call to another service, or waiting for a pool connection (see PostgresPoolExhausted, RedisPoolTimeouts).
- **Fix:** scale out if CPU is saturated; otherwise fix the slow dependency or query. Request deadlines (the `httpserver.Timeout` middleware) cap the damage.

### GRPCServerErrorRateHigh

More than 5% of gRPC calls ended Internal, Unknown, Unavailable, DeadlineExceeded or DataLoss for 10 minutes.

- **Check:** `rpc_method` on the dashboard and the access log (`grpc call`) for the failing method. DeadlineExceeded means the callers' deadlines are shorter than the work.
- **Fix:** as for HTTPServerErrorRateHigh.

## Kafka consumers and producers

### KafkaPartitionStopped

A record failed for good (retries exhausted, no dead-letter topic) and its partition is paused. Nothing after it is processed until the service restarts.

- **Check:** the error log for the record: topic, partition and offset are in the log fields.
- **Fix:** fix the handler or the data, then restart the service: the record is retried from the last committed offset. To move past a record that can never succeed, configure a dead-letter topic (`WithDLQ`) or `WithSkipOnFailure` and restart.

### KafkaConsumerLagHigh

The consumer group is more than 10,000 records behind on a topic.

- **Check:** is the lag growing or shrinking? Are records failing and retrying (`kafka_consumer_records_total` by outcome)? Is one partition stuck (KafkaPartitionStopped)?
- **Fix:** if the consumer is healthy but too slow, raise `WithConcurrency` or add replicas (up to the partition count). If a dependency is slow, fix it first.

### KafkaRecordsDeadLettered

Records failed for good and went to the dead-letter topic.

- **Check:** the error logs for those records, and the records themselves in the dead-letter topic (headers carry the original topic, partition, offset and error).
- **Fix:** fix the cause, then replay the dead-lettered records into the original topic.

### KafkaProduceErrors

Records are not acknowledged by the brokers.

- **Check:** the producer error logs (`error_type`), broker health, and KafkaBrokerConnectErrors. Authorization errors mean the service's ACLs are missing the topic.
- **Fix:** restore the brokers or the ACLs. Callers of `Publish` got the error and decide whether to retry; records written through the outbox are retried by the relay.

### KafkaBrokerConnectErrors

Connections to the Kafka brokers fail.

- **Check:** the brokers' addresses (`…BROKERS`), network policy, and TLS or SASL settings and credentials. A rotated SASL password that the service didn't pick up looks like this.
- **Fix:** restore connectivity or the credentials; the client reconnects on its own.

## Outbox

### OutboxRelayFailing

The relay keeps failing to publish: Kafka or PostgreSQL is unavailable to it. Events stay in the outbox table; nothing is lost.

- **Check:** the relay's error logs, then KafkaProduceErrors and the PostgreSQL alerts.
- **Fix:** fix Kafka or PostgreSQL. The relay resumes on its own and publishes the backlog in order.

### OutboxNoActiveRelay

No replica holds the relay lock, so no events are published.

- **Check:** is the service running at all? Do the replicas log relay errors at startup?
- **Fix:** restart the service. If a replica holds the advisory lock but is stuck, restarting it releases the lock.

### OutboxPublishLagHigh

Events take more than a minute (p99) from being written to being published.

- **Check:** the outbox table's size, the relay's batch durations, and Kafka latency.
- **Fix:** a large backlog after an outage drains on its own; a growing one means Kafka is slow or the relay's batches are too small for the write rate.

## PostgreSQL

### PostgresPoolExhausted

Requests wait for a connection from the pool.

- **Check:** pool usage on the dashboard, slow queries (`pg_stat_activity`), and long transactions holding connections.
- **Fix:** fix the slow queries first. Raise `…MAX_CONNS` only within the server's `max_connections` divided by the number of replicas.

### PostgresPoolSaturated

More than 90% of the pool's connections are in use for 15 minutes; PostgresPoolExhausted is next.

- **Check and fix:** as for PostgresPoolExhausted.

## Resilience

### CircuitBreakerOpen

Calls to a dependency fail fast because it kept failing.

- **Check:** the dependency named by `breaker`: its health, its own alerts, its recent deploys.
- **Fix:** fix the dependency. The breaker half-opens on its own and closes once calls succeed.

### RetriesExhausted

Operations fail after every retry attempt.

- **Check:** the error logs for `operation`; the last error is logged.
- **Fix:** fix the failing dependency. Don't raise the retry count to hide it: retries multiply the load on a struggling dependency.

## Idempotency

### IdempotencyStoreErrors

The idempotency store (Redis or PostgreSQL) fails, so operations can't be deduplicated. Kafka records and requests are retried later instead of being run unprotected.

- **Check:** the store's own alerts (RedisPoolTimeouts, PostgreSQL alerts) and connectivity.
- **Fix:** restore the store; retried work then completes.

## Vault and credentials

### VaultTokenExpiring

The service's Vault token expires in under five minutes: renewal and re-login fail.

- **Check:** Vault's health (sealed?), the service's Vault role and its policies, and the auth method's credentials (Kubernetes service account token, AppRole secret).
- **Fix:** restore Vault or the auth configuration. After expiry the service can't read secrets or rotate credentials; restart it once Vault works.

### CredentialRotationFailing

Rotating a credential (database, Redis) fails; the current one stays in use until it expires.

- **Check:** the rotation logs for `resource`, and Vault (VaultTokenExpiring, the secrets engine's connection to the database).
- **Fix:** restore Vault or its database connection. Rotation retries on its own; CredentialExpiring fires if time runs out.

### CredentialExpiring

A credential expires in under two minutes and has not been replaced. Connections fail when it does.

- **Check:** CredentialRotationFailing and VaultTokenExpiring.
- **Fix:** restore rotation. If it can't be fixed in time, restart the service after Vault works: it fetches a new credential at startup.

## Authentication

### AuthKeysUnavailable

Tokens are signed with a key that isn't cached, and the identity provider's JWKS can't be fetched, so valid users are rejected.

- **Check:** the identity provider's health, and whether the service can reach `…JWKS_URL` (or the issuer's discovery document).
- **Fix:** restore the identity provider or the network path. Keys are refetched on the next unknown key ID.

### AuthInvalidTokensHigh

More than 20% of bearer tokens are invalid (expired tokens don't count).

- **Check:** `auth_tokens_total` by outcome (`invalid` or `unknown_key`), and the access logs of the 401 responses to find which clients send the tokens.
- **Fix:** a wrong `…AUDIENCE` or `…ISSUER` after a deploy: fix the config. A misconfigured client: fix the client. Many sources with bad signatures: treat as an attack.

### JWKSRefreshFailing

Background refreshes of the identity provider's keys fail. Cached keys still work, but a key rotation at the provider will reject tokens until the JWKS is reachable.

- **Check and fix:** as for AuthKeysUnavailable, before it fires.

### OAuth2TokenRequestsFailing

Getting a client-credentials token from the identity provider fails for `client`. Calls to that service fail once the cached token expires.

- **Check:** the `oauth2 token request failed` logs, and the `outcome` label: `unauthorized` means the client ID or secret is wrong or revoked; `invalid_argument` a wrong scope or audience; `unavailable` the identity provider is unreachable.
- **Fix:** fix the credentials (`…CLIENT_ID`, `…CLIENT_SECRET`) or the scopes, or restore the identity provider.

## Redis

### RedisPoolTimeouts

Commands time out waiting for a Redis connection.

- **Check:** Redis latency and CPU, slow commands (`SLOWLOG`), and the pool's usage on the dashboard.
- **Fix:** fix slow commands or Redis itself; raise `…POOL_SIZE` if the pool is simply too small for the load.

### CacheRedisErrors

More than 5% of lookups in `cache` fail to reach Redis and are served by the loader, which now carries the full load.

- **Check:** Redis health and RedisPoolTimeouts; then the load on what the loader calls (usually PostgreSQL).
- **Fix:** restore Redis. Watch the loader's dependency meanwhile: it may need more capacity until the cache is back.

## Scheduled jobs

### ScheduledJobFailing

Runs of `job_name` failed or panicked in the last 30 minutes, so its work isn't getting done.

- **Check:** the `scheduled job failed` and `scheduled job panicked` logs (the `job` field names the job), and its traces (`schedule <name>`).
- **Fix:** fix the job or its dependency. A job that times out needs a longer `WithTimeout` or smaller batches. Runs skipped because another replica holds the lease are normal and don't count.
