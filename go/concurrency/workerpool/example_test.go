package workerpool_test

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Arif9878/common/go/concurrency/workerpool"
	"github.com/Arif9878/common/go/errors"
)

func Example() {
	pool := workerpool.New("thumbnails", workerpool.WithWorkers(20), workerpool.WithQueueSize(1000))

	// In an HTTP handler: reject instead of blocking when saturated.
	handler := func(w http.ResponseWriter, r *http.Request) {
		err := pool.TrySubmit(r.Context(), func(ctx context.Context) error {
			return nil // render the thumbnail
		})
		if errors.Is(err, workerpool.ErrQueueFull) || errors.Is(err, workerpool.ErrClosed) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
	_ = handler

	// On shutdown (graceful.Drain): finish queued work.
	fmt.Println(pool.Shutdown(context.Background()))
	// Output: <nil>
}
