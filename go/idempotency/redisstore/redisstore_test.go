package redisstore_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/idempotency/idempotencytest"
	"github.com/Arif9878/common/go/idempotency/redisstore"
)

func TestStore(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	// miniredis expires keys only when its clock is advanced.
	idempotencytest.Run(t, redisstore.New(rdb), func(d time.Duration) { m.FastForward(d) }, "")
}
