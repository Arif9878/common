// Package rotation keeps a credential-backed resource (a database pool, a
// Redis client, an API client, a TLS configuration) supplied with fresh
// credentials, replacing it without interrupting requests.
//
//	pools, err := rotation.New(ctx, "orders-db", rotation.Spec[*pgxpool.Pool]{
//		Fetch:    func(ctx context.Context) (secret.Secret, error) { return vault.Get(ctx, "database/creds/orders") },
//		Build:    func(ctx context.Context, s secret.Secret) (*pgxpool.Pool, error) { return connect(ctx, s) },
//		Validate: func(ctx context.Context, p *pgxpool.Pool) error { return p.Ping(ctx) },
//		Close: func(ctx context.Context, p *pgxpool.Pool, s secret.Secret) error {
//			p.Close()
//			return vault.Revoke(ctx, s.LeaseID)
//		},
//	})
//	shutdown.Register(graceful.CloseDeps, "orders-db", pools.Close)
//
//	pool, release, err := pools.Acquire()
//	defer release()
//
// The package does not depend on any secret provider; Fetch can call
// Vault, a cloud secret manager or a file.
//
// # Rotation flow
//
//	fetch credential -> build resource -> validate
//	  validation fails: close the new resource, keep the old one, retry later
//	  validation succeeds: atomically swap; new calls get the new resource;
//	    the old one is drained (in-flight users finish), then closed/revoked
//
// [New] performs the first rotation synchronously and fails if it fails, so
// a service never starts without a working resource. After that, a
// background loop rotates when 70% (by default) of the credential's
// lifetime has passed, with a little jitter so replicas do not rotate at
// the same moment. Credentials without an expiry are refetched every
// WithRefreshInterval, if set; an unchanged Version is a no-op.
//
// # Failures
//
// A failed rotation never affects the current resource. The loop retries
// with exponential backoff and jitter (1s doubling to 1m), indefinitely,
// because giving up would let the credential expire. Every failure is
// logged and counted; once the current credential has expired, failures
// are logged at error level. Watch the rotation.credential.ttl metric.
//
// Only one rotation runs at a time; concurrent [Rotator.Rotate] calls
// share its result.
//
// # Using the resource
//
// [Rotator.Acquire] returns the current resource and a release function;
// the old resource is closed only after every acquired use is released
// (or WithDrainTimeout, 30s by default, passes). [Rotator.Current] skips
// the reference count. It suits resources whose Close waits for their own
// users, like pgxpool.Pool, but only if the caller handles the window in
// which it obtained the old resource just before it was closed: pgxpool
// then fails the acquisition with a closed-pool error before any I/O, and
// datastore/postgres retries it on the new pool.
package rotation

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/secret"
)

// Spec describes how to obtain and manage the resource.
type Spec[R any] struct {
	// Fetch obtains a credential. Required.
	Fetch func(ctx context.Context) (secret.Secret, error)
	// Build creates a resource from a credential. Required.
	Build func(ctx context.Context, s secret.Secret) (R, error)
	// Validate checks a newly built resource before it is used, for example
	// with a ping. Optional.
	Validate func(ctx context.Context, r R) error
	// Close releases a resource that is no longer used, and may revoke its
	// credential. It is called for replaced resources after draining, for
	// resources that failed validation, and for the current resource on
	// Rotator.Close. Optional.
	Close func(ctx context.Context, r R, s secret.Secret) error
}

// ErrClosed is returned by Acquire and Rotate after Close. Its kind is
// Unavailable.
var ErrClosed = errors.Unavailable.New("rotation: closed")

// Option configures [New].
type Option func(*options)

type options struct {
	renewAt         float64
	jitter          float64
	refreshInterval time.Duration
	minBackoff      time.Duration
	maxBackoff      time.Duration
	opTimeout       time.Duration
	drainTimeout    time.Duration
	logger          *slog.Logger
	meterProvider   metric.MeterProvider
}

// WithRenewAt rotates when fraction (0 to 1) of a credential's lifetime has
// passed, plus or minus jitter (also a fraction of the lifetime). The
// default is 0.7 with 0.05 jitter.
func WithRenewAt(fraction, jitter float64) Option {
	return func(o *options) { o.renewAt, o.jitter = fraction, jitter }
}

// WithRefreshInterval refetches credentials without an expiry every d. The
// default, 0, never refetches them except through Rotate.
func WithRefreshInterval(d time.Duration) Option {
	return func(o *options) { o.refreshInterval = d }
}

// WithBackoff sets the retry delays after a failed rotation. The default is
// 1s doubling to 1m.
func WithBackoff(minDelay, maxDelay time.Duration) Option {
	return func(o *options) { o.minBackoff, o.maxBackoff = minDelay, max(maxDelay, minDelay) }
}

// WithOperationTimeout bounds each Fetch, Build, Validate and Close call.
// The default is 30 seconds.
func WithOperationTimeout(d time.Duration) Option {
	return func(o *options) { o.opTimeout = d }
}

// WithDrainTimeout bounds how long a replaced resource waits for acquired
// uses to be released before it is closed anyway. The default is 30s.
func WithDrainTimeout(d time.Duration) Option {
	return func(o *options) { o.drainTimeout = d }
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProvider = mp }
}

// retiredBit marks an entry that has been replaced; the low bits count
// acquired uses.
const retiredBit = int64(1) << 62

type entry[R any] struct {
	res      R
	sec      secret.Secret
	issuedAt time.Time

	refs    atomic.Int64
	drained chan struct{} // closed when retired and refs reach zero
	once    sync.Once
}

func (e *entry[R]) release() {
	if e.refs.Add(-1) == retiredBit {
		e.once.Do(func() { close(e.drained) })
	}
}

// retire marks e replaced; acquirers then load the new entry.
func (e *entry[R]) retire() {
	if e.refs.Or(retiredBit) == 0 {
		e.once.Do(func() { close(e.drained) })
	}
}

// Rotator owns the current resource and rotates it. It is safe for
// concurrent use.
type Rotator[R any] struct {
	name string
	spec Spec[R]
	o    options

	cur atomic.Pointer[entry[R]]

	rotateMu sync.Mutex
	inflight *call // non-nil while a rotation runs; guarded by rotateMu

	ctx     context.Context
	stop    context.CancelFunc
	kick    chan struct{} // reschedules the loop after a manual rotation
	loopWG  sync.WaitGroup
	drainWG sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool

	attempts metric.Int64Counter
	duration metric.Float64Histogram
	outcomes map[string]metric.AddOption
	attrs    metric.MeasurementOption
	reg      metric.Registration
}

type call struct {
	done chan struct{}
	err  error
}

// New builds the first resource and starts background rotation. It returns
// an error if Fetch or Build are missing or the first rotation fails.
func New[R any](ctx context.Context, name string, spec Spec[R], opts ...Option) (*Rotator[R], error) {
	if spec.Fetch == nil || spec.Build == nil {
		return nil, errors.InvalidArgument.New("rotation: Spec.Fetch and Spec.Build are required")
	}
	o := options{
		renewAt:       0.7,
		jitter:        0.05,
		minBackoff:    time.Second,
		maxBackoff:    time.Minute,
		opTimeout:     30 * time.Second,
		drainTimeout:  30 * time.Second,
		logger:        slog.Default(),
		meterProvider: otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt(&o)
	}

	r := &Rotator[R]{name: name, spec: spec, o: o, kick: make(chan struct{}, 1)}
	r.initMetrics()

	if err := r.rotate(ctx); err != nil {
		r.unregister()
		return nil, err
	}

	r.ctx, r.stop = context.WithCancel(context.Background()) //nolint:gosec // r.stop is called by Close
	r.loopWG.Add(1)
	go r.loop()
	return r, nil
}

func (r *Rotator[R]) initMetrics() {
	meter := r.o.meterProvider.Meter("github.com/Arif9878/common/go/secret/rotation")
	res := attribute.String("resource", r.name)
	r.attrs = metric.WithAttributeSet(attribute.NewSet(res))
	r.attempts, _ = meter.Int64Counter("rotation.attempts",
		metric.WithDescription("Rotation attempts by outcome: success, unchanged, fetch_error, build_error, validation_error."))
	r.duration, _ = meter.Float64Histogram("rotation.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of rotation attempts."))
	r.outcomes = map[string]metric.AddOption{}
	for _, o := range []string{"success", "unchanged", "fetch_error", "build_error", "validation_error"} {
		r.outcomes[o] = metric.WithAttributeSet(attribute.NewSet(res, attribute.String("outcome", o)))
	}
	ttl, _ := meter.Float64ObservableGauge("rotation.credential.ttl", metric.WithUnit("s"),
		metric.WithDescription("Seconds until the current credential expires; negative once expired. Absent for credentials without expiry."))
	r.reg, _ = meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		if e := r.cur.Load(); e != nil && !e.sec.ExpiresAt.IsZero() {
			obs.ObserveFloat64(ttl, time.Until(e.sec.ExpiresAt).Seconds(), r.attrs)
		}
		return nil
	}, ttl)
}

func (r *Rotator[R]) unregister() {
	if r.reg != nil {
		_ = r.reg.Unregister()
	}
}

// Acquire returns the current resource and a function that must be called
// when the caller is done with it. The resource is not closed before
// release is called (or the drain timeout passes). It returns [ErrClosed]
// after Close.
func (r *Rotator[R]) Acquire() (R, func(), error) {
	for {
		if r.closed.Load() {
			var zero R
			return zero, func() {}, ErrClosed
		}
		e := r.cur.Load()
		n := e.refs.Load()
		if n&retiredBit != 0 {
			continue // replaced meanwhile: load the new entry
		}
		if e.refs.CompareAndSwap(n, n+1) {
			var once sync.Once
			return e.res, func() { once.Do(e.release) }, nil
		}
	}
}

// Current returns the current resource without reference counting, for
// resources whose Close waits for their own users. The resource may be
// closed right after Current returns; see the package documentation. Do
// not keep it beyond one operation.
func (r *Rotator[R]) Current() R {
	return r.cur.Load().res
}

// Secret returns the credential of the current resource.
func (r *Rotator[R]) Secret() secret.Secret {
	return r.cur.Load().sec
}

// Rotate rotates now and returns the result. If a rotation is already
// running, it waits for that one instead of starting another.
func (r *Rotator[R]) Rotate(ctx context.Context) error {
	if r.closed.Load() {
		return ErrClosed
	}
	err := r.rotate(ctx)
	select {
	case r.kick <- struct{}{}:
	default:
	}
	return err
}

func (r *Rotator[R]) rotate(ctx context.Context) error {
	r.rotateMu.Lock()
	if r.closed.Load() {
		r.rotateMu.Unlock()
		return ErrClosed
	}
	if c := r.inflight; c != nil {
		r.rotateMu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	r.inflight = c
	r.rotateMu.Unlock()

	// The rotation runs to completion even if ctx is canceled: abandoning
	// it half-way could leak a built resource or an issued credential.
	c.err = r.doRotate(context.WithoutCancel(ctx))

	r.rotateMu.Lock()
	r.inflight = nil
	r.rotateMu.Unlock()
	close(c.done)
	return c.err
}

func (r *Rotator[R]) doRotate(ctx context.Context) error {
	start := time.Now()
	outcome := "success"
	defer func() {
		r.attempts.Add(ctx, 1, r.outcomes[outcome])
		r.duration.Record(ctx, time.Since(start).Seconds(), r.attrs)
	}()

	sec, err := withTimeout(ctx, r.o.opTimeout, r.spec.Fetch)
	if err != nil {
		outcome = "fetch_error"
		return r.fail(outcome, err)
	}
	old := r.cur.Load()
	if old != nil && sec.Version != "" && sec.Version == old.sec.Version {
		outcome = "unchanged"
		return nil
	}

	res, err := withTimeout(ctx, r.o.opTimeout, func(ctx context.Context) (R, error) { return r.spec.Build(ctx, sec) })
	if err != nil {
		outcome = "build_error"
		return r.fail(outcome, err)
	}
	if r.spec.Validate != nil {
		if _, err := withTimeout(ctx, r.o.opTimeout, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, r.spec.Validate(ctx, res)
		}); err != nil {
			outcome = "validation_error"
			r.closeResource(res, sec, "validation failed")
			return r.fail(outcome, err)
		}
	}

	r.cur.Store(&entry[R]{res: res, sec: sec, issuedAt: time.Now(), drained: make(chan struct{})})
	attrs := []any{"resource", r.name}
	if sec.Version != "" {
		attrs = append(attrs, "version", sec.Version)
	}
	if ttl := sec.TTL(); ttl != 0 {
		attrs = append(attrs, "expires_in", ttl.Round(time.Second).String())
	}
	r.o.logger.Info("credential rotated", attrs...)

	if old != nil {
		old.retire()
		r.drainWG.Add(1)
		go func() {
			defer r.drainWG.Done()
			r.drainAndClose(old)
		}()
	}
	return nil
}

func (r *Rotator[R]) fail(outcome string, err error) error {
	level := slog.LevelWarn
	if e := r.cur.Load(); e != nil && !e.sec.ExpiresAt.IsZero() && time.Now().After(e.sec.ExpiresAt) {
		level = slog.LevelError // the resource in use has an expired credential
	}
	r.o.logger.Log(context.Background(), level, "credential rotation failed",
		"resource", r.name, "outcome", outcome, logging.Err(err))
	return errors.Join(errors.New("rotation "+r.name+": "+outcome), err)
}

func (r *Rotator[R]) drainAndClose(e *entry[R]) {
	t := time.NewTimer(r.o.drainTimeout)
	defer t.Stop()
	select {
	case <-e.drained:
	case <-t.C:
		r.o.logger.Warn("closing replaced resource with uses still acquired",
			"resource", r.name, "acquired", e.refs.Load()&^retiredBit)
	}
	r.closeResource(e.res, e.sec, "replaced")
}

func (r *Rotator[R]) closeResource(res R, sec secret.Secret, reason string) {
	if r.spec.Close == nil {
		return
	}
	_, err := withTimeout(context.Background(), r.o.opTimeout, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, r.spec.Close(ctx, res, sec)
	})
	if err != nil {
		r.o.logger.Warn("closing resource failed", "resource", r.name, "reason", reason, logging.Err(err))
	}
}

func (r *Rotator[R]) loop() {
	defer r.loopWG.Done()
	failures := 0
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		wait, scheduled := r.nextDelay(failures)
		var fire <-chan time.Time
		if scheduled {
			timer.Reset(wait)
			fire = timer.C
		}
		select {
		case <-r.ctx.Done():
			return
		case <-r.kick:
			timer.Stop()
			failures = 0
			continue
		case <-fire:
		}
		if err := r.rotate(r.ctx); err != nil {
			failures++
		} else {
			failures = 0
		}
	}
}

// nextDelay returns how long to wait before the next rotation, and false
// if no rotation is scheduled.
func (r *Rotator[R]) nextDelay(failures int) (time.Duration, bool) {
	if failures > 0 {
		d := r.o.minBackoff
		for i := 1; i < failures && d < r.o.maxBackoff; i++ {
			d *= 2
		}
		d = min(d, r.o.maxBackoff)
		return d/2 + rand.N(d/2+1), true //nolint:gosec // jitter
	}
	e := r.cur.Load()
	if e.sec.ExpiresAt.IsZero() {
		return r.o.refreshInterval, r.o.refreshInterval > 0
	}
	lifetime := e.sec.ExpiresAt.Sub(e.issuedAt)
	frac := r.o.renewAt
	if r.o.jitter > 0 {
		frac += (rand.Float64()*2 - 1) * r.o.jitter //nolint:gosec // jitter
	}
	frac = min(max(frac, 0), 1)
	at := e.issuedAt.Add(time.Duration(float64(lifetime) * frac))
	return max(time.Until(at), 0), true
}

// Close stops rotation, waits for replaced resources to finish draining,
// then drains and closes the current one, all bounded by ctx. Acquire and
// Rotate fail with [ErrClosed] afterwards. Its signature matches
// graceful.Hook; repeated calls return the first result.
func (r *Rotator[R]) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.rotateMu.Lock()
		r.closed.Store(true) // under rotateMu: no rotation can start after this
		inflight := r.inflight
		r.rotateMu.Unlock()
		if inflight != nil {
			<-inflight.done // a rotation already running may still swap; wait for it
		}
		r.stop()
		r.loopWG.Wait()

		e := r.cur.Load()
		e.retire()
		done := make(chan struct{})
		go func() {
			r.drainWG.Wait()
			select {
			case <-e.drained:
			case <-ctx.Done():
			}
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
		}
		r.closeResource(e.res, e.sec, "shutdown")
		r.unregister()
		if err := ctx.Err(); err != nil {
			r.closeErr = errors.Timeout.Wrap(err, "rotation "+r.name+": closed before all uses were released")
		}
	})
	return r.closeErr
}

func withTimeout[T any](ctx context.Context, d time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return fn(ctx)
}
