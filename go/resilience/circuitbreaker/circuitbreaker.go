// Package circuitbreaker stops calling a dependency that keeps failing, so
// callers fail fast instead of piling up on timeouts, and the dependency
// gets room to recover.
//
//	cb := circuitbreaker.New("payments-api")
//	resp, err := circuitbreaker.Execute(ctx, cb, func(ctx context.Context) (*Response, error) {
//		return client.Charge(ctx, req)
//	})
//	if errors.Is(err, circuitbreaker.ErrOpen) { ... fall back ... }
//
// # States
//
//	CLOSED --(failure threshold)--> OPEN --(cooldown)--> HALF-OPEN
//	HALF-OPEN --(probe succeeds)--> CLOSED
//	HALF-OPEN --(probe fails)-----> OPEN
//
// Closed: calls pass and outcomes are counted. By default 5 consecutive
// failures open the breaker; [WithFailureRatio] trips on a failure ratio
// over a rolling window instead. Open: calls fail immediately with
// [ErrOpen] for the cooldown (10s by default). Half-open: a limited number
// of probe calls pass (1 by default); others get ErrOpen.
//
// # What counts as a failure
//
// By default, errors of kind Timeout, Unavailable, RateLimited, Internal or
// Unknown (see the errors package) are failures: they suggest the
// dependency is unhealthy. Client errors (InvalidArgument, NotFound,
// Conflict, Unauthorized, Forbidden) count as successes, since the
// dependency answered. Canceled calls, where the caller gave up, are not
// counted at all. Replace the rule with [WithIsFailure].
//
// # Composition
//
// The breaker never retries. Compose retry explicitly; usually the breaker
// goes inside the retry, so each attempt is checked and an open breaker
// (kind Unavailable, retryable) is retried after backoff:
//
//	err := retry.Do(ctx, func(ctx context.Context) error {
//		return cb.Execute(ctx, call)
//	})
//
// Use one Breaker per dependency (or per dependency and endpoint), shared by
// all goroutines; a Breaker is safe for concurrent use.
package circuitbreaker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
)

// State is the state of a breaker.
type State int

// States. The values are exported as the circuitbreaker.state metric.
const (
	Closed   State = 0
	HalfOpen State = 1
	Open     State = 2
)

// String returns "closed", "half_open" or "open".
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half_open"
	case Open:
		return "open"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// ErrOpen is returned, wrapped with the breaker name, when a call is
// rejected because the breaker is open or half-open probes are in use. Its
// kind is Unavailable.
var ErrOpen = errors.Unavailable.New("circuit breaker is open")

// Option configures [New].
type Option func(*config)

type config struct {
	consecutive   uint32
	ratio         float64
	minRequests   uint32
	window        time.Duration
	cooldown      time.Duration
	halfOpen      uint32
	isFailure     func(error) bool
	onStateChange func(name string, from, to State)
	logger        *slog.Logger
	meterProvider metric.MeterProvider
}

// WithConsecutiveFailures opens the breaker after n consecutive failures.
// It is the default trip rule, with n = 5.
func WithConsecutiveFailures(n uint32) Option {
	return func(c *config) { c.consecutive, c.ratio = max(n, 1), 0 }
}

// WithFailureRatio opens the breaker when, within the window, at least
// minRequests calls were counted and the fraction that failed is at least
// ratio. Combine it with [WithWindow]; without a window, counts accumulate
// for as long as the breaker stays closed.
func WithFailureRatio(ratio float64, minRequests uint32) Option {
	return func(c *config) { c.ratio, c.minRequests, c.consecutive = ratio, max(minRequests, 1), 0 }
}

// WithWindow counts outcomes over a rolling window of length d (in ten
// buckets) while closed. The default, 0, never resets counts while closed,
// which is right for consecutive-failure tripping.
func WithWindow(d time.Duration) Option {
	return func(c *config) { c.window = d }
}

// WithCooldown sets how long the breaker stays open before allowing
// half-open probes. The default is 10 seconds.
func WithCooldown(d time.Duration) Option {
	return func(c *config) { c.cooldown = d }
}

// WithHalfOpenRequests sets how many probe calls may run in the half-open
// state. All of them must succeed to close the breaker. The default is 1.
func WithHalfOpenRequests(n uint32) Option {
	return func(c *config) { c.halfOpen = max(n, 1) }
}

// WithIsFailure replaces the rule deciding whether a non-nil error is a
// failure. Errors of kind Canceled are never counted, whatever fn returns.
func WithIsFailure(fn func(error) bool) Option {
	return func(c *config) { c.isFailure = fn }
}

// WithOnStateChange calls fn on every state transition. fn runs while the
// breaker holds its lock: it must be fast and must not call the breaker.
func WithOnStateChange(fn func(name string, from, to State)) Option {
	return func(c *config) { c.onStateChange = fn }
}

// WithLogger sets the logger for state transitions. The default is
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// DefaultIsFailure is the default failure rule; see the package
// documentation.
func DefaultIsFailure(err error) bool {
	switch errors.KindOf(err) {
	case errors.InvalidArgument, errors.NotFound, errors.Conflict, errors.Unauthorized, errors.Forbidden:
		return false
	default:
		return true
	}
}

// Breaker is a circuit breaker. It is safe for concurrent use.
type Breaker struct {
	name      string
	isFailure func(error) bool
	cb        *gobreaker.TwoStepCircuitBreaker[struct{}]
	requests  metric.Int64Counter
	attrs     attribute.Set
	outcomes  map[string]metric.AddOption
}

// New returns a closed breaker. name identifies the dependency in logs and
// metrics; use a fixed, low-cardinality value.
func New(name string, opts ...Option) *Breaker {
	c := config{
		consecutive:   5,
		cooldown:      10 * time.Second,
		halfOpen:      1,
		isFailure:     DefaultIsFailure,
		logger:        slog.Default(),
		meterProvider: otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt(&c)
	}

	meter := c.meterProvider.Meter("github.com/Arif9878/common/go/resilience/circuitbreaker")
	b := &Breaker{name: name, isFailure: c.isFailure, attrs: attribute.NewSet(attribute.String("breaker", name))}
	b.requests, _ = meter.Int64Counter("circuitbreaker.requests",
		metric.WithDescription("Calls through circuit breakers, by outcome."))
	b.outcomes = make(map[string]metric.AddOption, 5)
	for _, o := range []string{"success", "failure", "client_error", "excluded", "rejected"} {
		b.outcomes[o] = metric.WithAttributeSet(attribute.NewSet(
			attribute.String("breaker", name), attribute.String("outcome", o)))
	}
	stateGauge, _ := meter.Int64Gauge("circuitbreaker.state",
		metric.WithDescription("Circuit breaker state: 0 closed, 1 half-open, 2 open."))
	stateGauge.Record(context.Background(), int64(Closed), metric.WithAttributeSet(b.attrs))

	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: c.halfOpen,
		Timeout:     c.cooldown,
		IsExcluded: func(err error) bool {
			return errors.KindOf(err) == errors.Canceled
		},
		IsSuccessful: func(err error) bool {
			return err == nil || !c.isFailure(err)
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			f, t := convert(from), convert(to)
			stateGauge.Record(context.Background(), int64(t), metric.WithAttributeSet(b.attrs))
			level := slog.LevelInfo
			if t == Open {
				level = slog.LevelWarn
			}
			c.logger.Log(context.Background(), level, "circuit breaker state changed",
				"breaker", name, "from", f.String(), "to", t.String())
			if c.onStateChange != nil {
				c.onStateChange(name, f, t)
			}
		},
	}
	if c.window > 0 {
		settings.Interval = c.window
		settings.BucketPeriod = c.window / 10
	}
	if c.ratio > 0 {
		ratio, minReq := c.ratio, c.minRequests
		settings.ReadyToTrip = func(counts gobreaker.Counts) bool {
			valid := counts.TotalSuccesses + counts.TotalFailures
			return valid >= minReq && float64(counts.TotalFailures)/float64(valid) >= ratio
		}
	} else {
		threshold := c.consecutive
		settings.ReadyToTrip = func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= threshold
		}
	}

	b.cb = gobreaker.NewTwoStepCircuitBreaker[struct{}](settings)
	return b
}

func convert(s gobreaker.State) State {
	switch s {
	case gobreaker.StateHalfOpen:
		return HalfOpen
	case gobreaker.StateOpen:
		return Open
	default:
		return Closed
	}
}

// Name returns the breaker name.
func (b *Breaker) Name() string { return b.name }

// State returns the current state.
func (b *Breaker) State() State { return convert(b.cb.State()) }

// Execute calls op if the breaker allows it and records the outcome. The
// circuitbreaker.requests metric counts calls by outcome: success, failure,
// client_error (an error the breaker counts as a success), excluded
// (canceled) and rejected. If ctx
// is already done, it returns ctx's error without calling op or counting
// anything. A panic in op is recorded as a failure and re-raised.
func (b *Breaker) Execute(ctx context.Context, op func(ctx context.Context) error) error {
	_, err := Execute(ctx, b, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, op(ctx)
	})
	return err
}

// Execute is [Breaker.Execute] for operations that return a value.
func Execute[T any](ctx context.Context, b *Breaker, op func(ctx context.Context) (T, error)) (_ T, err error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, errors.Canceled.Wrap(err, "circuitbreaker "+b.name)
	}

	done, allowErr := b.cb.Allow()
	if allowErr != nil {
		b.record(ctx, "rejected")
		return zero, fmt.Errorf("circuitbreaker %s: %w", b.name, ErrOpen)
	}

	defer func() {
		if r := recover(); r != nil {
			done(errors.Internal.Errorf("panic: %v", r))
			b.record(ctx, "failure")
			panic(r)
		}
	}()

	v, err := op(ctx)
	done(err)
	switch {
	case err == nil:
		b.record(ctx, "success")
	case errors.KindOf(err) == errors.Canceled:
		b.record(ctx, "excluded")
	case b.isFailure(err):
		b.record(ctx, "failure")
	default:
		b.record(ctx, "client_error") // counted as a success by the breaker
	}
	return v, err
}

func (b *Breaker) record(ctx context.Context, outcome string) {
	b.requests.Add(ctx, 1, b.outcomes[outcome])
}
