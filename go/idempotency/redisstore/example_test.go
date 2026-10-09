package redisstore_test

import (
	"context"
	"fmt"
	"log"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/idempotency/redisstore"
)

type Receipt struct {
	ChargeID string `json:"charge_id"`
}

func Example() {
	mr, err := miniredis.Run() // a real service passes its *redis.Client
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()
	store := redisstore.New(rdb)

	charges := 0
	charge := func(context.Context) (Receipt, error) {
		charges++ // the call to the payment provider
		return Receipt{ChargeID: "ch_1"}, nil
	}
	// The same event delivered twice charges once; the second delivery
	// gets the stored receipt.
	for range 2 {
		r, err := idempotency.Do(ctx, store, "charge:evt-42", charge)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(r.ChargeID)
	}
	fmt.Println("charges:", charges)
	// Output:
	// ch_1
	// ch_1
	// charges: 1
}
