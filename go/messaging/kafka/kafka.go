// Package kafka provides a producer and a consumer built on
// github.com/twmb/franz-go, with tracing, metrics, bounded concurrency,
// retries, dead-lettering and graceful shutdown.
//
// Records are *kgo.Record values throughout: Kafka concepts (topics,
// partitions, offsets, keys, headers) stay visible, and [Producer.Client]
// and [Consumer.Client] expose the underlying *kgo.Client.
//
// # Consumer semantics
//
// Delivery is at least once. A record's offset is marked for commit only
// after its handler succeeded or it was sent to the dead-letter topic.
// Marked offsets are committed every 5s, and synchronously when partitions
// are revoked and on Close. A record whose handling was interrupted
// (crash, rebalance, shutdown deadline) is delivered again, so handlers
// must be idempotent (see the idempotency package).
//
// Ordering is preserved per partition: each assigned partition has one
// worker that handles its records (or batches) one after another.
// Partitions are processed in parallel, with at most WithConcurrency
// handler calls running at once across all partitions. Records with the
// same key go to the same partition, so they are handled in order.
//
// Memory is bounded: each partition buffers at most WithPartitionBuffer
// records. When a partition's buffer is full, polling waits, which slows
// fetching to the speed of the handlers (backpressure). No goroutine is
// created per record.
//
// # Failures
//
// A handler error is retried in process with the WithRetry policy (by
// default 3 attempts with backoff, for retryable error kinds only: see
// errors.IsRetryable). When retries are exhausted or the error is not
// retryable, the record is:
//
//   - published to the next retry topic, if WithRetryTopics is set and the
//     error is retryable, and handled again after that topic's delay; its
//     offset is committed, so the partition moves on meanwhile;
//   - published to the dead-letter topic, if WithDLQ is set, with headers
//     describing the failure; then its offset is committed;
//   - otherwise skipped with an error log, if WithSkipOnFailure is set;
//   - otherwise its partition is paused: nothing after it is processed or
//     committed until the service restarts (or the partition moves to
//     another consumer, which retries it). Other partitions continue. This
//     default never loses a record silently; watch
//     kafka.consumer.partitions.stopped.
//
// Panics in handlers are recovered and treated as non-retryable errors.
//
// # Batches
//
// [NewBatchConsumer] hands records to the handler in per-partition batches
// of up to WithBatchSize records, waiting up to WithBatchTimeout to fill a
// batch. A batch handler that fails on one record returns [*BatchError]
// with the number of records processed before it; those are committed and
// the rest are retried. If that record still fails, only it is handled as
// a failure (see above) and the batch continues after it. Any other error
// retries or fails the whole batch, since the failed record is unknown.
//
// # Idempotency
//
// [WithIdempotency] skips records already processed, using an idempotency.Store
// shared by all replicas, such as Redis:
//
//	kafka.WithIdempotency(kafka.Idempotency{Store: redisstore.New(rdb)})
//
// Each record's key (by default group, topic, partition and offset; see
// HeaderKey for event IDs) is claimed before the handler runs and completed
// after it succeeds. Records whose key is already completed are committed
// without calling the handler and counted in kafka.consumer.duplicates. A key
// still claimed by another consumer is retried later, which keeps partition
// order. Processing stays at least once across a crash between the handler's
// side effects and completing the key; see the idempotency package.
//
// # Rebalancing
//
// When partitions are revoked, their workers stop taking new records,
// in-flight handler calls finish (up to WithRevokeTimeout), marked offsets
// are committed, and only then is the partition released. Buffered records
// that were not started are dropped; they are uncommitted, so the new
// owner receives them. Lost partitions (session timeout) cannot be
// committed and are simply stopped.
//
// # Producer semantics
//
// By default every write is acknowledged by all in-sync replicas with
// idempotent production, so retries inside the client cannot create
// duplicates or reorder records of a partition. [Producer.Publish] waits
// for acknowledgement; [Producer.PublishAsync] does not. The buffer of
// unacknowledged records is bounded; when full, publishing blocks until
// there is room or ctx ends.
package kafka

import (
	"context"
	"crypto/tls"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/resilience/retry"
)

// Config holds connection settings shared by producers and consumers.
// Environment variable names are relative; the service chooses the prefix,
// for example KAFKA_.
type Config struct {
	// Brokers are the seed brokers, host:port; the client discovers the rest
	// of the cluster from them.
	Brokers []string `env:"BROKERS,required" envSeparator:"," validate:"min=1"`
	// ClientID identifies the client in broker logs and quotas; empty uses
	// franz-go's default.
	ClientID string `env:"CLIENT_ID"`
	// TLS connects to the brokers with TLS, verifying their certificates.
	TLS bool `env:"TLS"`
	// TLSServerName overrides the name checked in broker certificates.
	TLSServerName string `env:"TLS_SERVER_NAME"`
	// SASLMechanism is "", "plain", "scram-sha-256" or "scram-sha-512".
	SASLMechanism string `env:"SASL_MECHANISM" validate:"omitempty,oneof=plain scram-sha-256 scram-sha-512"`
	// SASLUsername and SASLPassword authenticate with SASLMechanism.
	SASLUsername string `env:"SASL_USERNAME"`
	// SASLPassword is the SASL password.
	SASLPassword config.Secret `env:"SASL_PASSWORD"`
	// DialTimeout bounds opening one broker connection.
	DialTimeout time.Duration `env:"DIAL_TIMEOUT" envDefault:"10s"`
}

func (c Config) clientOpts() ([]kgo.Opt, error) {
	if err := config.Validate(c); err != nil {
		return nil, err
	}
	opts := []kgo.Opt{kgo.SeedBrokers(c.Brokers...)}
	if c.ClientID != "" {
		opts = append(opts, kgo.ClientID(c.ClientID))
	}
	if c.DialTimeout > 0 {
		opts = append(opts, kgo.DialTimeout(c.DialTimeout))
	}
	if c.TLS {
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.TLSServerName}))
	}
	user, pass := c.SASLUsername, c.SASLPassword.Reveal()
	switch c.SASLMechanism {
	case "plain":
		opts = append(opts, kgo.SASL(plain.Auth{User: user, Pass: pass}.AsMechanism()))
	case "scram-sha-256":
		opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha256Mechanism()))
	case "scram-sha-512":
		opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: pass}.AsSha512Mechanism()))
	}
	return opts, nil
}

// Option configures [NewProducer], [NewConsumer] and [NewBatchConsumer].
// Options that only concern one of them are ignored by the others.
type Option func(*options)

type options struct {
	logger      *slog.Logger
	meterProv   metric.MeterProvider
	tracerProv  trace.TracerProvider
	propagators propagation.TextMapPropagator
	kgoOpts     []kgo.Opt

	// producer
	acks        Acks
	linger      time.Duration
	maxBuffered int
	delivery    time.Duration

	// consumer
	concurrency     int
	partitionBuffer int
	batchSize       int
	batchTimeout    time.Duration
	retry           *retry.Policy
	dlq             *Producer
	dlqTopic        string
	skipOnFailure   bool
	revokeTimeout   time.Duration
	maxPollRecords  int
	commitInterval  time.Duration
	resetToLatest   bool
	idem            *Idempotency
	retryProducer   *Producer
	retrySteps      []RetryTopic
	tenant          bool
}

func newOptions(opts []Option) options {
	o := options{
		logger:         slog.Default(),
		meterProv:      otel.GetMeterProvider(),
		tracerProv:     otel.GetTracerProvider(),
		propagators:    otel.GetTextMapPropagator(),
		acks:           AcksAll,
		linger:         5 * time.Millisecond,
		maxBuffered:    10_000,
		delivery:       30 * time.Second,
		concurrency:    8,
		batchSize:      100,
		batchTimeout:   100 * time.Millisecond,
		revokeTimeout:  30 * time.Second,
		maxPollRecords: 500,
		commitInterval: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.partitionBuffer == 0 {
		o.partitionBuffer = 2 * o.batchSize
	}
	if o.retry == nil {
		o.retry = retry.New(retry.WithName("kafka.consumer"), retry.WithMeterProvider(o.meterProv),
			retry.WithMaxAttempts(3), retry.WithExponentialBackoff(100*time.Millisecond, 5*time.Second))
	}
	return o
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option { return func(o *options) { o.meterProv = mp } }

// WithTracerProvider sets the tracer provider. The default is the global one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProv = tp }
}

// WithPropagators sets how trace context is written to and read from
// record headers. The default is the global propagator.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(o *options) { o.propagators = p }
}

// WithKgoOptions adds native franz-go client options, applied last.
func WithKgoOptions(opts ...kgo.Opt) Option {
	return func(o *options) { o.kgoOpts = append(o.kgoOpts, opts...) }
}

func (o options) hooks(group string) kgo.Opt {
	topts := []kotel.TracerOpt{kotel.TracerProvider(o.tracerProv), kotel.TracerPropagator(o.propagators)}
	if group != "" {
		topts = append(topts, kotel.ConsumerGroup(group))
	}
	k := kotel.NewKotel(
		kotel.WithTracer(kotel.NewTracer(topts...)),
		kotel.WithMeter(kotel.NewMeter(kotel.MeterProvider(o.meterProv))),
	)
	return kgo.WithHooks(k.Hooks()...)
}

// classify gives client errors a kind: context errors keep theirs, and
// everything else from the client is a broker or network problem.
func classify(err error) error {
	if err == nil || errors.KindOf(err) != errors.Unknown {
		return err
	}
	switch {
	case errors.Is(err, kgo.ErrClientClosed):
		return errors.Unavailable.Wrap(err, "kafka: client closed")
	case errors.Is(err, kgo.ErrRecordTimeout), errors.Is(err, kgo.ErrRecordRetries):
		return errors.Timeout.Wrap(err, "kafka")
	case errors.Is(err, kgo.ErrMaxBuffered):
		return errors.Unavailable.Wrap(err, "kafka: producer buffer full")
	}
	return errors.Unavailable.Wrap(err, "kafka")
}

func ping(ctx context.Context, cl *kgo.Client) error {
	if err := cl.Ping(ctx); err != nil {
		return errors.Join(errors.New("kafka: no broker reachable"), classify(err))
	}
	return nil
}
