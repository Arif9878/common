// Package cache is a read-through (cache-aside) cache in Redis:
//
//	products := cache.New[Product](rdb, "products", 10*time.Minute)
//	p, err := products.Get(ctx, id, func(ctx context.Context) (Product, error) {
//		return repo.Product(ctx, id) // runs on a miss
//	})
//	_ = products.Delete(ctx, id) // after the product changes
//
// # Behavior
//
//   - A miss calls the loader once per key per replica, however many
//     requests are waiting for it (singleflight), so an expired hot key does
//     not stampede the database.
//   - Entries expire after the TTL plus or minus a jitter (10% by default),
//     so keys written together do not expire together.
//   - With WithMissingTTL, a loader error of kind NotFound is cached too, so
//     lookups of absent keys do not reach the database every time.
//   - Redis problems never fail a request: each Redis call is bounded by a
//     short timeout (100ms by default), and on error the loader's value is
//     returned, uncached. They are logged and counted.
//
// Values are JSON by default ([WithCodec] for another encoding). Keys are
// "<prefix><name>:<key>"; the prefix (default "cache:") separates services
// sharing a Redis.
package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
)

// Codec encodes cached values.
type Codec[T any] interface {
	Marshal(v T) ([]byte, error)
	Unmarshal(b []byte) (T, error)
}

// JSON is the default codec.
type JSON[T any] struct{}

// Marshal encodes v as JSON.
func (JSON[T]) Marshal(v T) ([]byte, error) { return json.Marshal(v) }

// Unmarshal decodes JSON.
func (JSON[T]) Unmarshal(b []byte) (T, error) {
	var v T
	err := json.Unmarshal(b, &v)
	return v, err
}

// Option configures [New].
type Option[T any] func(*Cache[T])

// WithCodec encodes values with c instead of JSON.
func WithCodec[T any](c Codec[T]) Option[T] { return func(ca *Cache[T]) { ca.codec = c } }

// WithJitter randomizes each entry's TTL by up to ±fraction (0 to 0.5).
// The default is 0.1.
func WithJitter[T any](fraction float64) Option[T] {
	return func(c *Cache[T]) { c.jitter = min(max(fraction, 0), 0.5) }
}

// WithMissingTTL caches NotFound loader errors for d.
func WithMissingTTL[T any](d time.Duration) Option[T] { return func(c *Cache[T]) { c.missingTTL = d } }

// WithPrefix replaces the key prefix "cache:".
func WithPrefix[T any](p string) Option[T] { return func(c *Cache[T]) { c.prefix = p } }

// WithRedisTimeout bounds each Redis call. The default is 100ms.
func WithRedisTimeout[T any](d time.Duration) Option[T] { return func(c *Cache[T]) { c.timeout = d } }

// WithLogger sets the logger. The default is slog.Default().
func WithLogger[T any](l *slog.Logger) Option[T] { return func(c *Cache[T]) { c.logger = l } }

// WithMeterProvider sets the meter provider. The default is the global one.
func WithMeterProvider[T any](mp metric.MeterProvider) Option[T] {
	return func(c *Cache[T]) { c.meterProv = mp }
}

// Cache caches values of type T. It is safe for concurrent use.
type Cache[T any] struct {
	rdb        goredis.Cmdable
	name       string
	ttl        time.Duration
	codec      Codec[T]
	jitter     float64
	missingTTL time.Duration
	prefix     string
	timeout    time.Duration
	logger     *slog.Logger
	meterProv  metric.MeterProvider

	group    singleflight.Group
	requests metric.Int64Counter
	loads    metric.Float64Histogram
	attrs    map[string]metric.AddOption
}

// missing marks a cached NotFound. A JSON value never starts with 0x00.
var missing = []byte{0, 'm'}

var errMissing = errors.NotFound.New("cache: not found (cached)")

// New returns a cache named name (a fixed, low-cardinality label used in
// keys and metrics) whose entries live ttl.
func New[T any](rdb goredis.Cmdable, name string, ttl time.Duration, opts ...Option[T]) *Cache[T] {
	c := &Cache[T]{rdb: rdb, name: name, ttl: ttl, codec: JSON[T]{}, jitter: 0.1, prefix: "cache:",
		timeout: 100 * time.Millisecond, logger: slog.Default(), meterProv: otel.GetMeterProvider()}
	for _, opt := range opts {
		opt(c)
	}
	meter := c.meterProv.Meter("github.com/Arif9878/common/go/datastore/redis/cache")
	c.requests, _ = meter.Int64Counter("cache.requests",
		metric.WithDescription("Cache lookups, by cache and outcome: hit, miss, missing (a cached not-found), error (Redis failed; served by the loader)."))
	c.loads, _ = meter.Float64Histogram("cache.load.duration", metric.WithUnit("s"),
		metric.WithDescription("Time spent in loaders on misses."))
	c.attrs = map[string]metric.AddOption{}
	for _, o := range []string{"hit", "miss", "missing", "error"} {
		c.attrs[o] = metric.WithAttributes(attribute.String("cache", name), attribute.String("outcome", o))
	}
	return c
}

func (c *Cache[T]) key(k string) string { return c.prefix + c.name + ":" + k }

// Get returns the cached value of key, or calls load, caches its result
// and returns it. load's errors are returned (and, for NotFound with
// WithMissingTTL, cached); Redis errors are not.
func (c *Cache[T]) Get(ctx context.Context, key string, load func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	b, err := c.redisGet(ctx, key)
	switch {
	case err == nil && string(b) == string(missing):
		c.requests.Add(ctx, 1, c.attrs["missing"])
		return zero, errMissing
	case err == nil:
		v, derr := c.codec.Unmarshal(b)
		if derr == nil {
			c.requests.Add(ctx, 1, c.attrs["hit"])
			return v, nil
		}
		c.logger.WarnContext(ctx, "cache: undecodable entry; reloading", "cache", c.name, logging.Err(derr))
		c.requests.Add(ctx, 1, c.attrs["miss"])
	case errors.Is(err, goredis.Nil):
		c.requests.Add(ctx, 1, c.attrs["miss"])
	default:
		c.requests.Add(ctx, 1, c.attrs["error"])
		c.logger.WarnContext(ctx, "cache: Redis read failed; using the loader", "cache", c.name, logging.Err(err))
	}

	ch := c.group.DoChan(key, func() (any, error) {
		// Detached from the first caller, so its cancellation does not fail
		// the others waiting for the same key.
		lctx := context.WithoutCancel(ctx)
		start := time.Now()
		v, err := load(lctx)
		c.loads.Record(lctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("cache", c.name)))
		switch {
		case err == nil:
			c.store(lctx, key, v)
		case errors.KindOf(err) == errors.NotFound && c.missingTTL > 0:
			_ = c.redisSet(lctx, key, missing, c.missingTTL) // logged; the error is returned anyway
		}
		return v, err
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return zero, r.Err
		}
		return r.Val.(T), nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Set caches v for key, replacing any entry.
func (c *Cache[T]) Set(ctx context.Context, key string, v T) error {
	b, err := c.codec.Marshal(v)
	if err != nil {
		return errors.InvalidArgument.Wrap(err, "cache: encode")
	}
	return c.redisSet(ctx, key, b, c.jitterTTL(c.ttl))
}

// Delete removes the entries of keys, so the next Get loads them again.
// Call it after the source data changes.
func (c *Cache[T]) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = c.key(k)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rdb.Del(ctx, full...).Err(); err != nil {
		return errors.Unavailable.Wrap(err, "cache: delete")
	}
	return nil
}

func (c *Cache[T]) store(ctx context.Context, key string, v T) {
	b, err := c.codec.Marshal(v)
	if err != nil {
		c.logger.WarnContext(ctx, "cache: cannot encode value; not cached", "cache", c.name, logging.Err(err))
		return
	}
	_ = c.redisSet(ctx, key, b, c.jitterTTL(c.ttl))
}

func (c *Cache[T]) jitterTTL(ttl time.Duration) time.Duration {
	if c.jitter == 0 {
		return ttl
	}
	f := 1 + c.jitter*(2*rand.Float64()-1) //nolint:gosec // jitter, not security
	return time.Duration(float64(ttl) * f)
}

func (c *Cache[T]) redisGet(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.rdb.Get(ctx, c.key(key)).Bytes()
}

func (c *Cache[T]) redisSet(ctx context.Context, key string, b []byte, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rdb.Set(ctx, c.key(key), b, ttl).Err(); err != nil {
		c.logger.WarnContext(ctx, "cache: Redis write failed", "cache", c.name, logging.Err(err))
		return errors.Unavailable.Wrap(err, "cache: write")
	}
	return nil
}
