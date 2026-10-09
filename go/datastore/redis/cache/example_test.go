package cache_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/datastore/redis/cache"
)

type Product struct {
	SKU   string `json:"sku"`
	Price int    `json:"price"`
}

func Example() {
	mr, err := miniredis.Run() // a real service passes its *redis.Client
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	products := cache.New[Product](rdb, "products", 10*time.Minute)
	ctx := context.Background()

	loads := 0
	load := func(context.Context) (Product, error) {
		loads++ // the database query
		return Product{SKU: "a-1", Price: 1500}, nil
	}
	for range 3 {
		p, err := products.Get(ctx, "a-1", load)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(p.SKU, p.Price)
	}
	fmt.Println("loads:", loads)

	// After a write, drop the cached copy.
	_ = products.Delete(ctx, "a-1")
	// Output:
	// a-1 1500
	// a-1 1500
	// a-1 1500
	// loads: 1
}
