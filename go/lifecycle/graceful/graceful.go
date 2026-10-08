// Package graceful coordinates ordered, bounded shutdown of a service.
//
// Components register stop hooks in a [Phase]. On SIGTERM or SIGINT, on a
// call to [Manager.Shutdown], or when a component started with [Manager.Go]
// exits, the manager runs the phases in order:
//
//	Unready     mark the service unready (health.Checker.Drain)
//	            ... optional delay so load balancers stop routing ...
//	StopIntake  stop accepting work: HTTP/gRPC servers, Kafka polling
//	Drain       finish in-flight work: worker pools, batch processors, producers
//	CloseDeps   close dependencies: database and Redis pools, Vault renewal
//	Telemetry   flush and stop tracing and metrics providers
//
// Telemetry runs last so that logs and spans produced while closing
// dependencies are still exported.
//
// Hooks within one phase run concurrently; put hooks that depend on each
// other in different phases. Every hook receives the same context, which
// expires after the shutdown timeout (30s by default). Hooks must return
// when it is done.
//
// # Failure behavior
//
//   - A hook error is recorded and the remaining hooks and phases still run.
//   - When the timeout expires, the manager stops waiting for hooks that have
//     not returned, records a timeout error naming them, and skips the
//     remaining phases, naming them in the error. Shutdown therefore takes at
//     most about the timeout, even if a hook hangs. Size the timeout so the
//     Telemetry phase is normally reached.
//   - A second signal during shutdown cancels the shutdown context, with the
//     same effect as the timeout expiring.
//
// Shutdown runs exactly once. [Manager.Wait] and [Manager.Shutdown] return
// the same aggregated error: the cause of the shutdown (a failed
// component), hook errors and timeouts, joined. A shutdown requested by a
// signal or Shutdown with no failures returns nil.
package graceful

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
)

// Phase is a step of the shutdown sequence. Phases run in declaration order.
type Phase int

// Shutdown phases, in execution order.
const (
	Unready Phase = iota
	StopIntake
	Drain
	CloseDeps
	Telemetry

	numPhases
)

var phaseNames = [numPhases]string{"unready", "stop_intake", "drain", "close_deps", "telemetry"}

// String returns the phase name, such as "stop_intake".
func (p Phase) String() string {
	if p < 0 || p >= numPhases {
		return fmt.Sprintf("Phase(%d)", int(p))
	}
	return phaseNames[p]
}

// Hook stops one component. It must return when ctx is done.
type Hook func(ctx context.Context) error

// ErrShutdownStarted is returned by [Manager.Register] after shutdown began.
var ErrShutdownStarted = errors.Unavailable.New("graceful: shutdown already started")

// Option configures [New].
type Option func(*Manager)

// WithTimeout sets the total time allowed for all phases. The default is
// 30 seconds; keep it below the orchestrator's kill deadline (Kubernetes
// terminationGracePeriodSeconds, 30s by default) minus WithUnreadyDelay.
func WithTimeout(d time.Duration) Option {
	return func(m *Manager) { m.timeout = d }
}

// WithUnreadyDelay waits d after the Unready phase before stopping intake,
// so load balancers and Kubernetes endpoints observe the failed readiness
// probe and stop sending new requests. The default is 0; behind a load
// balancer, set it to a little more than the readiness probe period. The
// delay counts toward the timeout.
func WithUnreadyDelay(d time.Duration) Option {
	return func(m *Manager) { m.unreadyDelay = d }
}

// WithSignals sets the signals that start shutdown. The default is SIGTERM
// and SIGINT. Passing none disables signal handling.
func WithSignals(sigs ...os.Signal) Option {
	return func(m *Manager) { m.signals = sigs }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) { m.logger = l }
}

// Manager runs the shutdown sequence. It is safe for concurrent use.
type Manager struct {
	timeout      time.Duration
	unreadyDelay time.Duration
	signals      []os.Signal
	logger       *slog.Logger

	mu      sync.Mutex
	hooks   [numPhases][]namedHook
	started bool
	cause   error
	cancel  context.CancelFunc

	stopping chan struct{} // closed when shutdown starts
	done     chan struct{} // closed when shutdown finished
	result   error
}

type namedHook struct {
	name string
	fn   Hook
}

// New returns a Manager. Signal handling starts when [Manager.Wait] is
// called.
func New(opts ...Option) *Manager {
	m := &Manager{
		timeout:  30 * time.Second,
		signals:  []os.Signal{syscall.SIGTERM, os.Interrupt},
		logger:   slog.Default(),
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Register adds a hook named name to phase. It returns an error if phase is
// invalid or shutdown has already started.
func (m *Manager) Register(phase Phase, name string, hook Hook) error {
	if phase < 0 || phase >= numPhases {
		return errors.InvalidArgument.Errorf("graceful: invalid phase %d", phase)
	}
	if hook == nil {
		return errors.InvalidArgument.New("graceful: nil hook")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return ErrShutdownStarted
	}
	m.hooks[phase] = append(m.hooks[phase], namedHook{name: name, fn: hook})
	return nil
}

// Go runs fn, typically a server's blocking serve loop, in a goroutine. If
// fn returns before shutdown has started, the service cannot work as
// intended, so shutdown starts and fn's error (or a note that it exited)
// becomes the shutdown cause. Results after shutdown has started are
// ignored; fn should treat its normal close error (http.ErrServerClosed) as
// nil.
func (m *Manager) Go(name string, fn func() error) {
	go func() {
		err := fn()
		select {
		case <-m.stopping:
			return
		default:
		}
		if err == nil {
			err = errors.Internal.Errorf("%s exited unexpectedly", name)
		} else {
			err = fmt.Errorf("%s: %w", name, err)
		}
		m.start(err)
	}()
}

// Stopping returns a channel that is closed when shutdown starts.
func (m *Manager) Stopping() <-chan struct{} { return m.stopping }

// Wait blocks until shutdown is triggered (by a signal, ctx being done, a
// failed component or Shutdown) and has finished, and returns its result.
// It also handles signals during shutdown: a second signal cancels the
// shutdown context.
func (m *Manager) Wait(ctx context.Context) error {
	var sigCh chan os.Signal
	if len(m.signals) > 0 {
		sigCh = make(chan os.Signal, 2)
		signal.Notify(sigCh, m.signals...)
		defer signal.Stop(sigCh)
	}

	select {
	case sig := <-sigCh:
		m.logger.Info("shutdown signal received", "signal", sig.String())
		m.start(nil)
	case <-ctx.Done():
		m.start(nil)
	case <-m.stopping:
	}

	for {
		select {
		case <-m.done:
			return m.result
		case sig := <-sigCh:
			m.logger.Warn("second signal received, aborting shutdown", "signal", sig.String())
			m.mu.Lock()
			m.cancel()
			m.mu.Unlock()
		}
	}
}

// Shutdown starts shutdown if it has not started and waits until it
// finishes or ctx is done. Every call returns the same result.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.start(nil)
	select {
	case <-m.done:
		return m.result
	case <-ctx.Done():
		return errors.Timeout.Wrap(ctx.Err(), "graceful: waiting for shutdown")
	}
}

// start begins shutdown once. cause is the reason for an unplanned
// shutdown, or nil.
func (m *Manager) start(cause error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return
	}
	m.started = true
	m.cause = cause

	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	m.cancel = cancel
	hooks := m.hooks
	close(m.stopping)

	if cause != nil {
		m.logger.Error("shutting down after component failure", logging.Err(cause))
	} else {
		m.logger.Info("shutting down")
	}
	go m.run(ctx, cancel, hooks, cause)
}

func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, hooks [numPhases][]namedHook, cause error) {
	defer cancel()
	start := time.Now()
	errs := []error{cause}

	for phase := range numPhases {
		if ctx.Err() != nil {
			var skipped []string
			for p := phase; p < numPhases; p++ {
				if len(hooks[p]) > 0 {
					skipped = append(skipped, p.String())
				}
			}
			if len(skipped) > 0 {
				errs = append(errs, errors.Timeout.Errorf("phases skipped after the shutdown deadline: %s",
					strings.Join(skipped, ", ")))
			}
			break
		}
		errs = append(errs, m.runPhase(ctx, phase, hooks[phase])...)
		if phase == Unready && m.unreadyDelay > 0 {
			timer := time.NewTimer(m.unreadyDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
		}
	}

	m.result = errors.Join(errs...)
	if m.result != nil && cause == nil {
		m.logger.Error("shutdown finished with errors", logging.Duration(time.Since(start)), logging.Err(m.result))
	} else {
		m.logger.Info("shutdown finished", logging.Duration(time.Since(start)))
	}
	close(m.done)
}

type hookResult struct {
	name string
	err  error
}

func (m *Manager) runPhase(ctx context.Context, phase Phase, hooks []namedHook) []error {
	if len(hooks) == 0 {
		return nil
	}
	results := make(chan hookResult, len(hooks)) // buffered: abandoned hooks never block
	for _, h := range hooks {
		go func() {
			hookStart := time.Now()
			err := runHook(ctx, h.fn)
			if err != nil {
				err = fmt.Errorf("%s %s: %w", phase, h.name, err)
				m.logger.Error("shutdown hook failed", "phase", phase.String(), "hook", h.name,
					logging.Duration(time.Since(hookStart)), logging.Err(err))
			} else {
				m.logger.Debug("shutdown hook finished", "phase", phase.String(), "hook", h.name,
					logging.Duration(time.Since(hookStart)))
			}
			results <- hookResult{name: h.name, err: err}
		}()
	}

	var errs []error
	pending := make(map[string]int, len(hooks))
	for _, h := range hooks {
		pending[h.name]++
	}
	for range hooks {
		select {
		case r := <-results:
			if pending[r.name]--; pending[r.name] == 0 {
				delete(pending, r.name)
			}
			if r.err != nil {
				errs = append(errs, r.err)
			}
		case <-ctx.Done():
			names := slices.Sorted(func(yield func(string) bool) {
				for name := range pending {
					if !yield(name) {
						return
					}
				}
			})
			err := errors.Timeout.Errorf("%s: hooks did not finish before the shutdown deadline: %s",
				phase, strings.Join(names, ", "))
			m.logger.Error("shutdown phase abandoned", "phase", phase.String(), logging.Err(err))
			return append(errs, err)
		}
	}
	return errs
}

func runHook(ctx context.Context, fn Hook) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.Internal.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx)
}
