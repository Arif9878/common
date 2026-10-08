// Package batch groups items and hands them to a handler in batches.
//
//	p := batch.New("audit-events", func(ctx context.Context, events []Event) error {
//		return store.InsertMany(ctx, events)
//	}, batch.WithSize(500), batch.WithFlushInterval(time.Second))
//	shutdown.Register(graceful.Drain, "audit-events", p.Close)
//
//	err := p.Add(ctx, event)
//
// # Flushing
//
// A batch is flushed when it reaches WithSize items, when its oldest item
// has waited WithFlushInterval, on [Processor.Flush], and on
// [Processor.Close]. Batches are handed to the handler in the order items
// were added. With WithFlushConcurrency(1), the default, one batch is
// handled at a time; higher values overlap batches and give up that
// ordering between batches.
//
// # Memory and backpressure
//
// At most WithMaxPending items wait for a batch (default: twice the batch
// size), plus the batches being handled. When the limit is reached,
// [Processor.Add] blocks until there is room or ctx is done, which slows
// producers down to the handler's pace. Nothing grows without bound.
//
// # Failures
//
// The handler receives a fresh slice it may keep. If it returns an error,
// the whole batch failed; to report that only some items failed, return a
// [*PartialError] listing their indexes. Failed items are passed to the
// WithOnFailure hook (by default, the count is logged) and are not
// retried or re-queued: retry inside the handler (see the retry package),
// or re-add items from the hook if that is safe. Handler panics are
// recovered and treated as a failure of the whole batch.
//
// Each handler call gets a context that expires after WithFlushTimeout
// (30s by default) and that is cancelled if Close's deadline expires.
package batch

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
)

// Handler processes one batch. items is never empty.
type Handler[T any] func(ctx context.Context, items []T) error

// PartialError reports that only the items at the given indexes of a batch
// failed.
type PartialError struct {
	Failed []int
	Err    error
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("%d items failed: %v", len(e.Failed), e.Err)
}

// Unwrap returns e.Err.
func (e *PartialError) Unwrap() error { return e.Err }

var (
	// ErrClosed is returned by Add and Flush after Close started. Its kind
	// is Unavailable.
	ErrClosed          = errors.Unavailable.New("batch: closed")
	errCloseDeadline   = errors.Timeout.New("batch: close deadline exceeded")
	errHandlerPanicked = errors.Internal.New("batch: handler panicked")
)

// Option configures [New].
type Option func(*options)

type options struct {
	size             int
	interval         time.Duration
	maxPending       int
	flushConcurrency int
	flushTimeout     time.Duration
	onFailure        func(ctx context.Context, items any, err error)
	logger           *slog.Logger
	meterProvider    metric.MeterProvider
}

// WithSize sets the maximum batch size. The default is 100.
func WithSize(n int) Option {
	return func(o *options) { o.size = max(n, 1) }
}

// WithFlushInterval sets how long the oldest item may wait before its batch
// is flushed although not full. The default is one second.
func WithFlushInterval(d time.Duration) Option {
	return func(o *options) { o.interval = d }
}

// WithMaxPending bounds the items waiting for a batch. The default is twice
// the batch size. Values below the batch size are raised to it.
func WithMaxPending(n int) Option {
	return func(o *options) { o.maxPending = n }
}

// WithFlushConcurrency sets how many batches may be handled at once. The
// default is 1, which preserves order between batches.
func WithFlushConcurrency(n int) Option {
	return func(o *options) { o.flushConcurrency = max(n, 1) }
}

// WithFlushTimeout bounds each handler call. The default is 30 seconds.
func WithFlushTimeout(d time.Duration) Option {
	return func(o *options) { o.flushTimeout = d }
}

// WithOnFailure calls fn with the items of a batch that failed and the
// error. items has type []T. The default logs the number of failed items.
func WithOnFailure[T any](fn func(ctx context.Context, items []T, err error)) Option {
	return func(o *options) {
		o.onFailure = func(ctx context.Context, items any, err error) { fn(ctx, items.([]T), err) }
	}
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProvider = mp }
}

// Processor batches items for a handler. It is safe for concurrent use.
type Processor[T any] struct {
	name    string
	handler Handler[T]
	opts    options

	input   chan T
	flushes chan chan error
	ctx     context.Context // cancelled when Close's deadline expires
	abort   context.CancelCauseFunc

	mu         sync.Mutex
	closed     bool
	closing    chan struct{}
	submitters sync.WaitGroup
	drain      chan struct{}
	done       chan struct{}

	sem      chan struct{} // flush concurrency
	inflight sync.WaitGroup
	pending  atomic.Int64

	metrics batchMetrics
}

type batchMetrics struct {
	items    metric.Int64Counter
	flushes  metric.Int64Counter
	size     metric.Int64Histogram
	duration metric.Float64Histogram
	attrs    metric.MeasurementOption
	outcome  map[string]metric.AddOption
	reason   map[string]metric.AddOption
	reg      metric.Registration
}

// New starts a processor. name identifies it in logs and metrics; use a
// fixed, low-cardinality value.
func New[T any](name string, handler Handler[T], opts ...Option) *Processor[T] {
	o := options{
		size:             100,
		interval:         time.Second,
		flushConcurrency: 1,
		flushTimeout:     30 * time.Second,
		logger:           slog.Default(),
		meterProvider:    otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.maxPending == 0 {
		o.maxPending = 2 * o.size
	}
	o.maxPending = max(o.maxPending, o.size)
	if o.onFailure == nil {
		logger := o.logger
		o.onFailure = func(ctx context.Context, items any, err error) {
			logger.ErrorContext(ctx, "batch failed", "processor", name,
				"items", reflectLen(items), logging.Err(err))
		}
	}

	p := &Processor[T]{
		name:    name,
		handler: handler,
		opts:    o,
		input:   make(chan T, o.maxPending-o.size),
		flushes: make(chan chan error),
		closing: make(chan struct{}),
		drain:   make(chan struct{}),
		done:    make(chan struct{}),
		sem:     make(chan struct{}, o.flushConcurrency),
	}
	p.ctx, p.abort = context.WithCancelCause(context.Background())
	p.initMetrics()
	go p.loop()
	return p
}

func reflectLen(items any) int {
	return reflect.ValueOf(items).Len()
}

func (p *Processor[T]) initMetrics() {
	meter := p.opts.meterProvider.Meter("github.com/Arif9878/common/go/concurrency/batch")
	procAttr := attribute.String("processor", p.name)
	m := &p.metrics
	m.attrs = metric.WithAttributeSet(attribute.NewSet(procAttr))
	m.items, _ = meter.Int64Counter("batch.items",
		metric.WithDescription("Items handled, by outcome: success or failure."))
	m.flushes, _ = meter.Int64Counter("batch.flushes",
		metric.WithDescription("Batches flushed, by reason: size, interval, manual or close."))
	m.size, _ = meter.Int64Histogram("batch.size", metric.WithUnit("{item}"),
		metric.WithDescription("Items per batch."),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000))
	m.duration, _ = meter.Float64Histogram("batch.flush.duration", metric.WithUnit("s"),
		metric.WithDescription("Handler run time per batch."))
	m.outcome = map[string]metric.AddOption{}
	for _, o := range []string{"success", "failure"} {
		m.outcome[o] = metric.WithAttributeSet(attribute.NewSet(procAttr, attribute.String("outcome", o)))
	}
	m.reason = map[string]metric.AddOption{}
	for _, r := range []string{"size", "interval", "manual", "close"} {
		m.reason[r] = metric.WithAttributeSet(attribute.NewSet(procAttr, attribute.String("reason", r)))
	}
	pending, _ := meter.Int64ObservableGauge("batch.pending",
		metric.WithDescription("Items added but not yet handed to the handler."))
	m.reg, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(pending, p.pending.Load(), m.attrs)
		return nil
	}, pending)
}

// Add queues item, blocking while the processor is at its pending limit.
// It returns ctx's error if ctx ends first and [ErrClosed] after Close
// started.
func (p *Processor[T]) Add(ctx context.Context, item T) error {
	if err := p.enter(); err != nil {
		return err
	}
	defer p.submitters.Done()
	select {
	case p.input <- item:
		p.pending.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closing:
		return ErrClosed
	}
}

func (p *Processor[T]) enter() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.submitters.Add(1)
	return nil
}

// Flush hands the items added so far to the handler and waits until they
// and all earlier batches have been handled. It returns the error of the
// batch it flushed (nil if there was nothing to flush); failures of earlier
// batches went to the WithOnFailure hook.
func (p *Processor[T]) Flush(ctx context.Context) error {
	if err := p.enter(); err != nil {
		return err
	}
	defer p.submitters.Done()

	reply := make(chan error, 1)
	select {
	case p.flushes <- reply:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closing:
		return ErrClosed
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loop owns the current batch. It is the only goroutine reading input.
func (p *Processor[T]) loop() {
	defer close(p.done)

	buf := make([]T, 0, p.opts.size)
	timer := time.NewTimer(p.opts.interval)
	timer.Stop()
	timerRunning := false

	add := func(item T) {
		buf = append(buf, item)
		if len(buf) == 1 {
			timer.Reset(p.opts.interval)
			timerRunning = true
		}
		if len(buf) >= p.opts.size {
			p.flush(buf, "size", nil)
			buf = make([]T, 0, p.opts.size)
			timer.Stop()
			timerRunning = false
		}
	}
	flushNow := func(reason string, reply chan error) {
		if timerRunning {
			timer.Stop()
			timerRunning = false
		}
		if len(buf) == 0 {
			if reply != nil {
				p.inflight.Wait()
				reply <- nil
			}
			return
		}
		p.flush(buf, reason, reply)
		buf = make([]T, 0, p.opts.size)
	}

	for {
		select {
		case item := <-p.input:
			add(item)
		case <-timer.C:
			timerRunning = false
			flushNow("interval", nil)
		case reply := <-p.flushes:
			// Take everything already added, so Flush covers all Adds
			// that returned before it was called.
			for drained := false; !drained; {
				select {
				case item := <-p.input:
					add(item)
				default:
					drained = true
				}
			}
			flushNow("manual", reply)
		case <-p.drain:
			for drained := false; !drained; {
				select {
				case item := <-p.input:
					add(item)
				default:
					drained = true
				}
			}
			flushNow("close", nil)
			p.inflight.Wait()
			return
		}
	}
}

// flush hands batch to the handler, on a new goroutine bounded by the flush
// concurrency. With a reply channel, it waits for all in-flight batches and
// sends this batch's error.
func (p *Processor[T]) flush(batch []T, reason string, reply chan error) {
	p.metrics.flushes.Add(context.Background(), 1, p.metrics.reason[reason])
	p.sem <- struct{}{} // blocks the loop, and so Add, while all slots are busy
	p.inflight.Add(1)
	result := make(chan error, 1)
	go func() {
		defer p.inflight.Done()
		defer func() { <-p.sem }()
		result <- p.handle(batch)
	}()
	if reply != nil {
		err := <-result
		p.inflight.Wait()
		reply <- err
	}
}

func (p *Processor[T]) handle(batch []T) error {
	ctx, cancel := context.WithTimeout(p.ctx, p.opts.flushTimeout)
	defer cancel()

	start := time.Now()
	err := p.callHandler(ctx, batch)
	m := &p.metrics
	m.duration.Record(ctx, time.Since(start).Seconds(), m.attrs)
	m.size.Record(ctx, int64(len(batch)), m.attrs)
	p.pending.Add(-int64(len(batch)))

	if err == nil {
		m.items.Add(ctx, int64(len(batch)), m.outcome["success"])
		return nil
	}

	failed := batch
	if pe, ok := errors.AsType[*PartialError](err); ok {
		failed = make([]T, 0, len(pe.Failed))
		for _, i := range pe.Failed {
			if i >= 0 && i < len(batch) {
				failed = append(failed, batch[i])
			}
		}
	}
	m.items.Add(ctx, int64(len(batch)-len(failed)), m.outcome["success"])
	m.items.Add(ctx, int64(len(failed)), m.outcome["failure"])
	if len(failed) > 0 {
		p.opts.onFailure(ctx, failed, err)
	}
	return err
}

func (p *Processor[T]) callHandler(ctx context.Context, batch []T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", errHandlerPanicked, r)
		}
	}()
	return p.handler(ctx, slices.Clip(batch))
}

// Close stops intake, flushes everything pending and waits for all batches
// to be handled. If ctx ends first, it cancels the handlers' context and
// returns a Timeout error; batches that then fail reach WithOnFailure. Its
// signature matches graceful.Hook. Calling it again waits for the same
// close.
func (p *Processor[T]) Close(ctx context.Context) error {
	p.mu.Lock()
	first := !p.closed
	p.closed = true
	p.mu.Unlock()

	if first {
		close(p.closing)
		go func() {
			p.submitters.Wait()
			close(p.drain)
		}()
	}

	select {
	case <-p.done:
		if p.metrics.reg != nil {
			_ = p.metrics.reg.Unregister()
		}
		return nil
	case <-ctx.Done():
		p.abort(errCloseDeadline)
		return errors.Timeout.Errorf("batch %s: close deadline exceeded with %d items pending: %w",
			p.name, p.pending.Load(), ctx.Err())
	}
}
