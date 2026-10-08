package errors_test

import (
	stderrors "errors"
	"fmt"

	"github.com/Arif9878/common/go/errors"
)

// Services declare their own classified sentinels; there is no central
// catalogue.
var ErrOrderNotFound = errors.NotFound.New("order not found")

func ExampleKind_Wrap() {
	dbErr := stderrors.New("dial tcp 10.0.0.5:5432: connection refused")

	err := errors.Unavailable.Wrap(dbErr, "load order")
	err = fmt.Errorf("checkout: %w", err)

	fmt.Println(errors.KindOf(err))
	fmt.Println(errors.IsRetryable(err))
	fmt.Println(errors.Is(err, dbErr))
	// Output:
	// unavailable
	// true
	// true
}

func ExamplePublicMessage() {
	err := fmt.Errorf("get order 42 for user 7: %w", ErrOrderNotFound)

	// Error() is for logs; PublicMessage is for responses.
	fmt.Println(err)
	fmt.Println(errors.PublicMessage(err))

	err = errors.WithPublicMessage(errors.InvalidArgument.New("qty < 1"), "quantity must be positive")
	fmt.Println(errors.PublicMessage(err))
	// Output:
	// get order 42 for user 7: order not found
	// not found
	// quantity must be positive
}
