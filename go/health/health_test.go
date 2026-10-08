package health_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Arif9878/common/go/testkit"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
)

// Drain is designed to be registered as a graceful.Hook.
var _ graceful.Hook = (*health.Checker)(nil).Drain

var quiet = health.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

func ok(context.Context) error { return nil }

func serve(t *testing.T, h http.Handler) (int, health.Report, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	var rep health.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, rep, rec.Header()
}

func TestReadinessLifecycle(t *testing.T) {
	h := health.New(quiet, health.WithCacheTTL(0))
	h.AddReadiness("db", ok)

	code, rep, hdr := serve(t, h.ReadyHandler())
	if code != http.StatusServiceUnavailable || rep.Reason != "starting" {
		t.Fatalf("before start: %d %+v", code, rep)
	}
	if hdr.Get("Content-Type") != "application/json" || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", hdr)
	}
	if code, _, _ := serve(t, h.StartupHandler()); code != http.StatusServiceUnavailable {
		t.Errorf("startup before MarkStarted = %d", code)
	}

	h.MarkStarted()
	if code, rep, _ := serve(t, h.ReadyHandler()); code != http.StatusOK || rep.Checks["db"].Status != health.StatusOK {
		t.Fatalf("after start: %d %+v", code, rep)
	}
	if code, _, _ := serve(t, h.StartupHandler()); code != http.StatusOK {
		t.Errorf("startup after MarkStarted = %d", code)
	}

	if err := h.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, rep, _ := serve(t, h.ReadyHandler()); code != http.StatusServiceUnavailable || rep.Reason != "draining" {
		t.Fatalf("draining: %d %+v", code, rep)
	}
	if code, _, _ := serve(t, h.LiveHandler()); code != http.StatusOK {
		t.Errorf("liveness failed while draining: %d", code)
	}
}

func TestLivenessWithoutChecks(t *testing.T) {
	h := health.New(quiet)
	h.AddReadiness("db", func(context.Context) error { return stderrors.New("down") })
	if code, _, _ := serve(t, h.LiveHandler()); code != http.StatusOK {
		t.Fatalf("liveness = %d, must not depend on readiness checks or startup", code)
	}
}

func TestCriticalAndNonCritical(t *testing.T) {
	const secret = "password authentication failed for user svc"
	h := health.New(quiet, health.WithCacheTTL(0))
	h.MarkStarted()

	var dbDown atomic.Bool
	h.AddReadiness("db", func(context.Context) error {
		if dbDown.Load() {
			return errors.Unavailable.Wrap(stderrors.New(secret), "ping")
		}
		return nil
	})
	h.AddReadiness("cache", func(context.Context) error { return stderrors.New(secret) }, health.NonCritical())

	code, rep, _ := serve(t, h.ReadyHandler())
	if code != http.StatusOK {
		t.Fatalf("non-critical failure failed readiness: %d %+v", code, rep)
	}
	if c := rep.Checks["cache"]; c.Status != health.StatusFail || c.Critical || c.ErrorType != "unknown" {
		t.Errorf("cache = %+v", c)
	}

	dbDown.Store(true)
	rec := httptest.NewRecorder()
	h.ReadyHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("critical failure: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("error details leaked in response: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"error_type":"unavailable"`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestCheckTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := health.New(quiet)
		h.MarkStarted()
		h.AddReadiness("slow", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}, health.WithTimeout(500*time.Millisecond))
		h.AddReadiness("late-nil", func(context.Context) error {
			time.Sleep(time.Second)
			return nil
		}, health.WithTimeout(500*time.Millisecond))

		start := time.Now()
		rep := h.Ready(context.Background())
		if d := time.Since(start); d != 500*time.Millisecond {
			t.Errorf("probe took %v, want the 500ms check timeout", d)
		}
		for _, name := range []string{"slow", "late-nil"} {
			if c := rep.Checks[name]; c.Status != health.StatusFail || c.ErrorType != "timeout" {
				t.Errorf("%s = %+v", name, c)
			}
		}
		if rep.Status != health.StatusFail {
			t.Errorf("status = %v", rep.Status)
		}

		// late-nil ignores its context and outlives the probe; let it finish.
		time.Sleep(time.Second)
		synctest.Wait()
	})
}

func TestHungCheckIsNotRestarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := health.New(quiet, health.WithCacheTTL(0))
		h.MarkStarted()
		release := make(chan struct{})
		var calls atomic.Int32
		h.AddReadiness("hung", func(context.Context) error {
			calls.Add(1)
			<-release // ignores ctx
			return nil
		}, health.WithTimeout(time.Second))

		for range 5 {
			if rep := h.Ready(context.Background()); rep.Checks["hung"].ErrorType != "timeout" {
				t.Fatalf("rep = %+v", rep)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("hung check started %d times, want 1", n)
		}

		close(release)
		synctest.Wait()
		if rep := h.Ready(context.Background()); rep.Status != health.StatusOK {
			t.Fatalf("after release: %+v", rep)
		}
		if n := calls.Load(); n != 2 {
			t.Fatalf("calls = %d, want 2", n)
		}
	})
}

func TestBoundedConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := health.New(quiet, health.WithMaxConcurrency(2))
		h.MarkStarted()
		var active, peak atomic.Int32
		for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
			h.AddReadiness(name, func(context.Context) error {
				n := active.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(time.Second)
				active.Add(-1)
				return nil
			})
		}

		start := time.Now()
		if rep := h.Ready(context.Background()); rep.Status != health.StatusOK {
			t.Fatalf("rep = %+v", rep)
		}
		if peak.Load() != 2 {
			t.Errorf("peak concurrency = %d, want 2", peak.Load())
		}
		if d := time.Since(start); d != 3*time.Second {
			t.Errorf("6 checks of 1s at concurrency 2 took %v, want 3s", d)
		}
	})
}

func TestCachingAndSharedRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := health.New(quiet, health.WithCacheTTL(time.Second))
		h.MarkStarted()
		var calls atomic.Int32
		h.AddReadiness("db", func(context.Context) error {
			calls.Add(1)
			time.Sleep(100 * time.Millisecond)
			return nil
		})

		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() { h.Ready(context.Background()) })
		}
		wg.Wait()
		if n := calls.Load(); n != 1 {
			t.Fatalf("20 concurrent probes ran the check %d times", n)
		}

		h.Ready(context.Background()) // cached
		if n := calls.Load(); n != 1 {
			t.Fatalf("cached probe ran the check (calls = %d)", n)
		}
		time.Sleep(time.Second)
		h.Ready(context.Background())
		if n := calls.Load(); n != 2 {
			t.Fatalf("expired cache not refreshed (calls = %d)", n)
		}
	})
}

func TestCallerCancelDoesNotFailOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := health.New(quiet)
		h.MarkStarted()
		h.AddReadiness("db", func(context.Context) error {
			time.Sleep(time.Second)
			return nil
		})

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var other health.Report
		done := make(chan struct{})
		go func() {
			other = h.Ready(context.Background())
			close(done)
		}()

		if rep := h.Ready(ctx); rep.Status != health.StatusFail || rep.Reason != "probe canceled" {
			t.Errorf("canceled caller got %+v", rep)
		}
		<-done
		if other.Status != health.StatusOK {
			t.Errorf("other caller got %+v", other)
		}
	})
}

func TestPanicIsRecovered(t *testing.T) {
	h := health.New(quiet)
	h.AddLiveness("loop", func(context.Context) error { panic("boom") })
	rep := h.Live(context.Background())
	if c := rep.Checks["loop"]; rep.Status != health.StatusFail || c.ErrorType != "internal" {
		t.Fatalf("rep = %+v", rep)
	}
}

func TestRegistrationPanics(t *testing.T) {
	h := health.New(quiet)
	h.AddReadiness("db", ok)
	h.AddLiveness("db", ok) // separate namespace

	for name, fn := range map[string]func(){
		"duplicate": func() { h.AddReadiness("db", ok) },
		"nil":       func() { h.AddReadiness("x", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			fn()
		})
	}
}

func TestLogsOnlyTransitions(t *testing.T) {
	logger, logs := testkit.NewLogger(t)
	h := health.New(health.WithLogger(logger), health.WithCacheTTL(0))
	h.MarkStarted()
	var down atomic.Bool
	h.AddReadiness("db", func(context.Context) error {
		if down.Load() {
			return errors.Unavailable.New("refused")
		}
		return nil
	})

	h.Ready(context.Background()) // ok: no log
	down.Store(true)
	for range 3 {
		h.Ready(context.Background())
	}
	down.Store(false)
	h.Ready(context.Background())

	out := logs.String()
	if n := strings.Count(out, "health check failing"); n != 1 {
		t.Errorf("failing logged %d times:\n%s", n, out)
	}
	if n := strings.Count(out, "health check recovered"); n != 1 {
		t.Errorf("recovered logged %d times:\n%s", n, out)
	}
	if !strings.Contains(out, `"error_type":"unavailable"`) {
		t.Errorf("failure log lacks error_type:\n%s", out)
	}
}

func TestMetrics(t *testing.T) {
	mp, m := testkit.NewMetrics(t)
	h := health.New(quiet, health.WithMeterProvider(mp))
	h.MarkStarted()
	h.AddReadiness("db", ok)
	h.Ready(context.Background())

	got := m.HistogramCount("health.check.duration",
		attribute.String("probe", "ready"), attribute.String("check", "db"), attribute.String("outcome", "ok"))
	if got != 1 {
		t.Errorf("health.check.duration{probe=ready,check=db,outcome=ok} count = %d, want 1", got)
	}
}
