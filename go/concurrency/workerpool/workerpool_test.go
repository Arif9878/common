package workerpool_test

import (
	"context"
	stderrors "errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/testkit"

	"github.com/Arif9878/common/go/concurrency/workerpool"
	"github.com/Arif9878/common/go/errors"
)

var quiet = workerpool.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

func shutdown(t *testing.T, p *workerpool.Pool) {
	t.Helper()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestBoundedWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := workerpool.New("test", quiet, workerpool.WithWorkers(4), workerpool.WithQueueSize(100))
		var active, peak, done atomic.Int32
		start := time.Now()
		for range 20 {
			err := p.Submit(context.Background(), func(context.Context) error {
				n := active.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(time.Second)
				active.Add(-1)
				done.Add(1)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		shutdown(t, p)
		if done.Load() != 20 || peak.Load() != 4 {
			t.Errorf("done = %d, peak = %d", done.Load(), peak.Load())
		}
		if d := time.Since(start); d != 5*time.Second {
			t.Errorf("20 one-second tasks on 4 workers took %v, want 5s", d)
		}
	})
}

func TestSubmitBlocksWhenFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := workerpool.New("test", quiet, workerpool.WithWorkers(1), workerpool.WithQueueSize(1))
		release := make(chan struct{})
		blocker := func(context.Context) error { <-release; return nil }

		_ = p.Submit(context.Background(), blocker) // running
		synctest.Wait()
		_ = p.Submit(context.Background(), blocker) // queued

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		if err := p.Submit(ctx, blocker); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Submit on full queue = %v", err)
		}
		if time.Since(start) != time.Second {
			t.Errorf("Submit returned after %v, want to block until ctx deadline", time.Since(start))
		}

		if err := p.TrySubmit(context.Background(), blocker); !errors.Is(err, workerpool.ErrQueueFull) ||
			errors.KindOf(err) != errors.Unavailable {
			t.Errorf("TrySubmit = %v", err)
		}

		// Room appears once the running task finishes.
		accepted := make(chan error)
		go func() { accepted <- p.Submit(context.Background(), func(context.Context) error { return nil }) }()
		synctest.Wait()
		select {
		case <-accepted:
			t.Fatal("Submit returned while the queue was still full")
		default:
		}
		close(release)
		if err := <-accepted; err != nil {
			t.Fatal(err)
		}
		shutdown(t, p)
	})
}

func TestZeroQueueHandsOffDirectly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := workerpool.New("test", quiet, workerpool.WithWorkers(1), workerpool.WithQueueSize(0))
		synctest.Wait() // worker idle
		release := make(chan struct{})
		if err := p.TrySubmit(context.Background(), func(context.Context) error { <-release; return nil }); err != nil {
			t.Fatalf("TrySubmit to idle worker = %v", err)
		}
		synctest.Wait()
		if err := p.TrySubmit(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, workerpool.ErrQueueFull) {
			t.Fatalf("TrySubmit to busy worker = %v", err)
		}
		close(release)
		shutdown(t, p)
	})
}

func TestErrorsAndPanics(t *testing.T) {
	var mu sync.Mutex
	var got []error
	p := workerpool.New("test", quiet, workerpool.WithWorkers(1), workerpool.WithOnError(func(_ context.Context, err error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, err)
	}))
	errTask := stderrors.New("task failed")
	var ranAfterPanic atomic.Bool
	_ = p.Submit(context.Background(), func(context.Context) error { return errTask })
	_ = p.Submit(context.Background(), func(context.Context) error { panic("boom") })
	_ = p.Submit(context.Background(), func(context.Context) error { ranAfterPanic.Store(true); return nil })
	shutdown(t, p)

	if len(got) != 2 || !errors.Is(got[0], errTask) || errors.KindOf(got[1]) != errors.Internal {
		t.Fatalf("errors = %v", got)
	}
	if !ranAfterPanic.Load() {
		t.Fatal("worker died after a panic")
	}
}

type ctxKey struct{}

func TestTaskContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := workerpool.New("test", quiet)
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "req-1"))

		result := make(chan string, 1)
		_ = p.Submit(ctx, func(ctx context.Context) error {
			time.Sleep(time.Second)
			v, _ := ctx.Value(ctxKey{}).(string)
			if ctx.Err() != nil {
				v += " canceled"
			}
			result <- v
			return nil
		})
		cancel() // the submitter giving up does not cancel the task
		if got := <-result; got != "req-1" {
			t.Fatalf("task saw %q", got)
		}
		shutdown(t, p)
	})
}

func TestShutdownDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := workerpool.New("test", quiet, workerpool.WithWorkers(2), workerpool.WithQueueSize(50))
		var done atomic.Int32
		for range 50 {
			_ = p.Submit(context.Background(), func(context.Context) error {
				time.Sleep(100 * time.Millisecond)
				done.Add(1)
				return nil
			})
		}
		shutdown(t, p)
		if done.Load() != 50 {
			t.Fatalf("done = %d, want all 50 queued tasks", done.Load())
		}
		if err := p.Submit(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, workerpool.ErrClosed) {
			t.Errorf("Submit after Shutdown = %v", err)
		}
		if err := p.TrySubmit(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, workerpool.ErrClosed) {
			t.Errorf("TrySubmit after Shutdown = %v", err)
		}
		shutdown(t, p) // idempotent
	})
}

func TestShutdownDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := workerpool.New("test", quiet, workerpool.WithWorkers(1), workerpool.WithQueueSize(10))
		var cause error
		var queuedRan atomic.Bool
		_ = p.Submit(context.Background(), func(ctx context.Context) error {
			<-ctx.Done()
			cause = context.Cause(ctx)
			return ctx.Err()
		})
		for range 5 {
			_ = p.Submit(context.Background(), func(context.Context) error { queuedRan.Store(true); return nil })
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		err := p.Shutdown(ctx)
		if time.Since(start) != 2*time.Second || errors.KindOf(err) != errors.Timeout {
			t.Fatalf("Shutdown = %v after %v", err, time.Since(start))
		}
		synctest.Wait()
		if errors.KindOf(cause) != errors.Timeout {
			t.Errorf("running task's cancel cause = %v", cause)
		}
		if queuedRan.Load() {
			t.Error("queued task ran after the shutdown deadline")
		}
	})
}

func TestSubmitShutdownRace(t *testing.T) {
	for range 20 {
		p := workerpool.New("test", quiet, workerpool.WithWorkers(4), workerpool.WithQueueSize(8))
		var accepted, ran atomic.Int64
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				for {
					err := p.Submit(context.Background(), func(context.Context) error {
						ran.Add(1)
						return nil
					})
					if err != nil {
						return
					}
					accepted.Add(1)
				}
			})
		}
		time.Sleep(time.Millisecond)
		shutdown(t, p)
		wg.Wait()
		if accepted.Load() != ran.Load() {
			t.Fatalf("accepted %d tasks but ran %d: tasks lost during shutdown", accepted.Load(), ran.Load())
		}
	}
}

func TestNoGoroutinePerTask(t *testing.T) {
	p := workerpool.New("test", quiet, workerpool.WithWorkers(5), workerpool.WithQueueSize(1000))
	release := make(chan struct{})
	before := runtime.NumGoroutine()
	for range 1000 {
		if err := p.Submit(context.Background(), func(context.Context) error { <-release; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if grown := runtime.NumGoroutine() - before; grown > 0 {
		t.Errorf("goroutines grew by %d while queuing 1000 tasks", grown)
	}
	close(release)
	shutdown(t, p)
}

func TestMetrics(t *testing.T) {
	mp, m := testkit.NewMetrics(t)
	p := workerpool.New("thumbs", quiet, workerpool.WithWorkers(3), workerpool.WithQueueSize(7),
		workerpool.WithMeterProvider(mp), workerpool.WithOnError(func(context.Context, error) {}))
	_ = p.Submit(context.Background(), func(context.Context) error { return nil })
	_ = p.Submit(context.Background(), func(context.Context) error { return stderrors.New("x") })
	_ = p.Submit(context.Background(), func(context.Context) error { panic("x") })

	// Gauges are observed while the pool is running.
	pool := attribute.String("pool", "thumbs")
	if m.Gauge("workerpool.workers", pool) != 3 || m.Gauge("workerpool.queue.capacity", pool) != 7 {
		t.Errorf("gauges: workers %v, capacity %v", m.Gauge("workerpool.workers", pool), m.Gauge("workerpool.queue.capacity", pool))
	}
	shutdown(t, p)
	for _, o := range []string{"success", "error", "panic"} {
		if got := m.Sum("workerpool.tasks", pool, attribute.String("outcome", o)); got != 1 {
			t.Errorf("workerpool.tasks{outcome=%s} = %v, want 1", o, got)
		}
	}
	if got := m.HistogramCount("workerpool.task.duration", pool); got != 3 {
		t.Errorf("task duration count = %d", got)
	}
}

func BenchmarkSubmit(b *testing.B) {
	p := workerpool.New("bench", quiet, workerpool.WithWorkers(runtime.GOMAXPROCS(0)), workerpool.WithQueueSize(1024))
	defer func() { _ = p.Shutdown(context.Background()) }()
	task := func(context.Context) error { return nil }
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = p.Submit(context.Background(), task)
		}
	})
}
