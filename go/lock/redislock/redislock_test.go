package redislock_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/lock/locktest"
	"github.com/Arif9878/common/go/lock/redislock"
	"github.com/Arif9878/common/go/testkit"
)

func TestLocker(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	locktest.Run(t, redislock.New(rdb), func(d time.Duration) { m.FastForward(d) }, "")
}

// TestLockerOnRedis runs the conformance suite against the server in
// REDIS_TEST_ADDR, so the Lua scripts and fencing tokens run on real
// Redis, not only on miniredis.
func TestLockerOnRedis(t *testing.T) {
	rdb := goredis.NewClient(&goredis.Options{Addr: testkit.Getenv(t, "REDIS_TEST_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("REDIS_TEST_ADDR: %v", err)
	}
	prefix := "commontest:" + strings.ToLower(rand.Text()[:8]) + ":"
	locktest.Run(t, redislock.New(rdb), time.Sleep, prefix)
}
