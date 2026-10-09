// Package schedule runs periodic jobs, on one replica at a time when given
// a lock.Locker:
//
//	s := schedule.New(schedule.WithLocker(redislock.New(rdb)))
//	_ = s.Cron("reports.daily", "0 2 * * *", buildDailyReport)        // 02:00 every day
//	_ = s.Every("outbox.cleanup", 10*time.Minute, cleanUp, schedule.WithTimeout(time.Minute))
//	shutdown.Go("scheduler", func() error { return s.Run(ctx) })
//	shutdown.Register(graceful.StopIntake, "scheduler", s.Stop)
//
// # Timing and replicas
//
// Runs happen at wall-clock times every replica computes alike: cron
// expressions (standard five fields, or @hourly, @daily, …) in the
// scheduler's location, and Every intervals aligned to multiples of the
// interval since the Unix epoch (every 10 minutes means :00, :10, :20).
//
// With WithLocker, the replica that fires first takes a lease on the job
// for the rest of its slot, until shortly before the next run, and the
// others skip that run, so each run happens once. Without a locker every
// replica runs every job. The lease is a strong hint, not a guarantee (see
// the lock package): make jobs idempotent, and use the lease's fencing
// token where duplicate work would be harmful. A replica whose clock is
// ahead of the store's by more than the slot margin (2% of the interval,
// at most 5s) can find the previous lease still held and skip a run.
//
// # Failures and limits
//
// A run's context ends after the job's timeout (by default its interval).
// Errors and panics are logged and counted; the job runs again at its next
// time. A run still going when the next is due on the same replica makes
// that replica skip it, so a job never overlaps itself in one process; a
// job running longer than its interval can overlap a run on another
// replica, so keep timeouts below the interval.
package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lock"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/tracing"
)

// Job is the work of one run. It must return when ctx ends.
type Job func(ctx context.Context) error

// Option configures [New].
type Option func(*Scheduler)

// WithLocker runs each job on one replica at a time, through leases on
// l. Without it, every replica runs every job.
func WithLocker(l lock.Locker) Option { return func(s *Scheduler) { s.locker = l } }

// WithLocation interprets cron expressions in loc. The default is UTC.
func WithLocation(loc *time.Location) Option { return func(s *Scheduler) { s.loc = loc } }

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(s *Scheduler) { s.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(s *Scheduler) { s.meterProv = mp }
}

// WithTracerProvider sets the tracer provider. The default is the global
// one.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(s *Scheduler) { s.tracerProv = tp }
}

// JobOption configures one job.
type JobOption func(*job)

// WithTimeout bounds each run of the job. The default is the job's
// interval (for cron jobs, the time between its first two runs).
func WithTimeout(d time.Duration) JobOption { return func(j *job) { j.timeout = d } }

type job struct {
	name    string
	next    func(time.Time) time.Time
	fn      Job
	timeout time.Duration

	running sync.Mutex // held during a run on this replica
}

// Scheduler runs jobs. Register jobs before Run.
type Scheduler struct {
	locker     lock.Locker
	loc        *time.Location
	logger     *slog.Logger
	meterProv  metric.MeterProvider
	tracerProv trace.TracerProvider

	mu      sync.Mutex
	jobs    []*job
	started bool

	stop     chan struct{}
	stopOnce sync.Once
	runCtx   context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	done     chan struct{}

	runs     metric.Int64Counter
	duration metric.Float64Histogram
}

// New returns a Scheduler.
func New(opts ...Option) *Scheduler {
	s := &Scheduler{loc: time.UTC, logger: slog.Default(), meterProv: otel.GetMeterProvider(),
		tracerProv: otel.GetTracerProvider(), stop: make(chan struct{}), done: make(chan struct{})}
	for _, opt := range opts {
		opt(s)
	}
	s.runCtx, s.cancel = context.WithCancel(context.Background())
	meter := s.meterProv.Meter("github.com/Arif9878/common/go/concurrency/schedule")
	s.runs, _ = meter.Int64Counter("schedule.runs",
		metric.WithDescription("Scheduled job runs, by job and outcome: success, failed, panicked, skipped (another replica or a run still going)."))
	s.duration, _ = meter.Float64Histogram("schedule.run.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of scheduled job runs."))
	return s
}

// Every runs fn every interval, at multiples of interval since the Unix
// epoch. name identifies the job in leases, logs and metrics.
func (s *Scheduler) Every(name string, interval time.Duration, fn Job, opts ...JobOption) error {
	if interval < time.Second {
		return errors.InvalidArgument.Errorf("schedule: job %s: interval %v under a second", name, interval)
	}
	return s.add(name, func(t time.Time) time.Time { return t.Truncate(interval).Add(interval) }, fn, opts)
}

// Cron runs fn on a standard cron schedule ("0 2 * * *", "*/15 * * * *",
// "@hourly") in the scheduler's location.
func (s *Scheduler) Cron(name, spec string, fn Job, opts ...JobOption) error {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return errors.InvalidArgument.Wrap(err, "schedule: job "+name)
	}
	loc := s.loc
	return s.add(name, func(t time.Time) time.Time { return sched.Next(t.In(loc)) }, fn, opts)
}

func (s *Scheduler) add(name string, next func(time.Time) time.Time, fn Job, opts []JobOption) error {
	if name == "" || fn == nil {
		return errors.InvalidArgument.New("schedule: a job needs a name and a function")
	}
	j := &job{name: name, next: next, fn: fn}
	for _, opt := range opts {
		opt(j)
	}
	if j.timeout <= 0 {
		first := next(time.Now())
		j.timeout = next(first).Sub(first)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.InvalidArgument.New("schedule: add jobs before Run")
	}
	for _, other := range s.jobs {
		if other.name == name {
			return errors.InvalidArgument.Errorf("schedule: job %s added twice", name)
		}
	}
	s.jobs = append(s.jobs, j)
	return nil
}

// Run schedules the jobs until ctx ends or Stop is called, then waits for
// runs in progress and returns nil. Run it once.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.InvalidArgument.New("schedule: Run called twice")
	}
	s.started = true
	jobs := s.jobs
	s.mu.Unlock()
	defer close(s.done)

	for _, j := range jobs {
		s.wg.Add(1)
		go s.loop(ctx, j)
	}
	select {
	case <-ctx.Done():
	case <-s.stop:
	}
	s.wg.Wait()
	return nil
}

// Stop stops scheduling and waits for runs in progress. If ctx ends first,
// their contexts are cancelled. Its signature matches graceful.Hook.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		s.cancel()
		<-s.done
		return ctx.Err()
	}
}

func (s *Scheduler) loop(ctx context.Context, j *job) {
	defer s.wg.Done()
	for {
		now := time.Now()
		at := j.next(now)
		timer := time.NewTimer(at.Sub(now))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.stop:
			timer.Stop()
			return
		}
		if !j.running.TryLock() {
			s.record(j, "skipped", 0)
			s.logger.Warn("scheduled job still running; skipping a run", "job", j.name)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer j.running.Unlock()
			s.run(j, at)
		}()
	}
}

// run takes the job's lease, if there is a locker, and runs it.
func (s *Scheduler) run(j *job, at time.Time) {
	ctx, cancel := context.WithTimeout(s.runCtx, j.timeout)
	defer cancel()
	if s.locker != nil {
		next := j.next(at)
		slot := next.Sub(at)
		margin := min(slot/50, 5*time.Second)
		ttl := time.Until(next) - margin
		if ttl < time.Second {
			ttl = time.Second
		}
		// Held for the rest of the slot, not released after the run, so a
		// replica firing a little later skips this run.
		if _, err := s.locker.TryAcquire(ctx, "schedule:"+j.name, ttl); err != nil {
			if !errors.Is(err, lock.ErrNotAcquired) {
				s.logger.Warn("scheduled job: lease failed; skipping a run", "job", j.name, logging.Err(err))
			}
			s.record(j, "skipped", 0)
			return
		}
	}

	ctx = logging.ContextWithAttrs(ctx, slog.String("job", j.name))
	ctx, span := s.tracerProv.Tracer("github.com/Arif9878/common/go/concurrency/schedule").Start(ctx,
		"schedule "+j.name, trace.WithAttributes(attribute.String("job.name", j.name)))
	start := time.Now()
	err := call(ctx, j.fn)
	tracing.End(span, &err)
	elapsed := time.Since(start)

	switch {
	case err == nil:
		s.record(j, "success", elapsed)
	case errors.Is(err, errPanic):
		s.record(j, "panicked", elapsed)
		s.logger.ErrorContext(ctx, "scheduled job panicked", logging.Err(err))
	default:
		s.record(j, "failed", elapsed)
		s.logger.ErrorContext(ctx, "scheduled job failed", logging.Err(err), slog.Duration("duration", elapsed))
	}
}

var errPanic = errors.Internal.New("schedule: job panicked")

func call(ctx context.Context, fn Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Join(errPanic, fmt.Errorf("%v\n%s", r, debug.Stack()))
		}
	}()
	return fn(ctx)
}

func (s *Scheduler) record(j *job, outcome string, d time.Duration) {
	attrs := metric.WithAttributes(attribute.String("job.name", j.name), attribute.String("outcome", outcome))
	s.runs.Add(context.Background(), 1, attrs)
	if outcome != "skipped" {
		s.duration.Record(context.Background(), d.Seconds(), metric.WithAttributes(attribute.String("job.name", j.name)))
	}
}
