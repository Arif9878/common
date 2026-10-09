package ratelimit_test

import (
	"context"
	"fmt"
	"time"

	"github.com/Arif9878/common/go/resilience/ratelimit"
)

func Example() {
	// 10 calls per second to a partner API, in bursts of up to 3.
	limiter := ratelimit.New("partner-api", 10, 3, ratelimit.WithMaxWait(time.Second))

	allowed := 0
	for range 5 {
		if limiter.Allow() { // shed load: reject instead of waiting
			allowed++
		}
	}
	fmt.Println("allowed at once:", allowed)

	// Or wait for a token, up to WithMaxWait or the context deadline.
	err := limiter.Wait(context.Background())
	fmt.Println("waited:", err)
	// Output:
	// allowed at once: 3
	// waited: <nil>
}
