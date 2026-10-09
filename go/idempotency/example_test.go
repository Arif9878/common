package idempotency_test

import (
	"context"
	"fmt"

	"github.com/Arif9878/common/go/idempotency"
)

type receipt struct{ ChargeID string }

func ExampleDo() {
	store := idempotency.NewMemoryStore() // use redisstore or pgstore across replicas
	ctx := context.Background()
	charges := 0
	charge := func(context.Context) (receipt, error) {
		charges++
		return receipt{ChargeID: fmt.Sprintf("ch_%d", charges)}, nil
	}

	// The same order delivered twice (a Kafka redelivery, a client retry)
	// is charged once; the second call returns the stored receipt.
	for range 2 {
		r, out, err := idempotency.DoOutcome(ctx, store, "charge:order-42", charge)
		fmt.Println(r.ChargeID, out == idempotency.Duplicate, err)
	}
	fmt.Println("charges:", charges)
	// Output:
	// ch_1 false <nil>
	// ch_1 true <nil>
	// charges: 1
}
