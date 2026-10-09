package requestid_test

import (
	"context"
	"fmt"

	"github.com/Arif9878/common/go/requestid"
)

func Example() {
	// httpserver and grpcserver do this for every request: keep a valid
	// incoming X-Request-ID, or create one.
	incoming := "req-3f2a"
	id := incoming
	if !requestid.Valid(id) {
		id = requestid.New()
	}
	ctx := requestid.NewContext(context.Background(), id)

	got, ok := requestid.FromContext(ctx) // logs and outgoing calls carry it
	fmt.Println(got, ok)
	fmt.Println(requestid.Valid("bad id with spaces"))
	// Output:
	// req-3f2a true
	// false
}
