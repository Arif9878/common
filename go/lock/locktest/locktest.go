// Package locktest checks lock.Locker implementations. Use it from the
// tests of a locker; production code must not import it.
package locktest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lock"
)

// Run checks l. advance must move the store's clock forward by d. Keys are
// prefixed with prefix so runs can share a store.
func Run(t *testing.T, l lock.Locker, advance func(d time.Duration), prefix string) {
	ctx := context.Background()
	key := func(name string) string { return prefix + t.Name() + ":" + name }

	t.Run("exclusive", func(t *testing.T) {
		a, err := l.TryAcquire(ctx, key("x"), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.TryAcquire(ctx, key("x"), time.Minute); !errors.Is(err, lock.ErrNotAcquired) || errors.KindOf(err) != errors.Conflict {
			t.Fatalf("second TryAcquire = %v", err)
		}
		if err := a.Release(ctx); err != nil {
			t.Fatal(err)
		}
		b, err := l.TryAcquire(ctx, key("x"), time.Minute)
		if err != nil {
			t.Fatalf("after release: %v", err)
		}
		if b.Fence <= a.Fence {
			t.Errorf("fence %d after %d; must increase", b.Fence, a.Fence)
		}
		_ = b.Release(ctx)
	})

	t.Run("expiry and takeover", func(t *testing.T) {
		old, err := l.TryAcquire(ctx, key("exp"), 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		advance(time.Second)
		cur, err := l.TryAcquire(ctx, key("exp"), time.Minute)
		if err != nil {
			t.Fatalf("expired lease not taken over: %v", err)
		}
		if err := old.Extend(ctx, time.Minute); !errors.Is(err, lock.ErrNotHeld) {
			t.Errorf("stale Extend = %v", err)
		}
		if err := old.Release(ctx); !errors.Is(err, lock.ErrNotHeld) {
			t.Errorf("stale Release = %v", err)
		}
		if _, err := l.TryAcquire(ctx, key("exp"), time.Minute); !errors.Is(err, lock.ErrNotAcquired) {
			t.Errorf("stale Release freed the new holder's lease: %v", err)
		}
		_ = cur.Release(ctx)
	})

	t.Run("extend keeps the lease", func(t *testing.T) {
		a, err := l.TryAcquire(ctx, key("ext"), 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Extend(ctx, 3*time.Second); err != nil {
			t.Fatal(err)
		}
		advance(time.Second)
		if _, err := l.TryAcquire(ctx, key("ext"), time.Minute); !errors.Is(err, lock.ErrNotAcquired) {
			t.Fatalf("extended lease was taken: %v", err)
		}
		_ = a.Release(ctx)
	})

	t.Run("acquire waits", func(t *testing.T) {
		a, err := l.TryAcquire(ctx, key("wait"), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if _, err := lock.Acquire(short, l, key("wait"), time.Minute); !errors.Is(err, lock.ErrNotAcquired) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Acquire with short deadline = %v", err)
		}
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = a.Release(ctx)
		}()
		long, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		defer cancel2()
		b, err := lock.Acquire(long, l, key("wait"), time.Minute)
		if err != nil {
			t.Fatalf("Acquire after release = %v", err)
		}
		_ = b.Release(ctx)
	})

	t.Run("mutual exclusion", func(t *testing.T) {
		var inside, violations, entries atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				for range 5 {
					c, cancel := context.WithTimeout(ctx, 30*time.Second)
					lease, err := lock.Acquire(c, l, key("mutex"), time.Minute,
						lock.WithPollInterval(time.Millisecond, 20*time.Millisecond))
					cancel()
					if err != nil {
						t.Error(err)
						return
					}
					if inside.Add(1) != 1 {
						violations.Add(1)
					}
					entries.Add(1)
					time.Sleep(time.Millisecond)
					inside.Add(-1)
					if err := lease.Release(ctx); err != nil {
						t.Error(err)
					}
				}
			})
		}
		wg.Wait()
		if violations.Load() != 0 || entries.Load() != 100 {
			t.Fatalf("%d violations in %d entries", violations.Load(), entries.Load())
		}
	})
}
