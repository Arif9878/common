package circuitbreaker_test

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/testkit"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/resilience/circuitbreaker"
	"github.com/Arif9878/common/go/resilience/retry"
)

// cooldown is just past the default 10s cooldown, whose boundary is
// exclusive.
const cooldown = 10*time.Second + time.Millisecond

var (
	quiet   = circuitbreaker.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	errDown = errors.Unavailable.New("down")
)

func fail(context.Context) error    { return errDown }
func succeed(context.Context) error { return nil }

func call(t *testing.T, b *circuitbreaker.Breaker, op func(context.Context) error, n int) {
	t.Helper()
	for range n {
		_ = b.Execute(context.Background(), op)
	}
}

func TestTripsAfterConsecutiveFailures(t *testing.T) {
	b := circuitbreaker.New("dep", quiet)
	call(t, b, fail, 4)
	call(t, b, succeed, 1) // resets the consecutive count
	call(t, b, fail, 4)
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v after non-consecutive failures", b.State())
	}
	call(t, b, fail, 1)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("state = %v after 5 consecutive failures", b.State())
	}

	var called bool
	err := b.Execute(context.Background(), func(context.Context) error { called = true; return nil })
	if called {
		t.Error("op called while open")
	}
	if !errors.Is(err, circuitbreaker.ErrOpen) || errors.KindOf(err) != errors.Unavailable {
		t.Errorf("err = %v", err)
	}
	if err.Error() != "circuitbreaker dep: circuit breaker is open" {
		t.Errorf("err = %q", err)
	}
}

func TestClientErrorsAndCancellationDoNotTrip(t *testing.T) {
	b := circuitbreaker.New("dep", quiet, circuitbreaker.WithConsecutiveFailures(2))
	call(t, b, func(context.Context) error { return errors.NotFound.New("x") }, 10)
	call(t, b, func(context.Context) error { return errors.InvalidArgument.New("x") }, 10)
	call(t, b, func(context.Context) error { return context.Canceled }, 10)
	if b.State() != circuitbreaker.Closed {
		t.Fatalf("state = %v", b.State())
	}

	// Canceled calls are not counted at all, so they do not reset a run of
	// failures either.
	call(t, b, fail, 1)
	call(t, b, func(context.Context) error { return context.Canceled }, 1)
	call(t, b, fail, 1)
	if b.State() != circuitbreaker.Open {
		t.Fatalf("state = %v, canceled call reset the failure count", b.State())
	}
}

func TestCustomIsFailure(t *testing.T) {
	b := circuitbreaker.New("dep", quiet, circuitbreaker.WithConsecutiveFailures(1),
		circuitbreaker.WithIsFailure(func(err error) bool { return errors.KindOf(err) == errors.NotFound }))
	call(t, b, fail, 3)
	if b.State() != circuitbreaker.Closed {
		t.Fatal("unavailable counted although rule excludes it")
	}
	call(t, b, func(context.Context) error { return errors.NotFound.New("x") }, 1)
	if b.State() != circuitbreaker.Open {
		t.Fatal("custom failure did not trip")
	}
}

func TestHalfOpenRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var transitions []string
		b := circuitbreaker.New("dep", quiet, circuitbreaker.WithConsecutiveFailures(1),
			circuitbreaker.WithCooldown(10*time.Second),
			circuitbreaker.WithOnStateChange(func(_ string, from, to circuitbreaker.State) {
				transitions = append(transitions, from.String()+">"+to.String())
			}))

		call(t, b, fail, 1)
		time.Sleep(9 * time.Second)
		if b.State() != circuitbreaker.Open {
			t.Fatalf("state = %v before cooldown", b.State())
		}
		time.Sleep(time.Second + time.Millisecond) // the cooldown boundary is exclusive
		if b.State() != circuitbreaker.HalfOpen {
			t.Fatalf("state = %v after cooldown", b.State())
		}

		call(t, b, fail, 1) // probe fails
		if b.State() != circuitbreaker.Open {
			t.Fatalf("state = %v after failed probe", b.State())
		}
		time.Sleep(cooldown)
		call(t, b, succeed, 1) // probe succeeds
		if b.State() != circuitbreaker.Closed {
			t.Fatalf("state = %v after successful probe", b.State())
		}

		want := []string{"closed>open", "open>half_open", "half_open>open", "open>half_open", "half_open>closed"}
		if len(transitions) != len(want) {
			t.Fatalf("transitions = %v", transitions)
		}
		for i := range want {
			if transitions[i] != want[i] {
				t.Fatalf("transitions = %v, want %v", transitions, want)
			}
		}
	})
}

func TestHalfOpenLimitsProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := circuitbreaker.New("dep", quiet, circuitbreaker.WithConsecutiveFailures(1))
		call(t, b, fail, 1)
		time.Sleep(cooldown)

		release := make(chan struct{})
		go func() {
			_ = b.Execute(context.Background(), func(context.Context) error {
				<-release
				return nil
			})
		}()
		synctest.Wait() // probe is in flight

		if err := b.Execute(context.Background(), succeed); !errors.Is(err, circuitbreaker.ErrOpen) {
			t.Errorf("second probe allowed: %v", err)
		}
		close(release)
		synctest.Wait()
		if b.State() != circuitbreaker.Closed {
			t.Errorf("state = %v", b.State())
		}
	})
}

func TestFailureRatio(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := circuitbreaker.New("dep", quiet,
			circuitbreaker.WithFailureRatio(0.5, 10), circuitbreaker.WithWindow(10*time.Second))

		// 4 failures, 4 successes: 50% but below the 10-request minimum.
		for range 4 {
			call(t, b, fail, 1)
			call(t, b, succeed, 1)
		}
		if b.State() != circuitbreaker.Closed {
			t.Fatalf("tripped below minRequests")
		}
		// The window rolls past these outcomes.
		time.Sleep(11 * time.Second)
		call(t, b, succeed, 6)
		call(t, b, fail, 4) // 4/10 = 40%
		if b.State() != circuitbreaker.Closed {
			t.Fatalf("tripped at 40%%; old outcomes not expired?")
		}
		call(t, b, fail, 2) // 6/12 = 50%
		if b.State() != circuitbreaker.Open {
			t.Fatalf("state = %v at 50%% failures", b.State())
		}
	})
}

func TestPanicRecordedAndRepanicked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := circuitbreaker.New("dep", quiet, circuitbreaker.WithConsecutiveFailures(1))
		panicky := func(context.Context) error { panic("boom") }

		mustPanic := func() {
			defer func() {
				if recover() == nil {
					t.Fatal("panic swallowed")
				}
			}()
			_ = b.Execute(context.Background(), panicky)
		}
		mustPanic()
		if b.State() != circuitbreaker.Open {
			t.Fatalf("panic not counted as failure: %v", b.State())
		}

		// A panicking half-open probe must not wedge the breaker.
		time.Sleep(cooldown)
		mustPanic()
		time.Sleep(cooldown)
		if err := b.Execute(context.Background(), succeed); err != nil {
			t.Fatalf("breaker wedged after panicking probe: %v", err)
		}
	})
}

func TestDoneContextNotCounted(t *testing.T) {
	b := circuitbreaker.New("dep", quiet, circuitbreaker.WithConsecutiveFailures(1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var called bool
	err := b.Execute(ctx, func(context.Context) error { called = true; return errDown })
	if called || errors.KindOf(err) != errors.Canceled || b.State() != circuitbreaker.Closed {
		t.Fatalf("called = %v, err = %v, state = %v", called, err, b.State())
	}
}

func TestExecuteValue(t *testing.T) {
	b := circuitbreaker.New("dep", quiet)
	v, err := circuitbreaker.Execute(context.Background(), b, func(context.Context) (int, error) { return 42, nil })
	if v != 42 || err != nil {
		t.Fatalf("Execute = %v, %v", v, err)
	}
}

func TestConcurrentUse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := circuitbreaker.New("dep", quiet, circuitbreaker.WithCooldown(time.Second))
		var wg sync.WaitGroup
		var rejected atomic.Int32
		for range 32 {
			wg.Go(func() {
				for range 200 {
					err := b.Execute(context.Background(), func(context.Context) error {
						if rand.IntN(3) == 0 { //nolint:gosec // test data
							return errDown
						}
						return nil
					})
					if errors.Is(err, circuitbreaker.ErrOpen) {
						rejected.Add(1)
						time.Sleep(100 * time.Millisecond)
					}
				}
			})
		}
		wg.Wait()
		t.Logf("rejected %d calls", rejected.Load())
	})
}

func TestRetryAroundBreaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := circuitbreaker.New("dep", quiet, circuitbreaker.WithConsecutiveFailures(2),
			circuitbreaker.WithCooldown(time.Second))
		var calls int
		start := time.Now()
		err := retry.Do(context.Background(), func(ctx context.Context) error {
			return b.Execute(ctx, func(context.Context) error {
				calls++
				if calls <= 2 {
					return errDown // trips the breaker
				}
				return nil
			})
		}, retry.WithoutJitter(), retry.WithMaxAttempts(10), retry.WithConstantBackoff(300*time.Millisecond))
		if err != nil {
			t.Fatalf("retry = %v", err)
		}
		// Two real failures, then rejections without calling the dependency
		// until the 1s cooldown passes, then one successful probe.
		if calls != 3 {
			t.Errorf("dependency called %d times, want 3", calls)
		}
		if d := time.Since(start); d < time.Second {
			t.Errorf("finished after %v, before the cooldown", d)
		}
	})
}

func TestMetrics(t *testing.T) {
	mp, m := testkit.NewMetrics(t)
	b := circuitbreaker.New("dep", quiet, circuitbreaker.WithMeterProvider(mp), circuitbreaker.WithConsecutiveFailures(1))
	call(t, b, succeed, 1)
	call(t, b, func(context.Context) error { return errors.NotFound.New("x") }, 1)
	call(t, b, fail, 1)
	call(t, b, succeed, 1) // rejected

	for _, o := range []string{"success", "client_error", "failure", "rejected"} {
		if got := m.Sum("circuitbreaker.requests", attribute.String("breaker", "dep"), attribute.String("outcome", o)); got != 1 {
			t.Errorf("circuitbreaker.requests{outcome=%s} = %v, want 1", o, got)
		}
	}
	if got := m.Gauge("circuitbreaker.state", attribute.String("breaker", "dep")); got != float64(circuitbreaker.Open) {
		t.Errorf("state gauge = %v, want %d", got, circuitbreaker.Open)
	}
}
