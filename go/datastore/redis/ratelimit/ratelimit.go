// Package ratelimit limits operations per key across every replica, with
// the state in Redis: per tenant, per API key, per client IP.
//
//	perTenant := ratelimit.New(rdb, "orders-api", 100, 20) // 100/s per key, bursts of 20
//	if err := perTenant.Take(ctx, tenantID); err != nil {
//		return err // kind RateLimited with RetryAfter: 429 and Retry-After over HTTP
//	}
//
// For HTTP, [Middleware] does this per request:
//
//	mux.Handle("/", ratelimit.Middleware(perTenant, func(r *http.Request) (string, bool) {
//		return r.Header.Get("X-Tenant-ID"), true
//	})(api))
//
// # Algorithm
//
// The generic cell rate algorithm (GCRA), the token bucket expressed as one
// timestamp per key: each key allows bursts of up to burst operations and
// then perSecond operations per second. One Lua script reads and updates
// the key atomically using Redis's clock, so replicas share each key's
// budget exactly, whatever their own clocks say. Keys expire once their
// bucket is full again, so idle keys cost nothing.
//
// The in-process [github.com/Arif9878/common/go/resilience/ratelimit] is
// cheaper when a limit does not have to be shared, such as protecting a
// partner API from one replica.
//
// # Redis failures
//
// Each Redis call is bounded by a short timeout (100ms by default). If it
// fails, the operation is allowed (fail open) and the failure logged and
// counted, so a Redis outage does not take the API down with it.
// [WithFailClosed] rejects instead, for limits that protect something
// that must not be overloaded.
package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/resilience/ratelimit"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

// gcra takes ARGV[3] cells from the bucket at KEYS[1], which holds the
// theoretical arrival time (TAT) in seconds since an epoch, with
// ARGV[1] = emission interval (seconds per cell) and ARGV[2] = burst.
// It returns {allowed (0/1), remaining cells, retry after, reset after},
// times in seconds as strings (Redis truncates Lua numbers to integers).
var gcra = goredis.NewScript(`
local interval = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])

local t = redis.call("TIME")
local now = (tonumber(t[1]) - 1700000000) + tonumber(t[2]) / 1000000

local tat = tonumber(redis.call("GET", KEYS[1])) or now
tat = math.max(tat, now)
local new_tat = tat + interval * cost
local allow_at = new_tat - interval * burst
local diff = now - allow_at

if diff < 0 then
	return {0, 0, tostring(-diff), tostring(tat - now)}
end
local reset_after = new_tat - now
redis.call("SET", KEYS[1], tostring(new_tat), "PX", math.ceil(reset_after * 1000))
return {1, math.floor(diff / interval), "0", tostring(reset_after)}
`)

// Result is the outcome of one check.
type Result struct {
	// Allowed reports whether the operation may happen. When it is false,
	// nothing was taken from the bucket.
	Allowed bool
	// Remaining is how many more operations the key allows right now.
	Remaining int
	// RetryAfter is how long until the operation would be allowed, when it
	// is not.
	RetryAfter time.Duration
	// ResetAfter is how long until the key's bucket is full again.
	ResetAfter time.Duration
}

// Option configures [New].
type Option func(*Limiter)

// WithPrefix sets the key prefix. The default is "ratelimit:"; keys are
// "<prefix><name>:<key>".
func WithPrefix(p string) Option { return func(l *Limiter) { l.prefix = p } }

// WithRedisTimeout bounds each Redis call. The default is 100ms.
func WithRedisTimeout(d time.Duration) Option { return func(l *Limiter) { l.timeout = d } }

// WithFailClosed rejects operations when Redis fails, with an error of
// kind Unavailable, instead of allowing them.
func WithFailClosed() Option { return func(l *Limiter) { l.failClosed = true } }

// WithLogger sets the logger for Redis failures. The default is
// slog.Default().
func WithLogger(lg *slog.Logger) Option { return func(l *Limiter) { l.logger = lg } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(l *Limiter) { l.meterProv = mp }
}

// Limiter limits operations per key. It is safe for concurrent use.
type Limiter struct {
	rdb        goredis.Scripter
	name       string
	interval   float64 // seconds per operation
	burst      int
	prefix     string
	timeout    time.Duration
	failClosed bool
	closed     bool // perSecond <= 0: nothing is allowed
	logger     *slog.Logger
	meterProv  metric.MeterProvider

	requests metric.Int64Counter
	attrs    map[string]metric.MeasurementOption
}

// New returns a limiter allowing each key perSecond operations per second,
// with bursts of up to burst (at least 1). name identifies the limit in
// keys and metrics; use a fixed, low-cardinality value. A perSecond of 0
// or less allows nothing.
func New(rdb goredis.Scripter, name string, perSecond float64, burst int, opts ...Option) *Limiter {
	l := &Limiter{
		rdb: rdb, name: name, burst: max(burst, 1),
		interval:  1 / perSecond,
		prefix:    "ratelimit:",
		timeout:   100 * time.Millisecond,
		logger:    slog.Default(),
		meterProv: otel.GetMeterProvider(),
	}
	if !(perSecond > 0) { // also NaN
		l.closed = true
	}
	for _, opt := range opts {
		opt(l)
	}
	meter := l.meterProv.Meter("github.com/Arif9878/common/go/datastore/redis/ratelimit")
	l.requests, _ = meter.Int64Counter("ratelimit.requests",
		metric.WithDescription("Rate limiter decisions: allowed, limited, or error (Redis failed; allowed unless fail-closed)."))
	l.attrs = map[string]metric.MeasurementOption{}
	for _, o := range []string{"allowed", "limited", "error"} {
		l.attrs[o] = metric.WithAttributeSet(attribute.NewSet(attribute.String("limiter", name), attribute.String("outcome", o)))
	}
	return l
}

// Allow checks one operation for key and takes it from the bucket if it is
// allowed. The error is non-nil only when Redis failed and the limiter is
// fail-closed; a fail-open limiter reports such operations as allowed.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	return l.AllowN(ctx, key, 1)
}

// AllowN is like [Limiter.Allow] for an operation costing n (at least 1)
// of the key's budget, such as a batch of n items. An n above the burst is
// never allowed.
func (l *Limiter) AllowN(ctx context.Context, key string, n int) (Result, error) {
	n = max(n, 1)
	if n > l.burst || l.closed {
		l.requests.Add(ctx, 1, l.attrs["limited"])
		return Result{}, nil
	}
	rctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	interval := strconv.FormatFloat(l.interval, 'f', -1, 64)
	vals, err := gcra.Run(rctx, l.rdb, []string{l.prefix + l.name + ":" + key}, interval, l.burst, n).Slice()
	if err == nil && len(vals) != 4 {
		err = errors.Internal.Errorf("ratelimit: unexpected script reply %v", vals)
	}
	if err != nil {
		l.requests.Add(ctx, 1, l.attrs["error"])
		l.logger.WarnContext(ctx, "redis rate limiter failed", "limiter", l.name, logging.Err(err))
		if l.failClosed {
			return Result{}, errors.Unavailable.Wrap(err, "ratelimit "+l.name)
		}
		return Result{Allowed: true}, nil
	}
	res := Result{
		Allowed:    vals[0] == int64(1),
		RetryAfter: seconds(vals[2]),
		ResetAfter: seconds(vals[3]),
	}
	if r, ok := vals[1].(int64); ok {
		res.Remaining = int(r)
	}
	if res.Allowed {
		l.requests.Add(ctx, 1, l.attrs["allowed"])
	} else {
		l.requests.Add(ctx, 1, l.attrs["limited"])
	}
	return res, nil
}

// Take is [Limiter.Allow] returning an error when the operation is not
// allowed: a *ratelimit.LimitedError of kind RateLimited whose RetryAfter
// httpserver sends as the Retry-After header, or the error of a
// fail-closed limiter.
func (l *Limiter) Take(ctx context.Context, key string) error {
	res, err := l.Allow(ctx, key)
	switch {
	case err != nil:
		return err
	case !res.Allowed:
		return &ratelimit.LimitedError{Limiter: l.name, After: res.RetryAfter}
	}
	return nil
}

// Middleware limits requests by the key that key returns; requests for
// which it returns false are not limited. Limited requests get 429 with
// Retry-After, as a problem document.
func Middleware(l *Limiter, key func(*http.Request) (string, bool)) httpserver.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k, ok := key(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if err := l.Take(r.Context(), k); err != nil {
				httpserver.WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func seconds(v any) time.Duration {
	s, _ := v.(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || f > math.MaxInt64/float64(time.Second) {
		return 0
	}
	// Redis's clock has microsecond resolution; drop float noise below it.
	return time.Duration(f * float64(time.Second)).Round(time.Microsecond)
}
