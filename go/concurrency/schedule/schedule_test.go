package schedule_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/concurrency/schedule"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lock"
	"github.com/Arif9878/common/go/testkit"
)

var quiet = schedule.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

// memLocker is an in-memory lock.Locker whose leases expire by the
// (fake) clock.
type memLocker struct {
	mu     sync.Mutex
	until  map[string]time.Time
	fences uint64
}

func (l *memLocker) TryAcquire(_ context.Context, key string, ttl time.Duration) (*lock.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.until == nil {
		l.until = map[string]time.Time{}
	}
	if time.Now().Before(l.until[key]) {
		return nil, lock.ErrNotAcquired
	}
	l.until[key] = time.Now().Add(ttl)
	l.fences++
	return lock.NewLease(key, "t", l.fences, ttl,
		func(context.Context) error { return nil },
		func(context.Context, time.Duration) error { return nil }), nil
}

// start runs s until the test's bubble ends.
func start(t *testing.T, s *schedule.Scheduler) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	t.Cleanup(func() {
		_ = s.Stop(context.Background())
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

func TestOnceAcrossReplicas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		locker := &memLocker{}
		mp, metrics := testkit.NewMetrics(t)
		var runs atomic.Int32
		for range 3 { // three replicas
			s := schedule.New(schedule.WithLocker(locker), schedule.WithMeterProvider(mp), quiet)
			if err := s.Every("report", time.Minute, func(context.Context) error { runs.Add(1); return nil }); err != nil {
				t.Fatal(err)
			}
			start(t, s)
		}
		time.Sleep(10*time.Minute + time.Second)
		synctest.Wait()
		if n := runs.Load(); n != 10 {
			t.Errorf("%d runs in 10 minutes across 3 replicas, want 10", n)
		}
		if n := metrics.Sum("schedule.runs", attribute.String("job", "report"), attribute.String("outcome", "skipped")); n != 20 {
			t.Errorf("skipped = %v, want 20", n)
		}
	})
}

func TestEveryReplicaWithoutLocker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		for range 2 {
			s := schedule.New(quiet)
			_ = s.Every("cleanup", time.Minute, func(context.Context) error { runs.Add(1); return nil })
			start(t, s)
		}
		time.Sleep(5*time.Minute + time.Second)
		synctest.Wait()
		if n := runs.Load(); n != 10 {
			t.Errorf("%d runs, want 10 (5 per replica)", n)
		}
	})
}

func TestCron(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var at []string
		s := schedule.New(quiet)
		err := s.Cron("quarterly", "*/15 * * * *", func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, time.Now().UTC().Format("15:04"))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		base := time.Now()
		start(t, s)
		time.Sleep(time.Until(base.Truncate(time.Hour).Add(time.Hour + time.Second)))
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		if len(at) == 0 {
			t.Fatal("cron job never ran")
		}
		for _, hm := range at {
			if m := hm[3:]; m != "00" && m != "15" && m != "30" && m != "45" {
				t.Errorf("ran at %s, not on a quarter hour", hm)
			}
		}
	})
}

func TestFailuresPanicsAndOverlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mp, metrics := testkit.NewMetrics(t)
		s := schedule.New(schedule.WithMeterProvider(mp), quiet)
		_ = s.Every("fails", time.Minute, func(context.Context) error { return errors.Unavailable.New("down") })
		_ = s.Every("panics", time.Minute, func(context.Context) error { panic("boom") })
		var concurrent, maxConcurrent atomic.Int32
		_ = s.Every("slow", time.Minute, func(context.Context) error {
			n := concurrent.Add(1)
			if n > maxConcurrent.Load() {
				maxConcurrent.Store(n)
			}
			time.Sleep(90 * time.Second)
			concurrent.Add(-1)
			return nil
		}, schedule.WithTimeout(5*time.Minute))
		start(t, s)
		time.Sleep(4*time.Minute + time.Second)
		synctest.Wait()

		outcome := func(job, o string) float64 {
			if !metrics.Has("schedule.runs") {
				return 0
			}
			return metrics.Sum("schedule.runs", attribute.String("job", job), attribute.String("outcome", o))
		}
		if outcome("fails", "failed") != 4 || outcome("panics", "panicked") != 4 {
			t.Errorf("failed %v, panicked %v; want 4 each (the scheduler keeps going)", outcome("fails", "failed"), outcome("panics", "panicked"))
		}
		if maxConcurrent.Load() != 1 || outcome("slow", "skipped") == 0 {
			t.Errorf("slow job: max concurrent %d, skipped %v", maxConcurrent.Load(), outcome("slow", "skipped"))
		}
	})
}

func TestTimeoutAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ended atomic.Int64
		s := schedule.New(quiet)
		_ = s.Every("blocks", time.Minute, func(ctx context.Context) error {
			began := time.Now()
			<-ctx.Done()
			ended.Store(int64(time.Since(began)))
			return ctx.Err()
		}, schedule.WithTimeout(20*time.Second))
		done := make(chan error, 1)
		go func() { done <- s.Run(context.Background()) }()
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		if d := time.Duration(ended.Load()); d != 0 {
			t.Fatalf("run ended after %v, before its timeout", d)
		}
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if d := time.Duration(ended.Load()); d != 20*time.Second {
			t.Errorf("run cancelled after %v, want its 20s timeout", d)
		}

		// Stop waits for a run in progress; at its deadline, it cancels it.
		time.Sleep(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute + time.Second)))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err == nil {
			t.Error("Stop returned before the blocked run ended")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestValidation(t *testing.T) {
	s := schedule.New(quiet)
	noop := func(context.Context) error { return nil }
	for name, err := range map[string]error{
		"bad cron":  s.Cron("x", "every day", noop),
		"too often": s.Every("y", time.Millisecond, noop),
		"no name":   s.Every("", time.Minute, noop),
	} {
		if errors.KindOf(err) != errors.InvalidArgument {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	_ = s.Every("dup", time.Minute, noop)
	if err := s.Every("dup", time.Minute, noop); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("duplicate name: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop before Run: %v", err)
	}
}
