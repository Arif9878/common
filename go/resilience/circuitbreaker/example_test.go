package circuitbreaker_test

import (
	"context"
	"fmt"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/resilience/circuitbreaker"
)

func Example() {
	cb := circuitbreaker.New("payments-api", circuitbreaker.WithConsecutiveFailures(3), quiet)
	charge := func(context.Context) (string, error) {
		return "", errors.Unavailable.New("payments-api unavailable")
	}

	for range 4 {
		_, err := circuitbreaker.Execute(context.Background(), cb, charge)
		if errors.Is(err, circuitbreaker.ErrOpen) {
			fmt.Println("open: failing fast")
			continue
		}
		fmt.Println("called:", err)
	}
	// Output:
	// called: payments-api unavailable
	// called: payments-api unavailable
	// called: payments-api unavailable
	// open: failing fast
}
