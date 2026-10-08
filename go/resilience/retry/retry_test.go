package retry_test

import (
	"context"
	stderrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/testkit"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/resilience/retry"
)

var errTransient = errors.Unavailable.New("unavailable")

// failing returns an op that fails with err for the first n calls.
func failing(n int, err error) (func(context.Context) error, *atomic.Int32) {
	var calls atomic.Int32
	return func(context.Context) error {
		if int(calls.Add(1)) <= n {
			return err
		}
		return nil
	}, &calls
}

func TestSucceedsAfterRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		op, calls := failing(2, errTransient)
		var delays []time.Duration
		start := time.Now()
		err := retry.Do(context.Background(), op, retry.WithoutJitter(),
			retry.WithOnRetry(func(_ int, _ error, d time.Duration) { delays = append(delays, d) }))
		if err != nil {
			t.Fatalf("Do = %v", err)
		}
		if calls.Load() != 3 {
			t.Errorf("calls = %d", calls.Load())
		}
		if len(delays) != 2 || delays[0] != 100*time.Millisecond || delays[1] != 200*time.Millisecond {
			t.Errorf("delays = %v", delays)
		}
		if d := time.Since(start); d != 300*time.Millisecond {
			t.Errorf("elapsed = %v", d)
		}
	})
}

func TestNonRetryableReturnsImmediately(t *testing.T) {
	plain := stderrors.New("validation failed")
	for name, err := range map[string]error{
		"unclassified": plain,
		"client error": errors.InvalidArgument.New("bad"),
		"canceled":     context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			op, calls := failing(10, err)
			got := retry.Do(context.Background(), op)
			if got != err { //nolint:errorlint // must be the identical, unwrapped error
				t.Errorf("Do = %v, want the original error unchanged", got)
			}
			if calls.Load() != 1 {
				t.Errorf("calls = %d", calls.Load())
			}
		})
	}
}

func TestPermanent(t *testing.T) {
	op, calls := failing(10, retry.Permanent(errTransient))
	err := retry.Do(context.Background(), op)
	if err != errTransient { //nolint:errorlint // must be the identical, unwrapped error
		t.Errorf("Do = %#v, want the unwrapped error", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d", calls.Load())
	}
	if retry.Permanent(nil) != nil {
		t.Error("Permanent(nil) != nil")
	}
}

func TestExhausted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		op, calls := failing(10, errTransient)
		err := retry.Do(context.Background(), op, retry.WithName("reserve"), retry.WithMaxAttempts(4))
		if calls.Load() != 4 {
			t.Errorf("calls = %d", calls.Load())
		}
		if !errors.Is(err, errTransient) || errors.KindOf(err) != errors.Unavailable {
			t.Errorf("err = %v, kind %v", err, errors.KindOf(err))
		}
		if want := "retry reserve: gave up after 4 attempts: unavailable"; err.Error() != want {
			t.Errorf("err = %q, want %q", err, want)
		}
	})
}

func TestMaxAttemptsAtLeastOne(t *testing.T) {
	op, calls := failing(10, errTransient)
	_ = retry.Do(context.Background(), op, retry.WithMaxAttempts(0))
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestBackoffCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		op, _ := failing(10, errTransient)
		var delays []time.Duration
		_ = retry.Do(context.Background(), op, retry.WithoutJitter(), retry.WithMaxAttempts(6),
			retry.WithExponentialBackoff(time.Second, 4*time.Second),
			retry.WithOnRetry(func(_ int, _ error, d time.Duration) { delays = append(delays, d) }))
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
		if len(delays) != len(want) {
			t.Fatalf("delays = %v", delays)
		}
		for i := range want {
			if delays[i] != want[i] {
				t.Fatalf("delays = %v, want %v", delays, want)
			}
		}
	})
}

func TestJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seen := map[time.Duration]bool{}
		for range 50 {
			op, _ := failing(1, errTransient)
			_ = retry.Do(context.Background(), op, retry.WithJitter(),
				retry.WithOnRetry(func(_ int, _ error, d time.Duration) {
					if d < 0 || d > 100*time.Millisecond {
						t.Errorf("delay %v outside [0, 100ms]", d)
					}
					seen[d] = true
				}))
		}
		if len(seen) < 10 {
			t.Errorf("only %d distinct delays in 50 runs; jitter not applied", len(seen))
		}
	})
}

func TestMaxElapsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		op, calls := failing(10, errTransient)
		start := time.Now()
		err := retry.Do(context.Background(), op, retry.WithoutJitter(), retry.WithMaxAttempts(10),
			retry.WithExponentialBackoff(time.Second, time.Minute), retry.WithMaxElapsed(2500*time.Millisecond))
		// t=0 fail, wait 1s; t=1s fail, next wait 2s would end at 3s > 2.5s.
		if calls.Load() != 2 || time.Since(start) != time.Second {
			t.Errorf("calls = %d after %v", calls.Load(), time.Since(start))
		}
		if !errors.Is(err, errTransient) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestStopsBeforeContextDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		op, calls := failing(10, errTransient)
		start := time.Now()
		err := retry.Do(ctx, op, retry.WithoutJitter(), retry.WithMaxAttempts(10))
		// t=0 fail, wait 100ms; t=100ms fail, next wait 200ms passes the deadline.
		if calls.Load() != 2 || time.Since(start) != 100*time.Millisecond {
			t.Errorf("calls = %d after %v", calls.Load(), time.Since(start))
		}
		if !errors.Is(err, errTransient) || !errors.Is(err, context.DeadlineExceeded) || errors.KindOf(err) != errors.Timeout {
			t.Errorf("err = %v, kind %v", err, errors.KindOf(err))
		}
	})
}

func TestCanceledWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		op, calls := failing(10, errTransient)
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		err := retry.Do(ctx, op, retry.WithoutJitter(), retry.WithMaxAttempts(10))
		if calls.Load() != 1 {
			t.Errorf("calls = %d", calls.Load())
		}
		if !errors.Is(err, context.Canceled) || !errors.Is(err, errTransient) || errors.KindOf(err) != errors.Canceled {
			t.Errorf("err = %v, kind %v", err, errors.KindOf(err))
		}
	})
}

type retryAfterErr struct{ after time.Duration }

func (e retryAfterErr) Error() string             { return "slow down" }
func (e retryAfterErr) RetryAfter() time.Duration { return e.after }

func TestRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		op, _ := failing(1, errors.RateLimited.Wrap(retryAfterErr{3 * time.Second}, ""))
		start := time.Now()
		if err := retry.Do(context.Background(), op); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 3*time.Second {
			t.Errorf("waited %v, want the 3s Retry-After", d)
		}
	})
}

func TestRetryIf(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plain := stderrors.New("eof")
		op, calls := failing(2, plain)
		err := retry.Do(context.Background(), op,
			retry.WithRetryIf(func(err error) bool { return errors.Is(err, plain) }))
		if err != nil || calls.Load() != 3 {
			t.Errorf("err = %v, calls = %d", err, calls.Load())
		}
	})
}

func TestDoValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		v, err := retry.DoValue(context.Background(), func(context.Context) (string, error) {
			calls++
			if calls < 2 {
				return "partial", errTransient
			}
			return "ok", nil
		})
		if v != "ok" || err != nil {
			t.Errorf("DoValue = %q, %v", v, err)
		}

		v, err = retry.DoValue(context.Background(), func(context.Context) (string, error) {
			return "partial", errors.NotFound.New("x")
		})
		if v != "" || err == nil {
			t.Errorf("failure returned %q, %v; want zero value", v, err)
		}
	})
}

func TestSharedPolicyConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := retry.New(retry.WithName("shared"), retry.WithMaxAttempts(5))
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() {
				op, _ := failing(i%4, errTransient)
				if err := p.Do(context.Background(), op); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
	})
}

func TestMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mp, m := testkit.NewMetrics(t)
		p := retry.New(retry.WithName("op"), retry.WithMeterProvider(mp), retry.WithMaxAttempts(2))

		op, _ := failing(1, errTransient)
		_ = p.Do(context.Background(), op) // retry, success
		op, _ = failing(5, errTransient)
		_ = p.Do(context.Background(), op) // retry, exhausted

		for outcome, want := range map[string]float64{"retry": 2, "success": 1, "exhausted": 1} {
			got := m.Sum("retry.attempts", attribute.String("operation", "op"), attribute.String("outcome", outcome))
			if got != want {
				t.Errorf("retry.attempts{outcome=%s} = %v, want %v", outcome, got, want)
			}
		}
	})
}
