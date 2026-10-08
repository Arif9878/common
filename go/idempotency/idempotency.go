// Package idempotency prevents an operation from running more than once
// for the same key when it may be delivered more than once (Kafka
// redelivery, client retries with an Idempotency-Key header).
//
//	receipt, err := idempotency.Do(ctx, store, "charge:"+event.ID,
//		func(ctx context.Context) (Receipt, error) {
//			return payments.Charge(ctx, event)
//		})
//
// # How it works
//
// Do first claims the key in the store, as "in progress" with a lease
// (WithLease, 5 minutes by default). Then:
//
//   - The claim succeeds: fn runs. On success its result is stored as
//     "completed" for WithTTL (24h by default) and returned. On failure the
//     claim is released, so a later delivery can try again.
//   - The key is completed: fn does not run; the stored result is decoded
//     and returned. [DoOutcome] reports such calls as [Duplicate].
//   - The key is in progress elsewhere: Do returns [ErrInProgress] (kind
//     Unavailable, so retry policies and Kafka consumers try again later).
//
// # Guarantees and their limits
//
// This is not exactly-once processing. What it guarantees: while a key is
// claimed or completed, no second claim succeeds, so concurrent and
// repeated deliveries do not run fn again. What it cannot guarantee:
//
//   - A crash after fn's side effect but before the result is stored
//     leaves the key "in progress" until the lease expires; the next
//     delivery then runs fn again. fn must therefore be safe to repeat
//     after a crash, or its side effect and the completion must commit in
//     one transaction (pgstore.DoTx does that for PostgreSQL side effects).
//   - If fn runs longer than the lease, another delivery can claim the key
//     and run fn concurrently. The first run's completion then fails with
//     [ErrLeaseLost]. Choose a lease well above fn's worst-case duration.
//   - Results are kept for WithTTL. A delivery after that runs fn again.
//
// # Stores
//
// [NewMemoryStore] is for tests and single-instance tools. pgstore
// (PostgreSQL) and redisstore (Redis) share state across replicas.
package idempotency

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
)

var (
	// ErrInProgress is returned when another execution holds the key. Its
	// kind is Unavailable: try again later.
	ErrInProgress = errors.Unavailable.New("idempotency: operation in progress")
	// ErrLeaseLost is returned when the claim expired and was taken over
	// before the result could be stored. fn ran, but its result was not
	// recorded. Its kind is Conflict.
	ErrLeaseLost = errors.Conflict.New("idempotency: lease lost before completion")
)

// State is the state of a key in a store.
type State string

// States.
const (
	InProgress State = "in_progress"
	Completed  State = "completed"
)

// Record is what a store holds for a key.
type Record struct {
	State  State
	Result []byte // set when Completed
}

// Store keeps idempotency records. Implementations must make Begin atomic.
type Store interface {
	// Begin claims key for lease. It returns claimed=true and an ownership
	// token if the key was absent or its claim had expired; otherwise the
	// existing record.
	Begin(ctx context.Context, key string, lease time.Duration) (token string, claimed bool, existing Record, err error)
	// Complete stores result for key, kept for ttl, if token still owns
	// the key; otherwise it returns ErrLeaseLost.
	Complete(ctx context.Context, key, token string, result []byte, ttl time.Duration) error
	// Release deletes the claim on key if token still owns it.
	Release(ctx context.Context, key, token string) error
}

// Option configures [Do].
type Option func(*options)

type options struct {
	lease     time.Duration
	ttl       time.Duration
	meterProv metric.MeterProvider
	name      string
}

// WithLease sets how long a claim lasts before another delivery may take
// it over. The default is 5 minutes.
func WithLease(d time.Duration) Option { return func(o *options) { o.lease = d } }

// WithTTL sets how long completed results are kept. The default is 24h.
func WithTTL(d time.Duration) Option { return func(o *options) { o.ttl = d } }

// WithName labels metrics with an operation name (fixed, low cardinality).
func WithName(name string) Option { return func(o *options) { o.name = name } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProv = mp }
}

// Outcome describes what [DoOutcome] did.
type Outcome int

// Outcomes.
const (
	// Executed means fn ran and its result was stored.
	Executed Outcome = iota
	// Duplicate means the key was already completed; fn did not run.
	Duplicate
)

// Do runs fn at most once per key as described in the package
// documentation. T is stored as JSON.
func Do[T any](ctx context.Context, store Store, key string, fn func(ctx context.Context) (T, error), opts ...Option) (T, error) {
	v, _, err := DoOutcome(ctx, store, key, fn, opts...)
	return v, err
}

// DoOutcome is like [Do] and also reports whether fn ran.
func DoOutcome[T any](ctx context.Context, store Store, key string, fn func(ctx context.Context) (T, error), opts ...Option) (T, Outcome, error) {
	o := options{lease: 5 * time.Minute, ttl: 24 * time.Hour, meterProv: otel.GetMeterProvider(), name: "unnamed"}
	for _, opt := range opts {
		opt(&o)
	}
	var zero T
	// The SDK returns the same instrument for repeated registrations.
	calls, _ := o.meterProv.Meter("github.com/Arif9878/common/go/idempotency").Int64Counter("idempotency.calls",
		metric.WithDescription("Idempotent calls by outcome: executed, duplicate, in_progress, failed, store_error."))
	record := func(outcome string) {
		calls.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", o.name), attribute.String("outcome", outcome)))
	}

	token, claimed, existing, err := store.Begin(ctx, key, o.lease)
	if err != nil {
		record("store_error")
		return zero, Executed, errors.Join(errors.New("idempotency: begin"), err)
	}
	if !claimed {
		if existing.State != Completed {
			record("in_progress")
			return zero, Executed, ErrInProgress
		}
		var v T
		if err := json.Unmarshal(existing.Result, &v); err != nil {
			record("store_error")
			return zero, Duplicate, errors.Internal.Wrap(err, "idempotency: decode stored result")
		}
		record("duplicate")
		return v, Duplicate, nil
	}

	v, err := fn(ctx)
	if err != nil {
		record("failed")
		// Release so the next delivery can try again; the error from fn is
		// what matters to the caller.
		if rerr := store.Release(context.WithoutCancel(ctx), key, token); rerr != nil && !errors.Is(rerr, ErrLeaseLost) {
			err = errors.Join(err, rerr)
		}
		return zero, Executed, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		_ = store.Release(context.WithoutCancel(ctx), key, token)
		return zero, Executed, errors.Internal.Wrap(err, "idempotency: encode result")
	}
	if err := store.Complete(context.WithoutCancel(ctx), key, token, b, o.ttl); err != nil {
		record("store_error")
		return v, Executed, err
	}
	record("executed")
	return v, Executed, nil
}

// NewToken returns a random ownership token, for Store implementations.
func NewToken() string { return rand.Text() }

// MemoryStore is an in-process Store for tests and single-instance tools.
// It forgets everything on restart and is not shared between replicas.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]memEntry
	ops     int
}

type memEntry struct {
	rec     Record
	token   string
	expires time.Time
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{entries: map[string]memEntry{}} }

// Begin implements Store.
func (s *MemoryStore) Begin(_ context.Context, key string, lease time.Duration) (string, bool, Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.sweep(now)
	if e, ok := s.entries[key]; ok && now.Before(e.expires) {
		return "", false, e.rec, nil
	}
	tok := NewToken()
	s.entries[key] = memEntry{rec: Record{State: InProgress}, token: tok, expires: now.Add(lease)}
	return tok, true, Record{}, nil
}

// Complete implements Store.
func (s *MemoryStore) Complete(_ context.Context, key, token string, result []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || e.token != token || time.Now().After(e.expires) {
		return ErrLeaseLost
	}
	s.entries[key] = memEntry{rec: Record{State: Completed, Result: result}, token: token, expires: time.Now().Add(ttl)}
	return nil
}

// Release implements Store.
func (s *MemoryStore) Release(_ context.Context, key, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && e.token == token {
		delete(s.entries, key)
		return nil
	}
	return ErrLeaseLost
}

// sweep drops expired entries every 1024 operations, bounding memory to
// live entries.
func (s *MemoryStore) sweep(now time.Time) {
	if s.ops++; s.ops%1024 != 0 {
		return
	}
	for k, e := range s.entries {
		if now.After(e.expires) {
			delete(s.entries, k)
		}
	}
}
