package health_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Arif9878/common/go/health"
)

func Example() {
	checks := health.New()
	checks.AddReadiness("database", func(context.Context) error { return nil }, health.WithTimeout(time.Second))
	checks.AddReadiness("cache", func(context.Context) error { return errors.New("connection refused") }, health.NonCritical())

	mux := http.NewServeMux()
	mux.Handle("GET /live", checks.LiveHandler())
	mux.Handle("GET /ready", checks.ReadyHandler())

	ctx := context.Background()
	fmt.Println("before start:", checks.Ready(ctx).Status, checks.Ready(ctx).Reason)
	checks.MarkStarted() // after migrations, warm-up, …
	r := checks.Ready(ctx)
	fmt.Println("after start:", r.Status, "cache:", r.Checks["cache"].Status)
	_ = checks.Drain(ctx) // in graceful.Unready, so load balancers stop routing here
	fmt.Println("draining:", checks.Ready(ctx).Status, checks.Ready(ctx).Reason)
	// Output:
	// before start: fail starting
	// after start: ok cache: fail
	// draining: fail draining
}
