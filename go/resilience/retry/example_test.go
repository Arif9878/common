package retry_test

import (
	"context"
	"fmt"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/resilience/retry"
)

func Example() {
	attempts := 0
	err := retry.Do(context.Background(), func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.Unavailable.New("inventory service unavailable")
		}
		return nil
	},
		retry.WithName("inventory.reserve"),
		retry.WithMaxAttempts(5),
		retry.WithExponentialBackoff(time.Millisecond, 10*time.Millisecond),
	)
	fmt.Println(attempts, err)
	// Output: 3 <nil>
}

func ExampleNew() {
	// Build once, reuse on every request.
	policy := retry.New(retry.WithName("ledger.append"), retry.WithMaxAttempts(3))

	err := policy.Do(context.Background(), func(ctx context.Context) error {
		return errors.InvalidArgument.New("amount must be positive")
	})
	fmt.Println(err) // client errors are not retried
	// Output: amount must be positive
}
