// Package idempotencytest checks idempotency.Store implementations. Use it
// from the tests of a store; production code must not import it.
package idempotencytest

import (
	"context"
	stderrors "errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
)

// Run checks store's behavior. advance must move the store's clock forward
// by d (sleep, or a fake clock such as miniredis.FastForward). Keys are
// prefixed with prefix so runs can share a store.
func Run(t *testing.T, store idempotency.Store, advance func(d time.Duration), prefix string) {
	ctx := context.Background()
	key := func(name string) string { return prefix + t.Name() + ":" + name }

	t.Run("duplicate returns stored result", func(t *testing.T) {
		var runs atomic.Int32
		fn := func(context.Context) (string, error) { runs.Add(1); return "receipt-1", nil }
		v, out, err := idempotency.DoOutcome(ctx, store, key("dup"), fn)
		if err != nil || v != "receipt-1" || out != idempotency.Executed {
			t.Fatalf("first = %q, %v, %v", v, out, err)
		}
		v, out, err = idempotency.DoOutcome(ctx, store, key("dup"), fn)
		if err != nil || v != "receipt-1" || out != idempotency.Duplicate || runs.Load() != 1 {
			t.Fatalf("second = %q, %v, %v; runs %d", v, out, err, runs.Load())
		}
	})

	t.Run("concurrent calls run once", func(t *testing.T) {
		var runs atomic.Int32
		release := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]error, 20)
		for i := range results {
			wg.Go(func() {
				_, results[i] = idempotency.Do(ctx, store, key("concurrent"), func(context.Context) (int, error) {
					runs.Add(1)
					<-release
					return 1, nil
				})
			})
		}
		time.Sleep(100 * time.Millisecond)
		close(release)
		wg.Wait()
		if runs.Load() != 1 {
			t.Fatalf("fn ran %d times", runs.Load())
		}
		for _, err := range results {
			if err != nil && !errors.Is(err, idempotency.ErrInProgress) {
				t.Fatalf("unexpected error %v", err)
			}
		}
	})

	t.Run("failure releases the key", func(t *testing.T) {
		boom := stderrors.New("payment gateway down")
		_, err := idempotency.Do(ctx, store, key("fail"), func(context.Context) (int, error) { return 0, boom })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		v, err := idempotency.Do(ctx, store, key("fail"), func(context.Context) (int, error) { return 7, nil })
		if err != nil || v != 7 {
			t.Fatalf("retry after failure = %v, %v", v, err)
		}
	})

	t.Run("expired claim is taken over", func(t *testing.T) {
		tok, claimed, _, err := store.Begin(ctx, key("lease"), 300*time.Millisecond)
		if err != nil || !claimed {
			t.Fatalf("Begin = %v, %v", claimed, err)
		}
		if _, claimed, rec, _ := store.Begin(ctx, key("lease"), time.Second); claimed || rec.State != idempotency.InProgress {
			t.Fatalf("second Begin claimed = %v, state %q", claimed, rec.State)
		}
		advance(time.Second)
		tok2, claimed, _, err := store.Begin(ctx, key("lease"), time.Minute)
		if err != nil || !claimed {
			t.Fatalf("Begin after lease expiry = %v, %v", claimed, err)
		}
		if err := store.Complete(ctx, key("lease"), tok, []byte(`1`), time.Minute); !errors.Is(err, idempotency.ErrLeaseLost) {
			t.Errorf("stale owner Complete = %v", err)
		}
		if err := store.Release(ctx, key("lease"), tok); !errors.Is(err, idempotency.ErrLeaseLost) {
			t.Errorf("stale owner Release = %v", err)
		}
		if err := store.Complete(ctx, key("lease"), tok2, []byte(`2`), time.Minute); err != nil {
			t.Errorf("new owner Complete = %v", err)
		}
	})

	t.Run("results expire", func(t *testing.T) {
		var runs atomic.Int32
		fn := func(context.Context) (int, error) { return int(runs.Add(1)), nil }
		if _, err := idempotency.Do(ctx, store, key("ttl"), fn, idempotency.WithTTL(300*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		advance(time.Second)
		v, err := idempotency.Do(ctx, store, key("ttl"), fn)
		if err != nil || v != 2 {
			t.Fatalf("after TTL = %v, %v; want a new execution", v, err)
		}
	})

	t.Run("many keys", func(t *testing.T) {
		for i := range 50 {
			k := key("k" + strconv.Itoa(i))
			if _, err := idempotency.Do(ctx, store, k, func(context.Context) (int, error) { return i, nil }); err != nil {
				t.Fatal(err)
			}
		}
	})
}
