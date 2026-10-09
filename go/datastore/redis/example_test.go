package redis_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
)

func Example() {
	mr, err := miniredis.Run() // stands in for the REDIS_ADDRS server
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()
	ctx := context.Background()
	shutdown := graceful.New()
	checks := health.New()

	rdb, err := redis.New(ctx, redis.Config{Addrs: []string{mr.Addr()}}) // config.Load with prefix REDIS_
	if err != nil {
		log.Fatal(err)
	}
	_ = shutdown.Register(graceful.CloseDeps, "redis", rdb.Stop)
	checks.AddReadiness("redis", rdb.HealthCheck)

	// Every go-redis command is available.
	if err := rdb.Set(ctx, "greeting", "hello", time.Minute).Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Println(rdb.Get(ctx, "greeting").Val())
	_ = rdb.Stop(ctx)
	// Output: hello
}
