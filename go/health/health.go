// Package health implements liveness, readiness and startup probes.
//
//	h := health.New()
//	h.AddReadiness("postgres", db.Ping, health.WithTimeout(time.Second))
//	h.AddReadiness("redis", rdb.Ping, health.NonCritical())
//	mux.Handle("/live", h.LiveHandler())
//	mux.Handle("/ready", h.ReadyHandler())
//	mux.Handle("/startup", h.StartupHandler())
//	...
//	h.MarkStarted() // after migrations, cache warm-up, etc.
//	shutdown.Register(graceful.Unready, "readiness", h.Drain)
//
// # Probe semantics
//
//   - Liveness answers "is this process wedged and should it be restarted?".
//     It must not depend on external services: if the database is down,
//     restarting every replica makes the outage worse. Only register
//     in-process checks with [Checker.AddLiveness]; with none it always
//     succeeds. Draining does not affect liveness.
//   - Readiness answers "should this replica receive traffic?". It fails
//     before [Checker.MarkStarted], after [Checker.Drain], and while any
//     critical readiness check fails. [NonCritical] checks are reported but
//     do not fail readiness; use them for dependencies the service can
//     degrade without.
//   - Startup succeeds once MarkStarted has been called.
//
// # Execution
//
// Checks run in parallel, at most WithMaxConcurrency at a time, each bounded
// by its timeout. Concurrent probes share one run, and a result is reused
// for WithCacheTTL, so frequent probes from many sources cause bounded load
// on dependencies. A check that ignores its context keeps at most one
// invocation running: until it returns, the check is reported as failed
// without being started again. Panics in checks are recovered and reported
// as failures.
//
// # Responses
//
// Handlers respond 200 or 503 with a JSON body listing each check's status
// and, for failures, only its error kind (such as "unavailable"). Error
// details are logged, not returned, because probe endpoints are usually
// unauthenticated.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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

// Check reports whether a component is healthy. It must return when ctx is
// done.
type Check func(ctx context.Context) error

// Status is the outcome of a probe or check.
type Status string

// Statuses.
const (
	StatusOK   Status = "ok"
	StatusFail Status = "fail"
)

// Report is the result of a probe.
type Report struct {
	Status Status `json:"status"`
	// Reason explains a failure not caused by checks: "starting" or
	// "draining".
	Reason string                 `json:"reason,omitempty"`
	Checks map[string]CheckResult `json:"checks,omitempty"`
}

// CheckResult is the result of one check.
type CheckResult struct {
	Status   Status `json:"status"`
	Critical bool   `json:"critical"`
	// ErrorType is the errors.Kind of the failure, such as "timeout".
	ErrorType  string  `json:"error_type,omitempty"`
	DurationMS float64 `json:"duration_ms"`
}

// Option configures [New].
type Option func(*Checker)

// WithMaxConcurrency limits how many checks run at once. The default is 4.
func WithMaxConcurrency(n int) Option {
	return func(c *Checker) { c.maxConcurrency = max(n, 1) }
}

// WithCacheTTL sets how long a probe result is reused. The default is one
// second; 0 disables caching (concurrent probes still share a run).
func WithCacheTTL(d time.Duration) Option {
	return func(c *Checker) { c.cacheTTL = d }
}

// WithLogger sets the logger used to report check status changes. The
// default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *Checker) { c.logger = l }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *Checker) { c.meterProvider = mp }
}

// CheckOption configures a registered check.
type CheckOption func(*check)

// WithTimeout bounds one run of the check. The default is 2 seconds.
func WithTimeout(d time.Duration) CheckOption {
	return func(c *check) { c.timeout = d }
}

// NonCritical marks a readiness check as informational: its failure is
// reported but does not fail readiness.
func NonCritical() CheckOption {
	return func(c *check) { c.critical = false }
}

// Checker holds the registered checks and probe state. It is safe for
// concurrent use.
type Checker struct {
	maxConcurrency int
	cacheTTL       time.Duration
	logger         *slog.Logger
	meterProvider  metric.MeterProvider
	duration       metric.Float64Histogram

	started  atomic.Bool
	draining atomic.Bool

	live  probe
	ready probe
}

type probe struct {
	kind string

	mu       sync.Mutex
	checks   []*check
	last     Report
	lastAt   time.Time
	inflight chan struct{} // closed when the current run finishes
}

type check struct {
	name     string
	fn       Check
	timeout  time.Duration
	critical bool

	running    atomic.Bool
	lastStatus atomic.Value // Status
}

// New returns a Checker with no checks.
func New(opts ...Option) *Checker {
	c := &Checker{
		maxConcurrency: 4,
		cacheTTL:       time.Second,
		logger:         slog.Default(),
		meterProvider:  otel.GetMeterProvider(),
		live:           probe{kind: "live"},
		ready:          probe{kind: "ready"},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.duration, _ = c.meterProvider.Meter("github.com/Arif9878/common/go/health").Float64Histogram(
		"health.check.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of health checks."),
	)
	return c
}

// AddReadiness registers a readiness check. It panics if name is already
// registered for readiness.
func (c *Checker) AddReadiness(name string, fn Check, opts ...CheckOption) {
	c.ready.add(name, fn, opts)
}

// AddLiveness registers a liveness check. Only use in-process checks; see
// the package documentation. It panics if name is already registered for
// liveness.
func (c *Checker) AddLiveness(name string, fn Check, opts ...CheckOption) {
	c.live.add(name, fn, opts)
}

func (p *probe) add(name string, fn Check, opts []CheckOption) {
	if fn == nil {
		panic("health: nil check " + name)
	}
	ch := &check{name: name, fn: fn, timeout: 2 * time.Second, critical: true}
	for _, opt := range opts {
		opt(ch)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if slices.ContainsFunc(p.checks, func(c *check) bool { return c.name == name }) {
		panic(fmt.Sprintf("health: duplicate %s check %q", p.kind, name))
	}
	p.checks = append(p.checks, ch)
	p.lastAt = time.Time{} // invalidate cache
}

// MarkStarted marks startup as complete.
func (c *Checker) MarkStarted() { c.started.Store(true) }

// Drain makes readiness fail from now on. Its signature matches a
// graceful.Hook, for registration in the graceful.Unready phase.
func (c *Checker) Drain(context.Context) error {
	c.draining.Store(true)
	return nil
}

// Live runs the liveness probe.
func (c *Checker) Live(ctx context.Context) Report {
	return c.live.run(ctx, c)
}

// Ready runs the readiness probe.
func (c *Checker) Ready(ctx context.Context) Report {
	switch {
	case !c.started.Load():
		return Report{Status: StatusFail, Reason: "starting"}
	case c.draining.Load():
		return Report{Status: StatusFail, Reason: "draining"}
	}
	return c.ready.run(ctx, c)
}

// Startup runs the startup probe.
func (c *Checker) Startup(context.Context) Report {
	if !c.started.Load() {
		return Report{Status: StatusFail, Reason: "starting"}
	}
	return Report{Status: StatusOK}
}

// LiveHandler serves the liveness probe.
func (c *Checker) LiveHandler() http.Handler { return handler(c.Live) }

// ReadyHandler serves the readiness probe.
func (c *Checker) ReadyHandler() http.Handler { return handler(c.Ready) }

// StartupHandler serves the startup probe.
func (c *Checker) StartupHandler() http.Handler { return handler(c.Startup) }

func handler(probe func(context.Context) Report) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := probe(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if rep.Status != StatusOK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(rep)
	})
}

// run returns a cached report, joins a run in progress, or starts one. The
// run itself is not tied to ctx, so one caller giving up does not fail the
// others; ctx only bounds how long this caller waits.
func (p *probe) run(ctx context.Context, c *Checker) Report {
	p.mu.Lock()
	if !p.lastAt.IsZero() && time.Since(p.lastAt) < c.cacheTTL {
		rep := p.last
		p.mu.Unlock()
		return rep
	}
	inflight := p.inflight
	if inflight == nil {
		inflight = make(chan struct{})
		p.inflight = inflight
		checks := slices.Clone(p.checks)
		go func() {
			rep := c.execute(p.kind, checks)
			p.mu.Lock()
			p.last, p.lastAt, p.inflight = rep, time.Now(), nil
			p.mu.Unlock()
			close(inflight)
		}()
	}
	p.mu.Unlock()

	select {
	case <-inflight:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.last
	case <-ctx.Done():
		return Report{Status: StatusFail, Reason: "probe canceled"}
	}
}

func (c *Checker) execute(kind string, checks []*check) Report {
	rep := Report{Status: StatusOK}
	if len(checks) == 0 {
		return rep
	}
	rep.Checks = make(map[string]CheckResult, len(checks))

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.maxConcurrency)
	for _, ch := range checks {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			res := c.runCheck(kind, ch)
			mu.Lock()
			defer mu.Unlock()
			rep.Checks[ch.name] = res
			if res.Status != StatusOK && res.Critical {
				rep.Status = StatusFail
			}
		})
	}
	wg.Wait()
	return rep
}

var errStillRunning = errors.Timeout.New("previous run has not returned")

func (c *Checker) runCheck(kind string, ch *check) CheckResult {
	start := time.Now()
	var err error
	if !ch.running.CompareAndSwap(false, true) {
		err = errStillRunning
	} else {
		err = ch.call()
	}
	d := time.Since(start)

	res := CheckResult{Status: StatusOK, Critical: ch.critical, DurationMS: float64(d) / float64(time.Millisecond)}
	if err != nil {
		res.Status = StatusFail
		res.ErrorType = errors.KindOf(err).String()
	}

	c.duration.Record(context.Background(), d.Seconds(), metric.WithAttributes(
		attribute.String("probe", kind),
		attribute.String("check", ch.name),
		attribute.String("outcome", string(res.Status)),
	))
	if prev, _ := ch.lastStatus.Swap(res.Status).(Status); prev != res.Status {
		attrs := []any{"probe", kind, "check", ch.name, "critical", ch.critical}
		switch {
		case err != nil:
			c.logger.Warn("health check failing", append(attrs, logging.Err(err))...)
		case prev != "":
			c.logger.Info("health check recovered", attrs...)
		}
	}
	return res
}

// call runs the check with its timeout. If the check ignores its context,
// call returns at the deadline and the check's goroutine keeps ch.running
// set until it finally returns.
func (ch *check) call() error {
	ctx, cancel := context.WithTimeout(context.Background(), ch.timeout)
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			ch.running.Store(false) // before sending, so the next run never sees a stale flag
			done <- err
		}()
		defer func() {
			if r := recover(); r != nil {
				err = errors.Internal.Errorf("health check panicked: %v", r)
			}
		}()
		err = ch.fn(ctx)
	}()

	defer cancel()
	select {
	case err := <-done:
		if err == nil && ctx.Err() != nil {
			// Returned nil only after its deadline: too slow to count as healthy.
			return errors.Timeout.Wrap(ctx.Err(), "check")
		}
		return err
	case <-ctx.Done():
		return errors.Timeout.Wrap(ctx.Err(), "check")
	}
}
