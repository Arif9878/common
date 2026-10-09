package redislock_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lock"
	"github.com/Arif9878/common/go/lock/redislock"
)

func Example() {
	mr, err := miniredis.Run() // a real service passes its *redis.Client
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()

	locker := redislock.New(rdb)
	lease, err := locker.TryAcquire(ctx, "reindex", time.Minute)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("acquired", lease.Key)

	// Another replica doesn't get it while the lease is held.
	_, err = locker.TryAcquire(ctx, "reindex", time.Minute)
	fmt.Println(errors.Is(err, lock.ErrNotAcquired))

	// Pass lease.Fence to the store being written, so a write from an
	// expired lease holder can be rejected.
	_ = lease.Release(ctx)
	// Output:
	// acquired reindex
	// true
}
