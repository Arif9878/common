package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/tracing"
	"github.com/Arif9878/common/go/resilience/retry"
)

// Handler processes one record.
type Handler func(ctx context.Context, r *kgo.Record) error

// BatchHandler processes records of one partition, in offset order.
type BatchHandler func(ctx context.Context, rs []*kgo.Record) error

// BatchError reports that a batch handler processed the first Processed
// records and then failed on record Processed with Err. Processed may be 0.
// The processed records are committed, and the rest are retried according
// to Err's kind. If record Processed still fails after retries, only that
// record goes to the dead-letter topic (or is skipped, or stops the
// partition), and the records after it are handled next.
type BatchError struct {
	Processed int
	Err       error
}

func (e *BatchError) Error() string {
	return fmt.Sprintf("batch failed after %d records: %v", e.Processed, e.Err)
}

// Unwrap returns e.Err.
func (e *BatchError) Unwrap() error { return e.Err }

// WithConcurrency caps handler calls running at once across all
// partitions. The default is 8.
func WithConcurrency(n int) Option { return func(o *options) { o.concurrency = max(n, 1) } }

// WithPartitionBuffer bounds records buffered per partition before polling
// waits. The default is twice the batch size (200).
func WithPartitionBuffer(n int) Option { return func(o *options) { o.partitionBuffer = max(n, 1) } }

// WithBatchSize sets the maximum batch for NewBatchConsumer. Default 100.
func WithBatchSize(n int) Option { return func(o *options) { o.batchSize = max(n, 1) } }

// WithBatchTimeout sets how long NewBatchConsumer waits to fill a batch.
// The default is 100ms.
func WithBatchTimeout(d time.Duration) Option { return func(o *options) { o.batchTimeout = d } }

// WithRetry sets the in-process retry policy for handler errors. The
// default makes 3 attempts with 100ms to 5s backoff for retryable kinds.
func WithRetry(p *retry.Policy) Option { return func(o *options) { o.retry = p } }

// WithDLQ publishes records that failed for good to topic with p, with
// headers dlq.original.topic, dlq.original.partition,
// dlq.original.offset, dlq.error.kind and dlq.error.
func WithDLQ(p *Producer, topic string) Option {
	return func(o *options) { o.dlq, o.dlqTopic = p, topic }
}

// WithSkipOnFailure commits records that failed for good, after logging
// them, instead of stopping their partition. Only use it when losing such
// records is acceptable.
func WithSkipOnFailure() Option { return func(o *options) { o.skipOnFailure = true } }

// WithRevokeTimeout bounds waiting for in-flight handlers when partitions
// are revoked. Keep it below the group's rebalance timeout. Default 30s.
func WithRevokeTimeout(d time.Duration) Option { return func(o *options) { o.revokeTimeout = d } }

// WithMaxPollRecords bounds records returned by one poll. Default 500.
func WithMaxPollRecords(n int) Option { return func(o *options) { o.maxPollRecords = max(n, 1) } }

// WithCommitInterval sets how often marked offsets are committed. Default 5s.
func WithCommitInterval(d time.Duration) Option { return func(o *options) { o.commitInterval = d } }

// WithResetToLatest starts a group without committed offsets at the end of
// each partition instead of the beginning.
func WithResetToLatest() Option { return func(o *options) { o.resetToLatest = true } }

var (
	errAlreadyRunning = errors.InvalidArgument.New("kafka: consumer is already running")
	errRevoked        = errors.Canceled.New("kafka: partition revoked")
)

type topicPartition struct {
	topic     string
	partition int32
}

// worker handles one partition. Only its goroutine reads ch.
type worker struct {
	tp      topicPartition
	ch      chan *kgo.Record
	stop    chan struct{} // closed: take no new records
	done    chan struct{} // closed when the goroutine exits
	revoked atomic.Bool   // set if the partition was given up before done
	failed  atomic.Bool   // partition stopped after a failure
}

func (w *worker) stopping() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// Consumer consumes topics as part of a consumer group. Create it with
// [NewConsumer] or [NewBatchConsumer], run it with [Consumer.Run] and stop
// it with [Consumer.Close].
type Consumer struct {
	cl     *kgo.Client
	o      options
	group  string
	batch  bool
	handle BatchHandler
	tracer *kotel.Tracer
	sem    chan struct{}

	handlerCtx context.Context // cancelled when Close's deadline expires
	abort      context.CancelFunc

	mu       sync.Mutex
	workers  map[topicPartition]*worker
	lag      map[topicPartition]int64
	stopPoll context.CancelFunc
	running  bool
	runDone  chan struct{}
	stopped  atomic.Int64
	closing  atomic.Bool

	closeOnce sync.Once
	closeErr  error

	records    metric.Int64Counter
	duration   metric.Float64Histogram
	size       metric.Int64Histogram
	duplicates metric.Int64Counter
	reg        metric.Registration
}

// NewConsumer creates a consumer of topics in group, calling h for each
// record. It checks that a broker is reachable.
func NewConsumer(ctx context.Context, cfg Config, group string, topics []string, h Handler, opts ...Option) (*Consumer, error) {
	return newConsumer(ctx, cfg, group, topics, false, func(ctx context.Context, rs []*kgo.Record) error {
		return h(ctx, rs[0])
	}, opts)
}

// NewBatchConsumer is like [NewConsumer] with a handler for batches of
// records from one partition.
func NewBatchConsumer(ctx context.Context, cfg Config, group string, topics []string, h BatchHandler, opts ...Option) (*Consumer, error) {
	return newConsumer(ctx, cfg, group, topics, true, h, opts)
}

func newConsumer(ctx context.Context, cfg Config, group string, topics []string, batch bool, h BatchHandler, opts []Option) (*Consumer, error) {
	if group == "" || len(topics) == 0 {
		return nil, errors.InvalidArgument.New("kafka: consumer needs a group and at least one topic")
	}
	o := newOptions(opts)
	if !batch {
		o.batchSize = 1
	}
	if o.dlq != nil && o.dlqTopic == "" {
		return nil, errors.InvalidArgument.New("kafka: WithDLQ needs a topic")
	}
	if o.idem != nil && o.idem.Store == nil {
		return nil, errors.InvalidArgument.New("kafka: WithIdempotency needs a store")
	}
	c := &Consumer{
		o: o, group: group, batch: batch, handle: h,
		sem:     make(chan struct{}, o.concurrency),
		workers: map[topicPartition]*worker{},
		lag:     map[topicPartition]int64{},
		runDone: make(chan struct{}),
		tracer: kotel.NewTracer(kotel.TracerProvider(o.tracerProv), kotel.TracerPropagator(o.propagators),
			kotel.ConsumerGroup(group)),
	}
	if o.idem != nil {
		c.handle = c.idempotent(h)
	}
	c.handlerCtx, c.abort = context.WithCancel(context.Background())
	c.initMetrics()

	kopts, err := cfg.clientOpts()
	if err != nil {
		return nil, err
	}
	kopts = append(kopts,
		kgo.WithHooks(kotel.NewKotel(kotel.WithTracer(c.tracer),
			kotel.WithMeter(kotel.NewMeter(kotel.MeterProvider(o.meterProv)))).Hooks()...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.AutoCommitMarks(),
		kgo.AutoCommitInterval(o.commitInterval),
		kgo.OnPartitionsAssigned(c.assigned),
		kgo.OnPartitionsRevoked(c.revoked),
		kgo.OnPartitionsLost(c.lost),
	)
	if o.resetToLatest {
		kopts = append(kopts, kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()))
	}
	cl, err := kgo.NewClient(append(kopts, o.kgoOpts...)...)
	if err != nil {
		return nil, errors.InvalidArgument.Wrap(err, "kafka: consumer")
	}
	if err := ping(ctx, cl); err != nil {
		cl.Close()
		return nil, err
	}
	c.cl = cl
	return c, nil
}

func (c *Consumer) initMetrics() {
	meter := c.o.meterProv.Meter("github.com/Arif9878/common/go/messaging/kafka")
	c.records, _ = meter.Int64Counter("kafka.consumer.records",
		metric.WithDescription("Records handled, by topic and outcome: success, dlq, skipped, failed (partition stopped)."))
	c.duration, _ = meter.Float64Histogram("kafka.consumer.process.duration", metric.WithUnit("s"),
		metric.WithDescription("Handler time per record or batch, including retries."))
	c.size, _ = meter.Int64Histogram("kafka.consumer.batch.size", metric.WithUnit("{record}"),
		metric.WithDescription("Records per handler call."),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000))
	c.duplicates, _ = meter.Int64Counter("kafka.consumer.duplicates",
		metric.WithDescription("Records skipped by WithIdempotency because they were already processed, counted once per delivered batch. They are also counted as success."))
	lag, _ := meter.Int64ObservableGauge("kafka.consumer.lag", metric.WithUnit("{record}"),
		metric.WithDescription("Records behind the high watermark at the last poll, per partition."))
	stopped, _ := meter.Int64ObservableGauge("kafka.consumer.partitions.stopped", metric.WithUnit("{partition}"),
		metric.WithDescription("Partitions paused after a record failed for good."))
	c.reg, _ = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		for tp, n := range c.lag {
			obs.ObserveInt64(lag, n, metric.WithAttributes(
				attribute.String("topic", tp.topic), attribute.Int("partition", int(tp.partition))))
		}
		obs.ObserveInt64(stopped, c.stopped.Load(), metric.WithAttributes(attribute.String("group", c.group)))
		return nil
	}, lag, stopped)
}

// Client returns the underlying franz-go client.
func (c *Consumer) Client() *kgo.Client { return c.cl }

// Run polls and dispatches records until ctx ends or Close is called, then
// returns nil. Run it once, for example with graceful.Manager.Go, and
// register Close in graceful.StopIntake.
func (c *Consumer) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running || c.closing.Load() {
		c.mu.Unlock()
		return errAlreadyRunning
	}
	c.running = true
	pollCtx, stop := context.WithCancel(ctx)
	c.stopPoll = stop
	c.mu.Unlock()
	defer close(c.runDone)
	defer stop()

	for {
		fetches := c.cl.PollRecords(pollCtx, c.o.maxPollRecords)
		if fetches.IsClientClosed() || pollCtx.Err() != nil {
			return nil
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			c.o.logger.Warn("kafka fetch error", logging.KeyTopic, topic, logging.KeyPartition, partition, logging.Err(classify(err)))
		})
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if len(p.Records) == 0 {
				return
			}
			tp := topicPartition{p.Topic, p.Partition}
			last := p.Records[len(p.Records)-1].Offset
			c.mu.Lock()
			w := c.workers[tp]
			if w != nil {
				c.lag[tp] = max(p.HighWatermark-last-1, 0)
			}
			c.mu.Unlock()
			if w == nil {
				return // revoked meanwhile; uncommitted records go to the new owner
			}
			for _, r := range p.Records {
				select {
				case w.ch <- r: // blocks while the partition buffer is full: backpressure
				case <-w.stop:
					return
				case <-pollCtx.Done():
					return
				}
			}
		})
	}
}

func (c *Consumer) assigned(_ context.Context, _ *kgo.Client, m map[string][]int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for topic, parts := range m {
		for _, p := range parts {
			tp := topicPartition{topic, p}
			if _, ok := c.workers[tp]; ok {
				continue
			}
			w := &worker{tp: tp, ch: make(chan *kgo.Record, c.o.partitionBuffer),
				stop: make(chan struct{}), done: make(chan struct{})}
			c.workers[tp] = w
			go c.runWorker(w)
		}
	}
}

// take removes the workers of m's partitions and returns them.
func (c *Consumer) take(m map[string][]int32) []*worker {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ws []*worker
	for topic, parts := range m {
		for _, p := range parts {
			tp := topicPartition{topic, p}
			if w, ok := c.workers[tp]; ok {
				ws = append(ws, w)
				delete(c.workers, tp)
				delete(c.lag, tp)
			}
		}
	}
	return ws
}

// stopWorkers stops ws and waits for their in-flight handler calls, up to
// timeout. Workers still running then are marked revoked, so they never
// mark offsets afterwards. It reports whether all finished.
func (c *Consumer) stopWorkers(ws []*worker, timeout time.Duration) bool {
	for _, w := range ws {
		close(w.stop)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	all := true
	for _, w := range ws {
		select {
		case <-w.done:
		case <-deadline.C:
			all = false
		}
		if !all {
			break
		}
	}
	for _, w := range ws {
		select {
		case <-w.done:
		default:
			w.revoked.Store(true)
		}
		if w.failed.Load() {
			c.stopped.Add(-1)
			c.cl.ResumeFetchPartitions(map[string][]int32{w.tp.topic: {w.tp.partition}})
		}
	}
	return all
}

func (c *Consumer) revoked(ctx context.Context, cl *kgo.Client, m map[string][]int32) {
	ws := c.take(m)
	if !c.stopWorkers(ws, c.o.revokeTimeout) {
		c.o.logger.Warn("kafka handlers still running at partition revoke; their records will be redelivered", "group", c.group)
	}
	if err := cl.CommitMarkedOffsets(ctx); err != nil {
		c.o.logger.Warn("kafka commit on revoke failed", "group", c.group, logging.Err(classify(err)))
	}
}

func (c *Consumer) lost(_ context.Context, _ *kgo.Client, m map[string][]int32) {
	c.stopWorkers(c.take(m), c.o.revokeTimeout)
}

func (c *Consumer) runWorker(w *worker) {
	defer close(w.done)
	for {
		var first *kgo.Record
		select {
		case <-w.stop:
			return
		case first = <-w.ch:
		}
		batch := c.fill(w, []*kgo.Record{first})
		if batch == nil {
			return // stopped while filling: not started, will be redelivered
		}
		if w.failed.Load() {
			continue // partition stopped: discard until it is revoked or resumed
		}
		c.process(w, batch)
	}
}

// fill adds buffered records to batch up to the batch size, waiting at most
// the batch timeout. It returns nil if the worker is stopped meanwhile.
func (c *Consumer) fill(w *worker, batch []*kgo.Record) []*kgo.Record {
	if c.o.batchSize <= 1 {
		return batch
	}
	timer := time.NewTimer(c.o.batchTimeout)
	defer timer.Stop()
	for len(batch) < c.o.batchSize {
		select {
		case r := <-w.ch:
			batch = append(batch, r)
		case <-timer.C:
			return batch
		case <-w.stop:
			return nil
		}
	}
	return batch
}

func (c *Consumer) process(w *worker, batch []*kgo.Record) {
	select {
	case c.sem <- struct{}{}:
	case <-w.stop:
		return // not started yet
	}
	defer func() { <-c.sem }()

	first := batch[0]
	ctx := logging.ContextWithAttrs(c.handlerCtx,
		slog.String(logging.KeyTopic, first.Topic),
		slog.Int(logging.KeyPartition, int(first.Partition)),
		slog.Int64(logging.KeyOffset, first.Offset))
	ctx, span := c.startSpan(ctx, batch)
	var err error
	defer func() { tracing.End(span, &err) }()

	topicAttr := metric.WithAttributes(attribute.String("topic", first.Topic))
	start := time.Now()
	c.size.Record(ctx, int64(len(batch)), topicAttr)

	var dups duplicateSet
	if c.o.idem != nil {
		dups = duplicateSet{}
		ctx = context.WithValue(ctx, duplicatesKey{}, dups)
	}
	remaining := batch
	for len(remaining) > 0 {
		var culprit bool // err is about remaining[0] alone (a BatchError)
		attempt := 0
		err = c.o.retry.Do(ctx, func(ctx context.Context) error {
			if attempt++; attempt > 1 && w.stopping() {
				return retry.Permanent(errRevoked) // don't keep retrying a partition we are giving up
			}
			err := c.call(ctx, remaining)
			culprit = false
			if be, ok := errors.AsType[*BatchError](err); ok && be.Processed >= 0 && be.Processed < len(remaining) {
				if be.Processed > 0 {
					c.mark(w, remaining[be.Processed-1])
					c.count(ctx, first.Topic, "success", be.Processed)
					remaining = remaining[be.Processed:]
				}
				culprit = true
				return be.Err
			}
			return err
		})

		switch {
		case err == nil:
			c.mark(w, remaining[len(remaining)-1])
			c.count(ctx, first.Topic, "success", len(remaining))
			remaining = nil
		case errors.Is(err, errRevoked), c.handlerCtx.Err() != nil:
			// Interrupted by revoke or shutdown: leave uncommitted for redelivery.
			remaining = nil
		case culprit && len(remaining) > 1:
			// Fail only the record the handler reported, then carry on
			// with the rest of the batch, unless that stopped the partition.
			c.fail(ctx, w, remaining[:1], err)
			if w.failed.Load() {
				remaining = nil
			} else {
				remaining = remaining[1:]
			}
		default:
			c.fail(ctx, w, remaining, err)
			remaining = nil
		}
	}
	c.duration.Record(ctx, time.Since(start).Seconds(), topicAttr)
	if n := len(dups); n > 0 {
		c.duplicates.Add(ctx, int64(n), topicAttr)
		c.o.logger.DebugContext(ctx, "kafka records skipped as duplicates", slog.Int("count", n))
	}
}

func (c *Consumer) call(ctx context.Context, rs []*kgo.Record) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = retry.Permanent(errors.Internal.Errorf("kafka handler panicked: %v", r))
		}
	}()
	return c.handle(ctx, rs)
}

func (c *Consumer) startSpan(ctx context.Context, batch []*kgo.Record) (context.Context, trace.Span) {
	if len(batch) == 1 {
		// kotel builds the span from the record's propagated context; keep
		// our context (cancellation, log attributes) and attach the span.
		_, span := c.tracer.WithProcessSpan(batch[0])
		return trace.ContextWithSpan(ctx, span), span
	}
	links := make([]trace.Link, 0, len(batch))
	for _, r := range batch {
		rc := c.o.propagators.Extract(context.Background(), kotel.NewRecordCarrier(r))
		if sc := trace.SpanContextFromContext(rc); sc.IsValid() {
			links = append(links, trace.Link{SpanContext: sc})
		}
	}
	return c.o.tracerProv.Tracer("github.com/Arif9878/common/go/messaging/kafka").Start(ctx,
		batch[0].Topic+" process", trace.WithSpanKind(trace.SpanKindConsumer), trace.WithLinks(links...),
		trace.WithAttributes(attribute.Int("messaging.batch.message_count", len(batch))))
}

func (c *Consumer) mark(w *worker, r *kgo.Record) {
	if w.revoked.Load() {
		return // the partition belongs to another consumer now
	}
	c.cl.MarkCommitRecords(r)
}

func (c *Consumer) count(ctx context.Context, topic, outcome string, n int) {
	c.records.Add(ctx, int64(n), metric.WithAttributes(attribute.String("topic", topic), attribute.String("outcome", outcome)))
}

func (c *Consumer) fail(ctx context.Context, w *worker, rs []*kgo.Record, err error) {
	first, last := rs[0], rs[len(rs)-1]
	attrs := []any{logging.KeyTopic, first.Topic, logging.KeyPartition, first.Partition,
		"first_offset", first.Offset, "last_offset", last.Offset, logging.Err(err)}

	if c.o.dlq != nil {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		derr := c.o.dlq.Publish(dctx, c.dlqRecords(rs, err)...)
		if derr == nil {
			c.mark(w, last)
			c.count(ctx, first.Topic, "dlq", len(rs))
			c.o.logger.WarnContext(ctx, "kafka records sent to dead-letter topic", append(attrs, "dlq_topic", c.o.dlqTopic)...)
			return
		}
		attrs = append(attrs, "dlq_error", derr.Error())
	} else if c.o.skipOnFailure {
		c.mark(w, last)
		c.count(ctx, first.Topic, "skipped", len(rs))
		c.o.logger.ErrorContext(ctx, "kafka records skipped after failure", attrs...)
		return
	}

	w.failed.Store(true)
	c.stopped.Add(1)
	c.cl.PauseFetchPartitions(map[string][]int32{first.Topic: {first.Partition}})
	c.count(ctx, first.Topic, "failed", len(rs))
	c.o.logger.ErrorContext(ctx, "kafka partition stopped: records failed and were not committed", attrs...)
}

func (c *Consumer) dlqRecords(rs []*kgo.Record, err error) []*kgo.Record {
	msg := err.Error()
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	out := make([]*kgo.Record, len(rs))
	for i, r := range rs {
		headers := append([]kgo.RecordHeader{}, r.Headers...)
		headers = append(headers,
			kgo.RecordHeader{Key: "dlq.original.topic", Value: []byte(r.Topic)},
			kgo.RecordHeader{Key: "dlq.original.partition", Value: []byte(strconv.Itoa(int(r.Partition)))},
			kgo.RecordHeader{Key: "dlq.original.offset", Value: []byte(strconv.FormatInt(r.Offset, 10))},
			kgo.RecordHeader{Key: "dlq.error.kind", Value: []byte(errors.KindOf(err).String())},
			kgo.RecordHeader{Key: "dlq.error", Value: []byte(msg)},
		)
		out[i] = &kgo.Record{Topic: c.o.dlqTopic, Key: r.Key, Value: r.Value, Headers: headers}
	}
	return out
}

// Close stops polling, waits within ctx for in-flight handler calls,
// commits marked offsets and leaves the group. Records buffered but not
// started are not committed and will be redelivered. If ctx ends first,
// in-flight handlers' contexts are cancelled. Its signature matches
// graceful.Hook (register it in graceful.StopIntake).
func (c *Consumer) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		c.mu.Lock()
		running, stop := c.running, c.stopPoll
		c.mu.Unlock()
		if running {
			stop()
			select {
			case <-c.runDone:
			case <-ctx.Done():
			}
		}

		all := map[string][]int32{}
		c.mu.Lock()
		for tp := range c.workers {
			all[tp.topic] = append(all[tp.topic], tp.partition)
		}
		c.mu.Unlock()
		timeout := time.Until(deadlineOr(ctx, time.Now().Add(c.o.revokeTimeout)))
		if !c.stopWorkers(c.take(all), timeout) {
			c.abort()
			c.closeErr = errors.Timeout.New("kafka: consumer closed with handlers still running; their records will be redelivered")
		}

		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := c.cl.CommitMarkedOffsets(cctx); err != nil {
			c.closeErr = errors.Join(c.closeErr, classify(err))
		}
		c.cl.Close() // leaves the group
		c.abort()
		if c.reg != nil {
			_ = c.reg.Unregister()
		}
	})
	return c.closeErr
}

func deadlineOr(ctx context.Context, fallback time.Time) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return fallback
}
