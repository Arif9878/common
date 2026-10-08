// Package redisstore is a Redis idempotency.Store. Records are hashes under
// a key prefix ("idem:" by default) that expire on their own. All
// check-and-set steps run as Lua scripts, so they are atomic on a single
// Redis node or within one cluster slot.
//
// Redis durability applies: with asynchronous replication, a failover can
// lose recent records, and a lost record means a repeated execution. Use
// pgstore where that matters.
package redisstore

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/idempotency"
)

// Store implements idempotency.Store in Redis.
type Store struct {
	rdb    goredis.Scripter
	prefix string
}

// Option configures [New].
type Option func(*Store)

// WithPrefix sets the key prefix. The default is "idem:".
func WithPrefix(p string) Option { return func(s *Store) { s.prefix = p } }

// New returns a store using rdb (a *redis.Client from the datastore/redis
// package, or any go-redis client).
func New(rdb goredis.Scripter, opts ...Option) *Store {
	s := &Store{rdb: rdb, prefix: "idem:"}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// begin returns {1, token} when claimed, else {0, state, result}.
var beginScript = goredis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
	local v = redis.call('HMGET', KEYS[1], 'state', 'result')
	return {0, v[1], v[2] or ''}
end
redis.call('HSET', KEYS[1], 'state', 'in_progress', 'token', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return {1, ARGV[1]}`)

var completeScript = goredis.NewScript(`
if redis.call('HGET', KEYS[1], 'token') ~= ARGV[1] then return 0 end
redis.call('HSET', KEYS[1], 'state', 'completed', 'result', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1`)

var releaseScript = goredis.NewScript(`
if redis.call('HGET', KEYS[1], 'token') ~= ARGV[1] then return 0 end
return redis.call('DEL', KEYS[1])`)

// Begin implements idempotency.Store.
func (s *Store) Begin(ctx context.Context, key string, lease time.Duration) (string, bool, idempotency.Record, error) {
	res, err := beginScript.Run(ctx, s.rdb, []string{s.prefix + key}, idempotency.NewToken(), lease.Milliseconds()).Slice()
	if err != nil {
		return "", false, idempotency.Record{}, redis.Classify(err)
	}
	if claimed, _ := res[0].(int64); claimed == 1 {
		tok, _ := res[1].(string)
		return tok, true, idempotency.Record{}, nil
	}
	state, _ := res[1].(string)
	result, _ := res[2].(string)
	return "", false, idempotency.Record{State: idempotency.State(state), Result: []byte(result)}, nil
}

// Complete implements idempotency.Store.
func (s *Store) Complete(ctx context.Context, key, token string, result []byte, ttl time.Duration) error {
	n, err := completeScript.Run(ctx, s.rdb, []string{s.prefix + key}, token, result, ttl.Milliseconds()).Int()
	if err != nil {
		return redis.Classify(err)
	}
	if n == 0 {
		return idempotency.ErrLeaseLost
	}
	return nil
}

// Release implements idempotency.Store.
func (s *Store) Release(ctx context.Context, key, token string) error {
	n, err := releaseScript.Run(ctx, s.rdb, []string{s.prefix + key}, token).Int()
	if err != nil {
		return redis.Classify(err)
	}
	if n == 0 {
		return idempotency.ErrLeaseLost
	}
	return nil
}
