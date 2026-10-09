package ratelimit_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/datastore/redis/ratelimit"
)

func Example() {
	mr, err := miniredis.Run() // a real service passes its *redis.Client
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()
	mr.SetTime(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)) // a fixed clock, for a stable output
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	// Each tenant may make 5 requests per second, in bursts of up to 2,
	// across every replica of the service.
	perTenant := ratelimit.New(rdb, "orders-api", 5, 2)
	ctx := context.Background()
	for range 3 {
		res, err := perTenant.Allow(ctx, "tenant-42")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(res.Allowed, res.RetryAfter)
	}
	// Output:
	// true 0s
	// true 0s
	// false 200ms
}
