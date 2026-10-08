// Package ratelimit limits the rate of operations within one process using
// a token bucket (golang.org/x/time/rate).
//
//	lim := ratelimit.New("partner-api", 50, 10) // 50/s, bursts of 10
//	if err := lim.Wait(ctx); err != nil {
//		return err // kind RateLimited, carries RetryAfter
//	}
//
// The bucket holds up to burst tokens and refills at the given rate per
// second; each operation takes one token. [Limiter.Allow] never waits.
// [Limiter.Wait] waits for a token, but never longer than the maximum wait
// (1s by default) or ctx's deadline: if the wait would be longer it fails
// immediately, without consuming a token, instead of sleeping and failing.
// A limiter with burst 1 spaces operations evenly, which gives the
// smoothing of a leaky bucket.
//
// Errors from Wait have kind RateLimited and a RetryAfter() time.Duration
// method, which the retry package uses as the minimum backoff and HTTP
// handlers can send as a Retry-After header.
//
// Limits are per process: with N replicas the total rate is N times the
// configured rate. Limits shared across replicas (distributed rate limiting)
// need a shared store and live in the datastore packages.
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/time/rate"

	"github.com/Arif9878/common/go/errors"
)

// ErrLimited is wrapped by every error returned for a rejected operation.
var ErrLimited = errors.RateLimited.New("rate limit exceeded")

// LimitedError is returned when an operation is rejected. It wraps
// [ErrLimited].
type LimitedError struct {
	Limiter string
	// After is how long until a token would be available, or 0 if the
	// request can never be satisfied.
	After time.Duration
}

func (e *LimitedError) Error() string {
	if e.After <= 0 {
		return fmt.Sprintf("ratelimit %s: %v", e.Limiter, ErrLimited)
	}
	return fmt.Sprintf("ratelimit %s: %v, retry after %v", e.Limiter, ErrLimited, e.After)
}

// Unwrap returns ErrLimited.
func (e *LimitedError) Unwrap() error { return ErrLimited }

// RetryAfter returns e.After.
func (e *LimitedError) RetryAfter() time.Duration { return e.After }

// Option configures [New].
type Option func(*Limiter)

// WithMaxWait sets the longest Wait will wait for a token. Zero makes Wait
// behave like Allow. The default is one second.
func WithMaxWait(d time.Duration) Option {
	return func(l *Limiter) { l.maxWait = max(d, 0) }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(l *Limiter) { l.meterProvider = mp }
}

// Limiter is a token-bucket rate limiter. It is safe for concurrent use.
type Limiter struct {
	name          string
	lim           *rate.Limiter
	maxWait       time.Duration
	meterProvider metric.MeterProvider

	requests metric.Int64Counter
	waited   metric.Float64Histogram
	attrs    map[string]metric.MeasurementOption // per outcome
}

// New returns a limiter allowing perSecond operations per second with bursts
// of up to burst. name identifies it in metrics; use a fixed, low-cardinality
// value. perSecond may be math.Inf(1) for no limit. burst below 1 is treated
// as 1.
func New(name string, perSecond float64, burst int, opts ...Option) *Limiter {
	limit := rate.Limit(perSecond)
	if math.IsInf(perSecond, 1) {
		limit = rate.Inf
	}
	l := &Limiter{
		name:          name,
		lim:           rate.NewLimiter(limit, max(burst, 1)),
		maxWait:       time.Second,
		meterProvider: otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt(l)
	}

	meter := l.meterProvider.Meter("github.com/Arif9878/common/go/resilience/ratelimit")
	l.requests, _ = meter.Int64Counter("ratelimit.requests",
		metric.WithDescription("Rate limiter decisions: allowed, delayed, limited or canceled."))
	l.waited, _ = meter.Float64Histogram("ratelimit.wait.duration", metric.WithUnit("s"),
		metric.WithDescription("Time spent waiting for a token, for delayed requests."))
	l.attrs = make(map[string]metric.MeasurementOption, 4)
	for _, outcome := range []string{"allowed", "delayed", "limited", "canceled"} {
		l.attrs[outcome] = metric.WithAttributeSet(attribute.NewSet(
			attribute.String("limiter", name), attribute.String("outcome", outcome)))
	}
	return l
}

// Allow reports whether an operation may happen now, consuming a token if
// so. It never waits.
func (l *Limiter) Allow() bool {
	ok := l.lim.Allow()
	if ok {
		l.requests.Add(context.Background(), 1, l.attrs["allowed"])
	} else {
		l.requests.Add(context.Background(), 1, l.attrs["limited"])
	}
	return ok
}

// Wait blocks until an operation may happen, then consumes a token. It
// returns a *[LimitedError] without waiting if the wait would exceed the
// maximum wait or ctx's deadline, and ctx's error (kind Canceled or Timeout)
// if ctx ends while waiting; in both cases no token is consumed.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		l.requests.Add(ctx, 1, l.attrs["canceled"])
		return err
	}

	now := time.Now()
	r := l.lim.ReserveN(now, 1)
	if !r.OK() {
		l.requests.Add(ctx, 1, l.attrs["limited"])
		return &LimitedError{Limiter: l.name}
	}
	delay := r.DelayFrom(now)
	if delay == 0 {
		l.requests.Add(ctx, 1, l.attrs["allowed"])
		return nil
	}

	deadline, hasDeadline := ctx.Deadline()
	if delay > l.maxWait || hasDeadline && now.Add(delay).After(deadline) {
		r.CancelAt(now)
		l.requests.Add(ctx, 1, l.attrs["limited"])
		return &LimitedError{Limiter: l.name, After: delay}
	}

	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		l.requests.Add(ctx, 1, l.attrs["delayed"])
		l.waited.Record(ctx, delay.Seconds(), l.attrs["delayed"])
		return nil
	case <-ctx.Done():
		r.Cancel()
		l.requests.Add(ctx, 1, l.attrs["canceled"])
		return ctx.Err()
	}
}

// Limit returns the current rate in operations per second, or +Inf for no
// limit.
func (l *Limiter) Limit() float64 {
	if limit := l.lim.Limit(); limit != rate.Inf {
		return float64(limit)
	}
	return math.Inf(1)
}

// Burst returns the bucket size.
func (l *Limiter) Burst() int { return l.lim.Burst() }

// SetLimit changes the rate, for example from dynamic configuration.
func (l *Limiter) SetLimit(perSecond float64) {
	limit := rate.Limit(perSecond)
	if math.IsInf(perSecond, 1) {
		limit = rate.Inf
	}
	l.lim.SetLimit(limit)
}
