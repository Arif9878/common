package ratelimit_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/testkit"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/resilience/ratelimit"
	"github.com/Arif9878/common/go/resilience/retry"
)

func TestAllow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 10, 2)
		for range 2 {
			if !l.Allow() {
				t.Fatal("burst not allowed")
			}
		}
		if l.Allow() {
			t.Fatal("allowed beyond burst")
		}
		time.Sleep(100 * time.Millisecond)
		if !l.Allow() {
			t.Fatal("token not refilled after 1/rate")
		}
	})
}

func TestWaitSpacesOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 10, 1)
		start := time.Now()
		for range 5 {
			if err := l.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if d := time.Since(start); d != 400*time.Millisecond {
			t.Errorf("5 operations at 10/s took %v, want 400ms", d)
		}
	})
}

func TestWaitRejectsLongWaitsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 1, 1, ratelimit.WithMaxWait(100*time.Millisecond))
		_ = l.Wait(context.Background())

		start := time.Now()
		err := l.Wait(context.Background())
		if time.Since(start) != 0 {
			t.Errorf("waited %v before rejecting", time.Since(start))
		}
		var le *ratelimit.LimitedError
		if !errors.As(err, &le) || le.RetryAfter() != time.Second || le.Limiter != "api" {
			t.Fatalf("err = %#v", err)
		}
		if !errors.Is(err, ratelimit.ErrLimited) || errors.KindOf(err) != errors.RateLimited || !errors.IsRetryable(err) {
			t.Errorf("err = %v, kind %v", err, errors.KindOf(err))
		}

		// The rejected call did not consume a token.
		time.Sleep(time.Second)
		if !l.Allow() {
			t.Error("rejected Wait consumed a token")
		}
	})
}

func TestWaitRespectsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 1, 1, ratelimit.WithMaxWait(time.Minute))
		_ = l.Wait(context.Background())

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := l.Wait(ctx); !errors.Is(err, ratelimit.ErrLimited) || time.Since(start) != 0 {
			t.Errorf("err = %v after %v; want immediate rejection", err, time.Since(start))
		}
	})
}

func TestWaitCanceledReturnsToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 1, 1)
		_ = l.Wait(context.Background())

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		if err := l.Wait(ctx); errors.KindOf(err) != errors.Canceled {
			t.Fatalf("err = %v", err)
		}
		// The canceled reservation is returned: the next token is due at 1s,
		// not 2s.
		start := time.Now()
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 900*time.Millisecond {
			t.Errorf("waited %v, want 900ms", got)
		}

		if err := l.Wait(ctx); errors.KindOf(err) != errors.Canceled {
			t.Errorf("Wait with done ctx = %v", err)
		}
	})
}

func TestUnlimited(t *testing.T) {
	l := ratelimit.New("api", math.Inf(1), 1)
	for range 1000 {
		if !l.Allow() {
			t.Fatal("infinite limit rejected")
		}
	}
	if l.Burst() != 1 || !math.IsInf(l.Limit(), 1) {
		t.Errorf("Limit = %v, Burst = %d", l.Limit(), l.Burst())
	}
}

func TestSetLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 1, 1)
		l.Allow()
		l.SetLimit(100)
		time.Sleep(10 * time.Millisecond)
		if !l.Allow() {
			t.Error("new rate not applied")
		}
	})
}

func TestConcurrentWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 100, 10, ratelimit.WithMaxWait(2*time.Second))
		start := time.Now()
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				if err := l.Wait(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		// 10 from the burst, then 90 at 100/s.
		if d := time.Since(start); d != 900*time.Millisecond {
			t.Errorf("took %v, want 900ms", d)
		}
	})
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := ratelimit.New("api", 1, 1, ratelimit.WithMaxWait(0))
		_ = l.Wait(context.Background())
		start := time.Now()
		err := retry.Do(context.Background(), l.Wait, retry.WithoutJitter())
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != time.Second {
			t.Errorf("retry waited %v, want the 1s Retry-After", d)
		}
	})
}

func TestMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mp, m := testkit.NewMetrics(t)
		l := ratelimit.New("api", 10, 1, ratelimit.WithMeterProvider(mp))
		l.Allow()                        // allowed
		l.Allow()                        // limited
		_ = l.Wait(context.Background()) // delayed

		for _, o := range []string{"allowed", "limited", "delayed"} {
			if got := m.Sum("ratelimit.requests", attribute.String("limiter", "api"), attribute.String("outcome", o)); got != 1 {
				t.Errorf("ratelimit.requests{outcome=%s} = %v, want 1", o, got)
			}
		}
		if got := m.HistogramCount("ratelimit.wait.duration"); got != 1 {
			t.Errorf("wait histogram count = %d", got)
		}
	})
}
