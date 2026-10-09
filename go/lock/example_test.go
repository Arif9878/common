package lock_test

import (
	"context"
	"time"

	"github.com/Arif9878/common/go/lock"
)

// locker would be pglock.New(db) or redislock.New(rdb).
var locker lock.Locker

func ExampleAcquire() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // bounds the wait
	defer cancel()
	lease, err := lock.Acquire(ctx, locker, "report:daily", time.Minute)
	if err != nil {
		return // another replica holds it
	}
	defer func() { _ = lease.Release(context.Background()) }()

	// Pass the fencing token to writes, so a stale holder's writes are
	// rejected by the store (see the package documentation).
	_ = lease.Fence
	// ... build the report; call lease.Extend for longer work ...
}
