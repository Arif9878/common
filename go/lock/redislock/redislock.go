// Package redislock implements lock.Locker on a single Redis node (or one
// cluster slot). Expiry uses Redis key expiry; fencing tokens come from a
// per-key counter.
//
// This is not Redlock and gives no guarantee across Redis failover: with
// asynchronous replication, a lease written to a primary that fails before
// replicating is lost, and a second holder can acquire the key. Use
// redislock to avoid duplicate work; use pglock, or fencing against the
// protected resource, where correctness matters (see the lock package).
package redislock

import (
	"context"
	"crypto/rand"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/lock"
)

// Locker implements lock.Locker.
type Locker struct {
	rdb    goredis.Scripter
	prefix string
}

// Option configures [New].
type Option func(*Locker)

// WithPrefix sets the key prefix. The default is "lock:".
func WithPrefix(p string) Option { return func(l *Locker) { l.prefix = p } }

// New returns a Locker using rdb.
func New(rdb goredis.Scripter, opts ...Option) *Locker {
	l := &Locker{rdb: rdb, prefix: "lock:"}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// acquire sets the lease and returns the next fence, or 0 if held. The
// fence counter key ({key}:fence) shares the lease key's hash slot.
var acquireScript = goredis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
	return redis.call('INCR', KEYS[2])
end
return 0`)

var releaseScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

var extendScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 0`)

// TryAcquire implements lock.Locker.
func (l *Locker) TryAcquire(ctx context.Context, key string, ttl time.Duration) (*lock.Lease, error) {
	k := l.prefix + "{" + key + "}"
	token := rand.Text()
	fence, err := acquireScript.Run(ctx, l.rdb, []string{k, k + ":fence"}, token, ttl.Milliseconds()).Int64()
	if err != nil {
		return nil, redis.Classify(err)
	}
	if fence == 0 {
		return nil, lock.ErrNotAcquired
	}
	return lock.NewLease(key, token, uint64(fence), ttl, //nolint:gosec // INCR results are positive
		func(ctx context.Context) error {
			return l.affect(releaseScript.Run(ctx, l.rdb, []string{k}, token))
		},
		func(ctx context.Context, ttl time.Duration) error {
			return l.affect(extendScript.Run(ctx, l.rdb, []string{k}, token, ttl.Milliseconds()))
		}), nil
}

func (l *Locker) affect(cmd *goredis.Cmd) error {
	n, err := cmd.Int()
	if err != nil {
		return redis.Classify(err)
	}
	if n == 0 {
		return lock.ErrNotHeld
	}
	return nil
}
