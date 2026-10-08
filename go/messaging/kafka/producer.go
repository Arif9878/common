package kafka

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
)

// Acks is the acknowledgement a producer waits for.
type Acks int

const (
	// AcksAll waits for all in-sync replicas and enables idempotent
	// production. It is the default.
	AcksAll Acks = iota
	// AcksLeader waits for the partition leader only. Records acknowledged
	// by a leader that then fails can be lost; idempotence is disabled.
	AcksLeader
	// AcksNone does not wait at all; delivery errors are not reported.
	AcksNone
)

// WithAcks sets the producer acknowledgement level.
func WithAcks(a Acks) Option { return func(o *options) { o.acks = a } }

// WithLinger sets how long the producer waits to fill a batch before
// sending. The default is 5ms; 0 sends immediately.
func WithLinger(d time.Duration) Option { return func(o *options) { o.linger = d } }

// WithMaxBufferedRecords bounds records waiting for acknowledgement. When
// the buffer is full, publishing blocks. The default is 10000.
func WithMaxBufferedRecords(n int) Option { return func(o *options) { o.maxBuffered = n } }

// WithDeliveryTimeout bounds how long the client keeps retrying a record
// before failing it. The default is 30s.
func WithDeliveryTimeout(d time.Duration) Option { return func(o *options) { o.delivery = d } }

// Producer publishes records. It is safe for concurrent use.
type Producer struct {
	cl      *kgo.Client
	records metric.Int64Counter

	once     sync.Once
	closeErr error
}

// NewProducer creates a producer and checks that a broker is reachable.
func NewProducer(ctx context.Context, cfg Config, opts ...Option) (*Producer, error) {
	o := newOptions(opts)
	kopts, err := cfg.clientOpts()
	if err != nil {
		return nil, err
	}
	kopts = append(kopts,
		o.hooks(""),
		kgo.ProducerLinger(o.linger),
		kgo.MaxBufferedRecords(o.maxBuffered),
		kgo.RecordDeliveryTimeout(o.delivery),
		kgo.ProducerBatchCompression(kgo.SnappyCompression(), kgo.NoCompression()),
	)
	switch o.acks {
	case AcksLeader:
		kopts = append(kopts, kgo.RequiredAcks(kgo.LeaderAck()), kgo.DisableIdempotentWrite())
	case AcksNone:
		kopts = append(kopts, kgo.RequiredAcks(kgo.NoAck()), kgo.DisableIdempotentWrite())
	default:
		kopts = append(kopts, kgo.RequiredAcks(kgo.AllISRAcks()))
	}
	cl, err := kgo.NewClient(append(kopts, o.kgoOpts...)...)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "kafka: producer")
	}
	if err := ping(ctx, cl); err != nil {
		cl.Close()
		return nil, err
	}
	p := &Producer{cl: cl}
	p.records, _ = o.meterProv.Meter("github.com/Arif9878/common/go/messaging/kafka").Int64Counter(
		"kafka.producer.records", metric.WithDescription("Records published, by topic and outcome (ok or the error kind)."))
	return p, nil
}

// Client returns the underlying franz-go client.
func (p *Producer) Client() *kgo.Client { return p.cl }

func (p *Producer) count(ctx context.Context, r *kgo.Record, err error) {
	outcome := "ok"
	if err != nil {
		outcome = errors.KindOf(err).String()
	}
	p.records.Add(ctx, 1, metric.WithAttributes(attribute.String("topic", r.Topic), attribute.String("outcome", outcome)))
}

// Publish sends records and waits until all are acknowledged. ctx carries
// the trace context into the record headers. It returns the first error,
// classified (most delivery failures are Unavailable or Timeout).
func (p *Producer) Publish(ctx context.Context, records ...*kgo.Record) error {
	results := p.cl.ProduceSync(ctx, records...)
	var first error
	for _, r := range results {
		err := classify(r.Err)
		p.count(ctx, r.Record, err)
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// PublishAsync sends r without waiting for acknowledgement and calls done,
// if not nil, with the result. It blocks while the buffer is full, until
// there is room or ctx ends. done runs on a client goroutine and must not
// block.
func (p *Producer) PublishAsync(ctx context.Context, r *kgo.Record, done func(*kgo.Record, error)) {
	p.cl.Produce(ctx, r, func(r *kgo.Record, err error) {
		err = classify(err)
		p.count(ctx, r, err)
		if done != nil {
			done(r, err)
		}
	})
}

// Close waits within ctx for buffered records to be acknowledged, then
// closes the client. Records not acknowledged by then fail. Its signature
// matches graceful.Hook (register it in graceful.Drain, after consumers).
func (p *Producer) Close(ctx context.Context) error {
	p.once.Do(func() {
		if err := p.cl.Flush(ctx); err != nil {
			p.closeErr = errors.Timeout.Errorf("kafka: producer closed with %d unacknowledged records: %w",
				p.cl.BufferedProduceRecords(), err)
		}
		p.cl.Close()
	})
	return p.closeErr
}

// Serializer encodes values of type T.
type Serializer[T any] interface {
	Serialize(v T) ([]byte, error)
}

// Deserializer decodes values of type T.
type Deserializer[T any] interface {
	Deserialize(b []byte) (T, error)
}

// JSON serializes values as JSON.
type JSON[T any] struct{}

// Serialize encodes v as JSON.
func (JSON[T]) Serialize(v T) ([]byte, error) { return json.Marshal(v) }

// Deserialize decodes JSON into a T.
func (JSON[T]) Deserialize(b []byte) (T, error) {
	var v T
	err := json.Unmarshal(b, &v)
	return v, err
}

// Encode builds a record for topic with key and v encoded by s. Encoding
// errors have kind InvalidArgument.
func Encode[T any](topic string, key []byte, v T, s Serializer[T]) (*kgo.Record, error) {
	b, err := s.Serialize(v)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "kafka: encode "+topic)
	}
	return &kgo.Record{Topic: topic, Key: key, Value: b}, nil
}

// Typed adapts a handler of decoded values to a [Handler]. Records that do
// not decode fail with kind InvalidArgument, which is not retried: they go
// to the dead-letter topic (or stop the partition) as poison messages.
func Typed[T any](d Deserializer[T], h func(ctx context.Context, r *kgo.Record, v T) error) Handler {
	return func(ctx context.Context, r *kgo.Record) error {
		v, err := d.Deserialize(r.Value)
		if err != nil {
			return errors.InvalidArgument.Wrap(err, "kafka: decode")
		}
		return h(ctx, r, v)
	}
}
