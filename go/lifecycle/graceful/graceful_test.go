package graceful_test

import (
	"context"
	stderrors "errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lifecycle/graceful"
)

var quiet = graceful.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

func newManager(opts ...graceful.Option) *graceful.Manager {
	return graceful.New(append([]graceful.Option{quiet, graceful.WithSignals()}, opts...)...)
}

func mustRegister(t *testing.T, m *graceful.Manager, p graceful.Phase, name string, h graceful.Hook) {
	t.Helper()
	if err := m.Register(p, name, h); err != nil {
		t.Fatalf("Register(%v, %s): %v", p, name, err)
	}
}

func TestPhaseOrder(t *testing.T) {
	m := newManager()
	var mu sync.Mutex
	var order []string
	record := func(name string) graceful.Hook {
		return func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}
	}
	// Register out of order on purpose.
	mustRegister(t, m, graceful.Telemetry, "tracing", record("telemetry"))
	mustRegister(t, m, graceful.CloseDeps, "postgres", record("close_deps"))
	mustRegister(t, m, graceful.StopIntake, "http", record("stop_intake"))
	mustRegister(t, m, graceful.Drain, "workers", record("drain"))
	mustRegister(t, m, graceful.Unready, "health", record("unready"))

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := "unready stop_intake drain close_deps telemetry"
	if got := strings.Join(order, " "); got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

func TestHooksInPhaseRunConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager()
		var wg sync.WaitGroup
		wg.Add(2)
		barrier := func(context.Context) error {
			wg.Done()
			wg.Wait() // deadlocks if the hooks run one after another
			return nil
		}
		mustRegister(t, m, graceful.CloseDeps, "postgres", barrier)
		mustRegister(t, m, graceful.CloseDeps, "redis", barrier)
		if err := m.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	})
}

func TestErrorsAreAggregated(t *testing.T) {
	m := newManager()
	errDB := stderrors.New("db close failed")
	var telemetryRan atomic.Bool

	mustRegister(t, m, graceful.StopIntake, "http", func(context.Context) error { panic("boom") })
	mustRegister(t, m, graceful.CloseDeps, "postgres", func(context.Context) error { return errDB })
	mustRegister(t, m, graceful.Telemetry, "tracing", func(context.Context) error {
		telemetryRan.Store(true)
		return nil
	})

	err := m.Shutdown(context.Background())
	if !errors.Is(err, errDB) {
		t.Errorf("error does not wrap hook error: %v", err)
	}
	for _, want := range []string{"stop_intake http: panic: boom", "close_deps postgres: db close failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if !telemetryRan.Load() {
		t.Error("later phase skipped after a hook error")
	}
}

func TestTimeoutSkipsRemainingPhases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(graceful.WithTimeout(10 * time.Second))
		release := make(chan struct{})
		defer close(release)

		var fastDone, telemetryRan atomic.Bool
		mustRegister(t, m, graceful.Drain, "stuck-consumer", func(context.Context) error {
			<-release // ignores ctx
			return nil
		})
		mustRegister(t, m, graceful.Drain, "fast-pool", func(context.Context) error {
			fastDone.Store(true)
			return nil
		})
		mustRegister(t, m, graceful.Telemetry, "tracing", func(context.Context) error {
			telemetryRan.Store(true)
			return nil
		})

		start := time.Now()
		err := m.Shutdown(context.Background())
		if elapsed := time.Since(start); elapsed != 10*time.Second {
			t.Errorf("shutdown took %v, want exactly the 10s timeout", elapsed)
		}
		if errors.KindOf(err) != errors.Timeout {
			t.Errorf("kind = %v, want timeout", errors.KindOf(err))
		}
		msg := err.Error()
		if !strings.Contains(msg, "drain: hooks did not finish before the shutdown deadline: stuck-consumer") {
			t.Errorf("error does not name the stuck hook: %v", msg)
		}
		if strings.Contains(msg, "fast-pool") {
			t.Errorf("finished hook reported as stuck: %v", msg)
		}
		if !strings.Contains(msg, "phases skipped after the shutdown deadline: telemetry") {
			t.Errorf("error does not name skipped phases: %v", msg)
		}
		if !fastDone.Load() || telemetryRan.Load() {
			t.Errorf("fast hook ran = %v, telemetry ran = %v", fastDone.Load(), telemetryRan.Load())
		}
	})
}

func TestHooksReceiveDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(graceful.WithTimeout(5 * time.Second))
		mustRegister(t, m, graceful.Drain, "pool", func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 5*time.Second {
				t.Errorf("deadline in %v, ok=%v", time.Until(deadline), ok)
			}
			return nil
		})
		_ = m.Shutdown(context.Background())
	})
}

func TestUnreadyDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(graceful.WithUnreadyDelay(5 * time.Second))
		var unreadyAt time.Time
		mustRegister(t, m, graceful.Unready, "health", func(context.Context) error {
			unreadyAt = time.Now()
			return nil
		})
		mustRegister(t, m, graceful.StopIntake, "http", func(context.Context) error {
			if d := time.Since(unreadyAt); d != 5*time.Second {
				t.Errorf("intake stopped %v after unready, want 5s", d)
			}
			return nil
		})
		if err := m.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRegister(t *testing.T) {
	m := newManager()
	noop := func(context.Context) error { return nil }
	if err := m.Register(graceful.Phase(99), "x", noop); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("invalid phase: %v", err)
	}
	if err := m.Register(graceful.Drain, "x", nil); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("nil hook: %v", err)
	}
	_ = m.Shutdown(context.Background())
	if err := m.Register(graceful.Drain, "late", noop); !errors.Is(err, graceful.ErrShutdownStarted) {
		t.Errorf("after shutdown: %v", err)
	}
}

func TestShutdownRunsOnce(t *testing.T) {
	m := newManager()
	var runs atomic.Int32
	errHook := stderrors.New("x")
	mustRegister(t, m, graceful.Drain, "pool", func(context.Context) error {
		runs.Add(1)
		return errHook
	})

	var wg sync.WaitGroup
	results := make([]error, 10)
	for i := range results {
		wg.Go(func() { results[i] = m.Shutdown(context.Background()) })
	}
	wg.Wait()

	if runs.Load() != 1 {
		t.Fatalf("hook ran %d times", runs.Load())
	}
	for _, err := range results {
		if !errors.Is(err, errHook) || err.Error() != results[0].Error() {
			t.Fatalf("results differ: %v vs %v", err, results[0])
		}
	}
}

func TestShutdownCallerContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newManager(graceful.WithTimeout(time.Minute))
		release := make(chan struct{})
		mustRegister(t, m, graceful.Drain, "slow", func(context.Context) error {
			<-release
			return nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); errors.KindOf(err) != errors.Timeout {
			t.Errorf("Shutdown with expired caller ctx = %v", err)
		}
		close(release)
		if err := m.Shutdown(context.Background()); err != nil {
			t.Errorf("second Shutdown = %v", err)
		}
	})
}

func TestGoFailureTriggersShutdown(t *testing.T) {
	m := newManager()
	var stopped atomic.Bool
	mustRegister(t, m, graceful.StopIntake, "http", func(context.Context) error {
		stopped.Store(true)
		return nil
	})

	errBind := stderrors.New("bind: address already in use")
	m.Go("http", func() error { return errBind })
	// A component that exits during shutdown does not add a second cause.
	m.Go("grpc", func() error {
		<-m.Stopping()
		return stderrors.New("server closed")
	})

	err := m.Wait(context.Background())
	if !errors.Is(err, errBind) || !strings.Contains(err.Error(), "http: bind") {
		t.Fatalf("Wait = %v, want the component error", err)
	}
	if strings.Contains(err.Error(), "server closed") {
		t.Errorf("error after shutdown started was recorded: %v", err)
	}
	if !stopped.Load() {
		t.Error("hooks did not run")
	}
}

func TestGoUnexpectedExit(t *testing.T) {
	m := newManager()
	m.Go("consumer", func() error { return nil })
	err := m.Wait(context.Background())
	if errors.KindOf(err) != errors.Internal || !strings.Contains(err.Error(), "consumer exited unexpectedly") {
		t.Fatalf("Wait = %v", err)
	}
}

func TestWaitContextCanceled(t *testing.T) {
	m := newManager()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Wait(ctx); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	select {
	case <-m.Stopping():
	default:
		t.Fatal("Stopping not closed")
	}
}

func TestPhaseString(t *testing.T) {
	if graceful.StopIntake.String() != "stop_intake" || graceful.Phase(42).String() != "Phase(42)" {
		t.Fatal("unexpected phase names")
	}
}
