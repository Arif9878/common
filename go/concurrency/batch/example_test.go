package batch_test

import (
	"context"
	"fmt"
	"time"

	"github.com/Arif9878/common/go/concurrency/batch"
)

func Example() {
	p := batch.New("audit-events", func(ctx context.Context, events []string) error {
		fmt.Println("insert", events) // e.g. one multi-row INSERT
		return nil
	}, batch.WithSize(2), batch.WithFlushInterval(time.Second))

	for _, e := range []string{"login", "view", "logout"} {
		if err := p.Add(context.Background(), e); err != nil {
			fmt.Println(err)
		}
	}
	// Close flushes the incomplete last batch; register it in graceful.Drain.
	_ = p.Close(context.Background())
	// Output:
	// insert [login view]
	// insert [logout]
}
