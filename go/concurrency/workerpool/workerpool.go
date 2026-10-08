// Package workerpool runs tasks on a fixed number of goroutines fed by a
// bounded queue.
//
//	pool := workerpool.New("thumbnails", workerpool.WithWorkers(20), workerpool.WithQueueSize(1000))
//	shutdown.Register(graceful.Drain, "thumbnails", pool.Shutdown)
//
//	err := pool.Submit(ctx, func(ctx context.Context) error {
//		return render(ctx, img)
//	})
//
// # Backpressure
//
// The pool never creates a goroutine per task and never grows its queue.
// When the queue is full, [Pool.Submit] blocks until there is room or ctx
// is done, and [Pool.TrySubmit] fails immediately with [ErrQueueFull].
// Choose per call: block where the caller can slow down (a consumer loop),
// reject where it cannot (an HTTP handler, which should answer 503).
// WithQueueSize(0) hands each task directly to an idle worker.
//
// # Tasks
//
// Tasks are fire-and-forget: their errors are counted, passed to the
// WithOnError hook (by default, logged) and not returned to the submitter.
// For fan-out where the caller needs the results, use
// golang.org/x/sync/errgroup with SetLimit instead.
//
// A task's context carries the values of the context passed to Submit
// (trace span, request ID) but not its cancellation or deadline: the
// submitter returning does not cancel work it handed off. The task context
// is cancelled only if Shutdown's deadline expires. Panics are recovered,
// counted and reported as errors of kind Internal; the worker keeps
// running.
//
// # Shutdown
//
// [Pool.Shutdown] stops intake (Submit returns [ErrClosed]), then waits
// for queued and running tasks to finish. If its ctx ends first, it cancels
// the context of running tasks, discards tasks still queued (counted as
// dropped and reported in the error) and returns without waiting for tasks
// that ignore cancellation.
package workerpool

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
)

// Task is a unit of work.
type Task func(ctx context.Context) error

var (
	// ErrQueueFull is returned by TrySubmit when the queue is full. Its kind
	// is Unavailable.
	ErrQueueFull = errors.Unavailable.New("workerpool: queue full")
	// ErrClosed is returned by Submit and TrySubmit after Shutdown started.
	// Its kind is Unavailable.
	ErrClosed = errors.Unavailable.New("workerpool: closed")
	// errShutdownTimeout is the cancellation cause of tasks cut off by
	// Shutdown's deadline.
	errShutdownTimeout = errors.Timeout.New("workerpool: shutdown deadline exceeded")
)

// Option configures [New].
type Option func(*Pool)

// WithWorkers sets the number of worker goroutines. The default is 10.
func WithWorkers(n int) Option {
	return func(p *Pool) { p.workers = max(n, 1) }
}

// WithQueueSize sets how many tasks may wait for a worker. The default is
// 100. Zero means tasks are handed directly to idle workers.
func WithQueueSize(n int) Option {
	return func(p *Pool) { p.queueSize = max(n, 0) }
}

// WithOnError calls fn with the error of every failed task, including
// recovered panics. It runs on the worker goroutine and should be fast. The
// default logs the error.
func WithOnError(fn func(ctx context.Context, err error)) Option {
	return func(p *Pool) { p.onError = fn }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(p *Pool) { p.logger = l }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(p *Pool) { p.meterProvider = mp }
}

// Pool is a bounded worker pool. It is safe for concurrent use.
type Pool struct {
	name          string
	workers       int
	queueSize     int
	onError       func(context.Context, error)
	logger        *slog.Logger
	meterProvider metric.MeterProvider

	queue chan item
	ctx   context.Context // cancelled when Shutdown's deadline expires
	abort context.CancelCauseFunc

	mu         sync.Mutex
	closed     bool
	closing    chan struct{} // closed when Shutdown starts
	submitters sync.WaitGroup
	drain      chan struct{} // closed when no more submits can enqueue
	workersWG  sync.WaitGroup
	done       chan struct{} // closed when all workers exited
	aborted    atomic.Bool
	dropped    atomic.Int64
	active     atomic.Int64

	tasks     metric.Int64Counter
	duration  metric.Float64Histogram
	queueWait metric.Float64Histogram
	outcomes  map[string]metric.AddOption
	poolAttrs metric.MeasurementOption
	gaugeReg  metric.Registration
}

type item struct {
	ctx      context.Context
	task     Task
	enqueued time.Time
}

// New starts a pool. name identifies it in logs and metrics; use a fixed,
// low-cardinality value.
func New(name string, opts ...Option) *Pool {
	p := &Pool{
		name:          name,
		workers:       10,
		queueSize:     100,
		logger:        slog.Default(),
		meterProvider: otel.GetMeterProvider(),
		closing:       make(chan struct{}),
		drain:         make(chan struct{}),
		done:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.onError == nil {
		p.onError = func(ctx context.Context, err error) {
			p.logger.ErrorContext(ctx, "worker pool task failed", "pool", p.name, logging.Err(err))
		}
	}
	p.queue = make(chan item, p.queueSize)
	p.ctx, p.abort = context.WithCancelCause(context.Background())
	p.initMetrics()

	p.workersWG.Add(p.workers)
	for range p.workers {
		go p.worker()
	}
	go func() {
		p.workersWG.Wait()
		close(p.done)
	}()
	return p
}

func (p *Pool) initMetrics() {
	meter := p.meterProvider.Meter("github.com/Arif9878/common/go/concurrency/workerpool")
	poolAttr := attribute.String("pool", p.name)
	p.poolAttrs = metric.WithAttributeSet(attribute.NewSet(poolAttr))

	p.tasks, _ = meter.Int64Counter("workerpool.tasks",
		metric.WithDescription("Tasks by outcome: success, error, panic, rejected or dropped."))
	p.duration, _ = meter.Float64Histogram("workerpool.task.duration", metric.WithUnit("s"),
		metric.WithDescription("Task run time."))
	p.queueWait, _ = meter.Float64Histogram("workerpool.queue.wait", metric.WithUnit("s"),
		metric.WithDescription("Time tasks spent queued before a worker started them."))
	p.outcomes = make(map[string]metric.AddOption, 5)
	for _, o := range []string{"success", "error", "panic", "rejected", "dropped"} {
		p.outcomes[o] = metric.WithAttributeSet(attribute.NewSet(poolAttr, attribute.String("outcome", o)))
	}

	active, _ := meter.Int64ObservableGauge("workerpool.workers.active",
		metric.WithDescription("Workers currently running a task."))
	workers, _ := meter.Int64ObservableGauge("workerpool.workers",
		metric.WithDescription("Configured number of workers."))
	depth, _ := meter.Int64ObservableGauge("workerpool.queue.depth",
		metric.WithDescription("Tasks waiting in the queue."))
	capacity, _ := meter.Int64ObservableGauge("workerpool.queue.capacity",
		metric.WithDescription("Queue capacity."))
	p.gaugeReg, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(active, p.active.Load(), p.poolAttrs)
		o.ObserveInt64(workers, int64(p.workers), p.poolAttrs)
		o.ObserveInt64(depth, int64(len(p.queue)), p.poolAttrs)
		o.ObserveInt64(capacity, int64(p.queueSize), p.poolAttrs)
		return nil
	}, active, workers, depth, capacity)
}

// Submit queues task, blocking while the queue is full. It returns ctx's
// error if ctx ends first and [ErrClosed] if the pool is shutting down.
func (p *Pool) Submit(ctx context.Context, task Task) error {
	if err := p.enter(); err != nil {
		return err
	}
	defer p.submitters.Done()

	it := item{ctx: ctx, task: task, enqueued: time.Now()}
	select {
	case p.queue <- it:
		return nil
	case <-ctx.Done():
		p.tasks.Add(ctx, 1, p.outcomes["rejected"])
		return ctx.Err()
	case <-p.closing:
		p.tasks.Add(ctx, 1, p.outcomes["rejected"])
		return ErrClosed
	}
}

// TrySubmit queues task if there is room and returns [ErrQueueFull]
// otherwise. It never blocks.
func (p *Pool) TrySubmit(ctx context.Context, task Task) error {
	if err := p.enter(); err != nil {
		return err
	}
	defer p.submitters.Done()

	select {
	case p.queue <- item{ctx: ctx, task: task, enqueued: time.Now()}:
		return nil
	default:
		p.tasks.Add(ctx, 1, p.outcomes["rejected"])
		return ErrQueueFull
	}
}

// enter registers a submitter, so Shutdown can wait for in-progress
// submits before workers drain the queue.
func (p *Pool) enter() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.tasks.Add(context.Background(), 1, p.outcomes["rejected"])
		return ErrClosed
	}
	p.submitters.Add(1)
	return nil
}

func (p *Pool) worker() {
	defer p.workersWG.Done()
	for {
		select {
		case it := <-p.queue:
			p.run(it)
		case <-p.drain:
			for {
				select {
				case it := <-p.queue:
					p.run(it)
				default:
					return
				}
			}
		}
	}
}

func (p *Pool) run(it item) {
	if p.aborted.Load() {
		p.dropped.Add(1)
		p.tasks.Add(context.Background(), 1, p.outcomes["dropped"])
		return
	}

	// Values from the submitter, cancellation from the pool.
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(it.ctx))
	stop := context.AfterFunc(p.ctx, func() { cancel(context.Cause(p.ctx)) })
	defer func() {
		stop()
		cancel(nil)
	}()

	start := time.Now()
	p.queueWait.Record(ctx, start.Sub(it.enqueued).Seconds(), p.poolAttrs)
	p.active.Add(1)
	err, panicked := runTask(ctx, it.task)
	p.active.Add(-1)
	p.duration.Record(ctx, time.Since(start).Seconds(), p.poolAttrs)

	switch {
	case panicked:
		p.tasks.Add(ctx, 1, p.outcomes["panic"])
		p.onError(ctx, err)
	case err != nil:
		p.tasks.Add(ctx, 1, p.outcomes["error"])
		p.onError(ctx, err)
	default:
		p.tasks.Add(ctx, 1, p.outcomes["success"])
	}
}

func runTask(ctx context.Context, task Task) (err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			err, panicked = errors.Internal.Errorf("workerpool: task panicked: %v", r), true
		}
	}()
	return task(ctx), false
}

// Shutdown stops intake and waits for queued and running tasks; see the
// package documentation. Its signature matches graceful.Hook. Calling it
// again waits for the same shutdown.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	first := !p.closed
	p.closed = true
	p.mu.Unlock()

	if first {
		close(p.closing)
		go func() {
			// Submitters blocked on a full queue return once closing is
			// closed, so this wait is short. After it, nothing else can
			// enqueue and workers may drain and exit.
			p.submitters.Wait()
			close(p.drain)
		}()
	}

	select {
	case <-p.done:
		p.unregister()
		return nil
	case <-ctx.Done():
		p.aborted.Store(true)
		p.abort(errShutdownTimeout)
		// Count what is still queued; workers discard it from now on.
		dropped := p.dropped.Load() + int64(len(p.queue))
		return errors.Timeout.Errorf("workerpool %s: shutdown deadline exceeded with %d running and about %d queued tasks: %w",
			p.name, p.active.Load(), dropped, ctx.Err())
	}
}

func (p *Pool) unregister() {
	if p.gaugeReg != nil {
		_ = p.gaugeReg.Unregister()
	}
}

// String returns a description for debugging.
func (p *Pool) String() string {
	return fmt.Sprintf("workerpool %s (%d workers, queue %d/%d)", p.name, p.workers, len(p.queue), p.queueSize)
}
