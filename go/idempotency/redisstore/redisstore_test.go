package redisstore_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/idempotency/idempotencytest"
	"github.com/Arif9878/common/go/idempotency/redisstore"
	"github.com/Arif9878/common/go/testkit"
)

func TestStore(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	// miniredis expires keys only when its clock is advanced.
	idempotencytest.Run(t, redisstore.New(rdb), func(d time.Duration) { m.FastForward(d) }, "")
}

// TestStoreOnRedis runs the conformance suite against the server in
// REDIS_TEST_ADDR, so the Lua scripts run on real Redis, not only on
// miniredis. Keys get a random prefix; they expire on their own.
func TestStoreOnRedis(t *testing.T) {
	rdb := goredis.NewClient(&goredis.Options{Addr: testkit.Getenv(t, "REDIS_TEST_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("REDIS_TEST_ADDR: %v", err)
	}
	prefix := "commontest:" + strings.ToLower(rand.Text()[:8]) + ":"
	idempotencytest.Run(t, redisstore.New(rdb), time.Sleep, prefix)
}
