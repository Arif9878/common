package batch_test

import (
	"context"
	stderrors "errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Arif9878/common/go/testkit"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/concurrency/batch"
	"github.com/Arif9878/common/go/errors"
)

var quiet = batch.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

// recorder is a handler that records batches and when they arrived.
type recorder struct {
	mu      sync.Mutex
	batches [][]int
	times   []time.Duration
	start   time.Time
}

func newRecorder() *recorder { return &recorder{start: time.Now()} }

func (r *recorder) handle(_ context.Context, items []int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, items)
	r.times = append(r.times, time.Since(r.start))
	return nil
}

func addAll(t *testing.T, p *batch.Processor[int], items ...int) {
	t.Helper()
	for _, it := range items {
		if err := p.Add(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
}

func closeP(t *testing.T, p *batch.Processor[int]) {
	t.Helper()
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestFlushBySizeThenInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRecorder()
		p := batch.New("t", r.handle, quiet, batch.WithSize(3), batch.WithFlushInterval(time.Second))
		addAll(t, p, 1, 2, 3, 4, 5, 6, 7)
		time.Sleep(2 * time.Second)
		closeP(t, p)

		want := [][]int{{1, 2, 3}, {4, 5, 6}, {7}}
		if len(r.batches) != 3 {
			t.Fatalf("batches = %v", r.batches)
		}
		for i := range want {
			if !slices.Equal(r.batches[i], want[i]) {
				t.Fatalf("batches = %v, want %v", r.batches, want)
			}
		}
		if r.times[0] != 0 || r.times[1] != 0 || r.times[2] != time.Second {
			t.Errorf("flush times = %v, want [0 0 1s]", r.times)
		}
	})
}

func TestIntervalCountsFromOldestItem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRecorder()
		p := batch.New("t", r.handle, quiet, batch.WithSize(100), batch.WithFlushInterval(time.Second))
		addAll(t, p, 1)
		time.Sleep(600 * time.Millisecond)
		addAll(t, p, 2)
		time.Sleep(2 * time.Second)
		addAll(t, p, 3)
		closeP(t, p)

		if len(r.batches) != 2 || !slices.Equal(r.batches[0], []int{1, 2}) || r.times[0] != time.Second {
			t.Fatalf("batches = %v at %v", r.batches, r.times)
		}
		if !slices.Equal(r.batches[1], []int{3}) {
			t.Fatalf("close did not flush the remainder: %v", r.batches)
		}
	})
}

func TestExplicitFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errStore := stderrors.New("store down")
		var fail atomic.Bool
		var handled atomic.Int64
		p := batch.New("t", func(_ context.Context, items []int) error {
			time.Sleep(100 * time.Millisecond)
			handled.Add(int64(len(items)))
			if fail.Load() {
				return errStore
			}
			return nil
		}, quiet, batch.WithSize(10), batch.WithFlushInterval(time.Hour),
			batch.WithOnFailure(func(context.Context, []int, error) {}))

		if err := p.Flush(context.Background()); err != nil {
			t.Fatalf("empty Flush = %v", err)
		}
		addAll(t, p, 1, 2, 3)
		if err := p.Flush(context.Background()); err != nil || handled.Load() != 3 {
			t.Fatalf("Flush = %v, handled %d", err, handled.Load())
		}
		fail.Store(true)
		addAll(t, p, 4)
		if err := p.Flush(context.Background()); !errors.Is(err, errStore) {
			t.Fatalf("Flush = %v, want handler error", err)
		}
		closeP(t, p)
	})
}

func TestCloseFlushesAndRejects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRecorder()
		p := batch.New("t", r.handle, quiet, batch.WithSize(100), batch.WithFlushInterval(time.Hour))
		addAll(t, p, 1, 2)
		closeP(t, p)
		if len(r.batches) != 1 || len(r.batches[0]) != 2 {
			t.Fatalf("batches = %v", r.batches)
		}
		if err := p.Add(context.Background(), 3); !errors.Is(err, batch.ErrClosed) {
			t.Errorf("Add after Close = %v", err)
		}
		if err := p.Flush(context.Background()); !errors.Is(err, batch.ErrClosed) {
			t.Errorf("Flush after Close = %v", err)
		}
		closeP(t, p) // idempotent
	})
}

func TestBoundedMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		p := batch.New("t", func(context.Context, []int) error { <-release; return nil },
			quiet, batch.WithSize(2), batch.WithMaxPending(4))

		accepted := 0
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := p.Add(ctx, accepted)
			cancel()
			if err != nil {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				break
			}
			accepted++
			if accepted > 100 {
				t.Fatal("Add never applied backpressure")
			}
		}
		// One batch (2) in the handler plus maxPending (4) waiting.
		if accepted != 6 {
			t.Errorf("accepted %d items before blocking, want 6", accepted)
		}
		close(release)
		closeP(t, p)
	})
}

func TestPartialAndTotalFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var failed [][]int
		var errs []error
		call := 0
		p := batch.New("t", func(_ context.Context, items []int) error {
			call++
			switch call {
			case 1:
				return &batch.PartialError{Failed: []int{1, 7}, Err: stderrors.New("row 2 rejected")}
			case 2:
				return errors.Unavailable.New("db down")
			default:
				panic("boom")
			}
		}, quiet, batch.WithSize(3), batch.WithOnFailure(func(_ context.Context, items []int, err error) {
			mu.Lock()
			defer mu.Unlock()
			failed = append(failed, items)
			errs = append(errs, err)
		}))
		addAll(t, p, 1, 2, 3, 4, 5, 6, 7, 8, 9)
		closeP(t, p)

		want := [][]int{{2}, {4, 5, 6}, {7, 8, 9}}
		if len(failed) != 3 {
			t.Fatalf("failed = %v", failed)
		}
		for i := range want {
			if !slices.Equal(failed[i], want[i]) {
				t.Fatalf("failed = %v, want %v (out-of-range index ignored)", failed, want)
			}
		}
		if errors.KindOf(errs[1]) != errors.Unavailable || errors.KindOf(errs[2]) != errors.Internal {
			t.Errorf("errors = %v", errs)
		}
	})
}

func TestDefaultFailureHookLogsCountOnly(t *testing.T) {
	logger, logs := testkit.NewLogger(t)
	p := batch.New("audit", func(context.Context, []string) error { return stderrors.New("down") },
		batch.WithLogger(logger))
	for _, s := range []string{"secret-a", "secret-b", "secret-c"} {
		_ = p.Add(context.Background(), s)
	}
	_ = p.Close(context.Background())
	out := logs.String()
	if !strings.Contains(out, `"items":3`) || strings.Contains(out, "secret") {
		t.Fatalf("log = %s", out)
	}
}

func TestCloseDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cause error
		var lost atomic.Int64
		p := batch.New("t", func(ctx context.Context, _ []int) error {
			<-ctx.Done()
			cause = context.Cause(ctx)
			return ctx.Err()
		}, quiet, batch.WithSize(5), batch.WithOnFailure(func(_ context.Context, items []int, _ error) {
			lost.Add(int64(len(items)))
		}))
		addAll(t, p, 1, 2, 3)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		err := p.Close(ctx)
		if errors.KindOf(err) != errors.Timeout || time.Since(start) != 2*time.Second {
			t.Fatalf("Close = %v after %v", err, time.Since(start))
		}
		synctest.Wait()
		if errors.KindOf(cause) != errors.Timeout || lost.Load() != 3 {
			t.Errorf("handler cause = %v, items reported lost = %d", cause, lost.Load())
		}
	})
}

func TestFlushTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var remaining time.Duration
		p := batch.New("t", func(ctx context.Context, _ []int) error {
			d, _ := ctx.Deadline()
			remaining = time.Until(d)
			return nil
		}, quiet, batch.WithFlushTimeout(5*time.Second))
		addAll(t, p, 1)
		closeP(t, p)
		if remaining != 5*time.Second {
			t.Errorf("handler deadline in %v", remaining)
		}
	})
}

func TestFlushConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(2)
		p := batch.New("t", func(context.Context, []int) error {
			wg.Done()
			wg.Wait() // deadlocks unless two batches run at once
			return nil
		}, quiet, batch.WithSize(1), batch.WithFlushConcurrency(2))
		addAll(t, p, 1, 2)
		closeP(t, p)
	})
}

func TestConcurrentProducers(t *testing.T) {
	var handled atomic.Int64
	p := batch.New("t", func(_ context.Context, items []int) error {
		handled.Add(int64(len(items)))
		return nil
	}, quiet, batch.WithSize(50), batch.WithFlushInterval(time.Millisecond))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 1000 {
				_ = p.Add(context.Background(), i)
				if i%250 == 0 {
					_ = p.Flush(context.Background())
				}
			}
		})
	}
	wg.Wait()
	closeP(t, p)
	if handled.Load() != 8000 {
		t.Fatalf("handled %d of 8000 items", handled.Load())
	}
}

func TestMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mp, m := testkit.NewMetrics(t)
		p := batch.New("events", func(context.Context, []int) error { return nil },
			quiet, batch.WithSize(2), batch.WithMeterProvider(mp))
		addAll(t, p, 1, 2, 3)
		time.Sleep(2 * time.Second)

		proc := attribute.String("processor", "events")
		checks := []struct {
			name      string
			got, want float64
		}{
			{"items success", m.Sum("batch.items", proc, attribute.String("outcome", "success")), 3},
			{"flushes by size", m.Sum("batch.flushes", proc, attribute.String("reason", "size")), 1},
			{"flushes by interval", m.Sum("batch.flushes", proc, attribute.String("reason", "interval")), 1},
			{"batch count", float64(m.HistogramCount("batch.size", proc)), 2},
			{"items in batches", m.HistogramSum("batch.size", proc), 3},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
			}
		}
		closeP(t, p)
	})
}

func BenchmarkAdd(b *testing.B) {
	p := batch.New("bench", func(context.Context, []int) error { return nil }, quiet, batch.WithSize(500))
	defer func() { _ = p.Close(context.Background()) }()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = p.Add(context.Background(), 1)
		}
	})
}
