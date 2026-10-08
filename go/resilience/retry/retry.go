// Package retry re-runs operations that fail with transient errors.
//
//	err := retry.Do(ctx, func(ctx context.Context) error {
//		return client.Send(ctx, msg)
//	}, retry.WithName("send"), retry.WithMaxAttempts(5))
//
// # Defaults
//
// 3 attempts in total, exponential backoff starting at 100ms and capped at
// 5s, with full jitter (each delay is uniformly random between 0 and the
// exponential value, which spreads out retries from many clients). There is
// no way to retry forever: WithMaxAttempts must be at least 1.
//
// # What is retried
//
// By default an error is retried only if [errors.IsRetryable] reports true
// (kinds Timeout, Unavailable and RateLimited). Unclassified errors are not
// retried. Wrap an error with [Permanent] to stop immediately regardless,
// or replace the rule with [WithRetryIf].
//
// Only retry operations that are safe to repeat. Retrying a non-idempotent
// write after a timeout can apply it twice, because the first attempt may
// have succeeded.
//
// # Delays
//
// If an error has a RetryAfter() time.Duration method (as rate-limit errors
// from this library do), the delay is at least that long. Do never sleeps
// past ctx's deadline or [WithMaxElapsed]: if the next delay would end
// after either, it gives up immediately instead of waiting to fail.
//
// # Result
//
// On success Do returns nil. Otherwise it returns the last operation error,
// wrapped with the attempt count; [errors.Is], [errors.As] and
// [errors.KindOf] see the original. If ctx ends while waiting, the error
// also wraps ctx's error.
package retry

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
)

// Option configures a [Policy].
type Option func(*Policy)

// Policy is a reusable retry configuration. Build it once with [New] for
// operations on hot paths; [Do] and [DoValue] build one per call. A Policy
// is safe for concurrent use.
type Policy struct {
	name        string
	maxAttempts int
	maxElapsed  time.Duration
	initial     time.Duration
	maxDelay    time.Duration
	jitter      bool
	retryIf     func(error) bool
	onRetry     func(attempt int, err error, delay time.Duration)
	meterProv   metric.MeterProvider
	attempts    metric.Int64Counter
	outcomes    map[string]metric.AddOption
}

// WithName names the operation for metrics. Use a fixed, low-cardinality
// name such as "inventory.reserve". The default is "unnamed".
func WithName(name string) Option {
	return func(p *Policy) { p.name = name }
}

// WithMaxAttempts sets the total number of attempts, including the first.
// Values below 1 are treated as 1 (no retries).
func WithMaxAttempts(n int) Option {
	return func(p *Policy) { p.maxAttempts = max(n, 1) }
}

// WithMaxElapsed stops retrying once d has passed since the first attempt
// started. Zero, the default, means no limit other than attempts and ctx.
func WithMaxElapsed(d time.Duration) Option {
	return func(p *Policy) { p.maxElapsed = d }
}

// WithExponentialBackoff sets the delay before the first retry and the cap
// on later delays, which double after every attempt.
func WithExponentialBackoff(initial, maxDelay time.Duration) Option {
	return func(p *Policy) { p.initial, p.maxDelay = initial, max(maxDelay, initial) }
}

// WithConstantBackoff waits d before every retry (before jitter).
func WithConstantBackoff(d time.Duration) Option {
	return WithExponentialBackoff(d, d)
}

// WithJitter enables full jitter. It is the default.
func WithJitter() Option {
	return func(p *Policy) { p.jitter = true }
}

// WithoutJitter uses the exact backoff delays, for deterministic tests.
func WithoutJitter() Option {
	return func(p *Policy) { p.jitter = false }
}

// WithRetryIf replaces the rule deciding whether an error is retried.
// [Permanent] errors are never retried, whatever fn returns.
func WithRetryIf(fn func(error) bool) Option {
	return func(p *Policy) { p.retryIf = fn }
}

// WithOnRetry calls fn before each retry with the attempt that failed
// (starting at 1), its error and the delay about to be waited.
func WithOnRetry(fn func(attempt int, err error, delay time.Duration)) Option {
	return func(p *Policy) { p.onRetry = fn }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(p *Policy) { p.meterProv = mp }
}

// Permanent marks err as not retryable. Do returns err itself (unwrapped
// from the marker). It returns nil if err is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Do calls op until it succeeds, returns a non-retryable error, or the
// policy built from opts gives up. See the package documentation.
func Do(ctx context.Context, op func(ctx context.Context) error, opts ...Option) error {
	return newPolicy(opts, false).Do(ctx, op)
}

// DoValue is like [Do] for operations that return a value. On failure it
// returns the zero T.
func DoValue[T any](ctx context.Context, op func(ctx context.Context) (T, error), opts ...Option) (T, error) {
	return DoValueWith(ctx, newPolicy(opts, false), op)
}

// Do calls op under policy p. See the package-level [Do].
func (p *Policy) Do(ctx context.Context, op func(ctx context.Context) error) error {
	_, err := DoValueWith(ctx, p, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, op(ctx)
	})
	return err
}

// DoValueWith calls op under policy p and returns its value.
func DoValueWith[T any](ctx context.Context, p *Policy, op func(ctx context.Context) (T, error)) (T, error) {
	start := time.Now()
	var zero T

	for attempt := 1; ; attempt++ {
		v, err := op(ctx)
		if err == nil {
			p.record(ctx, "success")
			return v, nil
		}

		if pe, ok := errors.AsType[*permanentError](err); ok {
			p.record(ctx, "permanent")
			return zero, pe.err
		}
		if !p.retryIf(err) {
			p.record(ctx, "permanent")
			return zero, err
		}
		if attempt >= p.maxAttempts {
			p.record(ctx, "exhausted")
			return zero, fmt.Errorf("retry %s: gave up after %d attempts: %w", p.name, attempt, err)
		}

		delay := p.delay(attempt, err)
		now := time.Now()
		if p.maxElapsed > 0 && now.Add(delay).Sub(start) > p.maxElapsed {
			p.record(ctx, "exhausted")
			return zero, fmt.Errorf("retry %s: gave up after %d attempts, max elapsed time reached: %w", p.name, attempt, err)
		}
		if deadline, ok := ctx.Deadline(); ok && now.Add(delay).After(deadline) {
			p.record(ctx, "deadline")
			return zero, fmt.Errorf("retry %s: gave up after %d attempts, context deadline before next attempt: %w",
				p.name, attempt, errors.Join(err, context.DeadlineExceeded))
		}

		p.record(ctx, "retry")
		if p.onRetry != nil {
			p.onRetry(attempt, err, delay)
		}
		if waitErr := sleep(ctx, delay); waitErr != nil {
			return zero, fmt.Errorf("retry %s: stopped after %d attempts: %w", p.name, attempt, errors.Join(err, waitErr))
		}
	}
}

// New returns a policy with the defaults described in the package
// documentation, changed by opts.
func New(opts ...Option) *Policy {
	return newPolicy(opts, true)
}

// newPolicy builds a policy. Long-lived policies precompute the metric
// attributes for every outcome; one-off policies build the one or two they
// use on demand.
func newPolicy(opts []Option, reusable bool) *Policy {
	p := &Policy{
		name:        "unnamed",
		maxAttempts: 3,
		initial:     100 * time.Millisecond,
		maxDelay:    5 * time.Second,
		jitter:      true,
		retryIf:     errors.IsRetryable,
		meterProv:   otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt(p)
	}
	p.attempts, _ = p.meterProv.Meter("github.com/Arif9878/common/go/resilience/retry").Int64Counter(
		"retry.attempts",
		metric.WithDescription("Attempts made by retry policies, by outcome."),
	)
	if reusable {
		p.outcomes = make(map[string]metric.AddOption, 5)
		for _, o := range []string{"success", "retry", "permanent", "exhausted", "deadline"} {
			p.outcomes[o] = p.outcomeAttrs(o)
		}
	}
	return p
}

func (p *Policy) outcomeAttrs(outcome string) metric.AddOption {
	return metric.WithAttributeSet(attribute.NewSet(
		attribute.String("operation", p.name), attribute.String("outcome", outcome)))
}

// record counts one attempt. Outcomes: success, retry (failed, will retry),
// permanent (failed, not retryable), exhausted (failed, attempts or elapsed
// time used up), deadline (failed, ctx deadline too close).
func (p *Policy) record(ctx context.Context, outcome string) {
	attrs, ok := p.outcomes[outcome]
	if !ok {
		attrs = p.outcomeAttrs(outcome)
	}
	p.attempts.Add(ctx, 1, attrs)
}

func (p *Policy) delay(attempt int, err error) time.Duration {
	d := p.initial
	for i := 1; i < attempt && d < p.maxDelay; i++ {
		d *= 2
	}
	d = min(d, p.maxDelay)
	if p.jitter && d > 0 {
		d = rand.N(d + 1) //nolint:gosec // jitter needs spread, not unpredictability
	}
	if ra, ok := errors.AsType[interface {
		error
		RetryAfter() time.Duration
	}](err); ok {
		d = max(d, ra.RetryAfter())
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
