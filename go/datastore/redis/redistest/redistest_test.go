package redistest_test

import (
	"context"
	"testing"
	"time"

	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/datastore/redis/redistest"
)

func TestConfigWorksWithRedisNew(t *testing.T) {
	ctx := context.Background()
	cfg, mr := redistest.Config(t)
	rdb, err := redis.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Stop(ctx) })
	if err := rdb.Set(ctx, "k", "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Minute)
	if mr.Exists("k") {
		t.Error("the returned server does not control expiry")
	}
}

func TestClient(t *testing.T) {
	rdb := redistest.Client(t)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestReal(t *testing.T) {
	rdb, prefix := redistest.Real(t) // skipped without REDIS_TEST_ADDR
	ctx := context.Background()
	if err := rdb.Set(ctx, prefix+"k", "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
}
