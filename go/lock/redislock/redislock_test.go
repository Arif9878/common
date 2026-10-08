package redislock_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/lock/locktest"
	"github.com/Arif9878/common/go/lock/redislock"
)

func TestLocker(t *testing.T) {
	m := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	locktest.Run(t, redislock.New(rdb), func(d time.Duration) { m.FastForward(d) }, "")
}
