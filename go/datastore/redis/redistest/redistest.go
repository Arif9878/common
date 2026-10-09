// Package redistest gives tests a Redis: an in-process miniredis, or the
// server in REDIS_TEST_ADDR to run against real Redis (Lua scripts, server
// time, cluster-specific behavior).
//
//	rdb := redistest.Client(t)               // miniredis, closed at cleanup
//	cfg, mr := redistest.Config(t)           // for redis.New or commonfx.Redis; mr controls time
//	rdb, prefix := redistest.Real(t)         // REDIS_TEST_ADDR, skipped when unset
package redistest

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/datastore/redis"
)

// EnvVar names the variable with a real Redis address to test against.
const EnvVar = "REDIS_TEST_ADDR"

// Config starts a miniredis and returns a redis.Config for it, and the
// server, whose FastForward and SetTime control expiry and TIME. The server
// stops when the test ends.
func Config(t testing.TB) (redis.Config, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return redis.Config{Addrs: []string{mr.Addr()}}, mr
}

// Client returns a go-redis client of a new miniredis, closed when the test
// ends. It satisfies the goredis.Cmdable and goredis.Scripter parameters of
// the cache, lock, idempotency and rate limit packages.
func Client(t testing.TB) *goredis.Client {
	t.Helper()
	cfg, _ := Config(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: cfg.Addrs[0]})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// Real returns a client of the server in REDIS_TEST_ADDR and a key prefix
// unique to the test ("commontest:<random>:"); it skips the test when the
// variable is unset. Keys under the prefix are deleted when the test ends;
// use it for every key the test writes, since the server may be shared.
func Real(t testing.TB) (*goredis.Client, string) {
	t.Helper()
	addr := os.Getenv(EnvVar)
	if addr == "" {
		t.Skip(EnvVar + " not set")
	}
	rdb := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		t.Fatalf("%s: %v", EnvVar, err)
	}
	prefix := "commontest:" + strings.ToLower(rand.Text()[:8]) + ":"
	t.Cleanup(func() {
		ctx := context.Background()
		iter := rdb.Scan(ctx, 0, prefix+"*", 100).Iterator()
		for iter.Next(ctx) {
			_ = rdb.Del(ctx, iter.Val()).Err()
		}
		_ = rdb.Close()
	})
	return rdb, prefix
}
