// Package lock provides leases for distributed coordination: at most one
// holder per key at a time, for a limited time.
//
//	lease, err := lock.Acquire(ctx, locker, "reports:daily", time.Minute)
//	if err != nil {
//		return err // lock.ErrNotAcquired if ctx ended first
//	}
//	defer lease.Release(context.WithoutCancel(ctx))
//
// Implementations: lock/pglock (PostgreSQL) and lock/redislock (Redis).
//
// # What a lease does not guarantee
//
// A lease is a strong hint, not mutual exclusion, under real-world
// failures:
//
//   - Process pauses: a holder can stall (garbage collection, CPU
//     starvation, a VM migration) past the TTL, its lease expires, someone
//     else acquires it, and the first holder resumes believing it still
//     holds it. Nothing can prevent this on the holder's side.
//   - Network partitions: a holder that cannot reach the store cannot
//     extend or release; the lease expires underneath it.
//   - Clocks: expiry is measured by the store's clock only (pglock uses the
//     database's now(), redislock Redis's key expiry), so holders' clocks
//     do not matter, but a lease's remaining time as seen by the holder is
//     an estimate that ignores pauses and network delay.
//
// So use leases to avoid duplicate work (two replicas generating the same
// report), not to protect correctness. Where correctness depends on
// exclusivity, use the lease's fencing token: every acquisition gets a
// larger Fence than the previous one for that store, and the protected
// resource must reject writes carrying a fence lower than the highest it
// has seen, for example:
//
//	UPDATE reports SET body = $1, fence = $2 WHERE id = $3 AND fence < $2
//
// A paused former holder's late write then fails instead of overwriting
// the new holder's. Better still, do the work in a database transaction
// and need no lock at all.
//
// Extend long-running work before the TTL passes and stop if Extend
// fails with [ErrNotHeld]: the lease is gone.
package lock

import (
	"context"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
)

var (
	// ErrNotAcquired is returned when the key is held by someone else. Its
	// kind is Conflict.
	ErrNotAcquired = errors.Conflict.New("lock: held by another owner")
	// ErrNotHeld is returned by Release and Extend when the lease already
	// expired or was taken over. Its kind is Conflict.
	ErrNotHeld = errors.Conflict.New("lock: lease no longer held")
)

// Locker acquires leases from a store.
type Locker interface {
	// TryAcquire acquires key for ttl if it is free or its lease expired,
	// and returns ErrNotAcquired otherwise. It does not wait.
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (*Lease, error)
}

// Lease is an acquired lock.
type Lease struct {
	// Key is the locked key.
	Key string
	// Token identifies this holder.
	Token string
	// Fence increases with every acquisition in the store; see the package
	// documentation.
	Fence uint64
	// ExpiresAt is the holder's estimate of when the lease ends.
	ExpiresAt time.Time

	release func(ctx context.Context) error
	extend  func(ctx context.Context, ttl time.Duration) error
}

// NewLease is for Locker implementations.
func NewLease(key, token string, fence uint64, ttl time.Duration,
	release func(ctx context.Context) error,
	extend func(ctx context.Context, ttl time.Duration) error,
) *Lease {
	return &Lease{Key: key, Token: token, Fence: fence, ExpiresAt: time.Now().Add(ttl), release: release, extend: extend}
}

// Release gives the lease up. It returns ErrNotHeld if it had already
// expired or been taken over.
func (l *Lease) Release(ctx context.Context) error { return l.release(ctx) }

// Extend renews the lease for ttl from now. It returns ErrNotHeld if the
// lease was lost; the holder must then stop the protected work.
func (l *Lease) Extend(ctx context.Context, ttl time.Duration) error {
	start := time.Now()
	if err := l.extend(ctx, ttl); err != nil {
		return err
	}
	l.ExpiresAt = start.Add(ttl)
	return nil
}

// Option configures [Acquire].
type Option func(*options)

type options struct {
	minPoll, maxPoll time.Duration
	meterProv        metric.MeterProvider
}

// WithPollInterval sets the delays between attempts: starting at min,
// doubling up to max, jittered. The default is 50ms to 1s.
func WithPollInterval(minDelay, maxDelay time.Duration) Option {
	return func(o *options) { o.minPoll, o.maxPoll = minDelay, max(maxDelay, minDelay) }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProv = mp }
}

// Acquire waits until key can be acquired or ctx ends. It returns
// ErrNotAcquired (wrapped with ctx's error) if ctx ends first. Bound the
// wait with a context deadline.
func Acquire(ctx context.Context, l Locker, key string, ttl time.Duration, opts ...Option) (*Lease, error) {
	o := options{minPoll: 50 * time.Millisecond, maxPoll: time.Second, meterProv: otel.GetMeterProvider()}
	for _, opt := range opts {
		opt(&o)
	}
	meter := o.meterProv.Meter("github.com/Arif9878/common/go/lock")
	attempts, _ := meter.Int64Counter("lock.acquire", metric.WithDescription("Lock acquisitions by outcome: acquired, timeout, error."))
	wait, _ := meter.Float64Histogram("lock.acquire.wait", metric.WithUnit("s"), metric.WithDescription("Time spent waiting to acquire."))

	start := time.Now()
	done := func(outcome string) {
		a := metric.WithAttributes(attribute.String("outcome", outcome))
		attempts.Add(ctx, 1, a)
		wait.Record(ctx, time.Since(start).Seconds(), a)
	}
	delay := o.minPoll
	for {
		lease, err := l.TryAcquire(ctx, key, ttl)
		if err == nil {
			done("acquired")
			return lease, nil
		}
		if ctx.Err() != nil {
			// The deadline may hit inside TryAcquire; report it the same way.
			done("timeout")
			return nil, errors.Join(ErrNotAcquired, ctx.Err())
		}
		if !errors.Is(err, ErrNotAcquired) {
			done("error")
			return nil, err
		}
		t := time.NewTimer(delay/2 + rand.N(delay/2+1)) //nolint:gosec // jitter
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			done("timeout")
			return nil, errors.Join(ErrNotAcquired, ctx.Err())
		}
		delay = min(delay*2, o.maxPoll)
	}
}
