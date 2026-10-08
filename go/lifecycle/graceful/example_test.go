package graceful_test

import (
	"context"
	stderrors "errors"
	"log"
	"net/http"
	"time"

	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
)

func Example() {
	ctx := context.Background()
	shutdown := graceful.New(
		graceful.WithTimeout(25*time.Second),
		graceful.WithUnreadyDelay(5*time.Second), // > readiness probe period
	)

	h := health.New()
	_ = shutdown.Register(graceful.Unready, "readiness", h.Drain)

	mux := http.NewServeMux()
	mux.Handle("/ready", h.ReadyHandler())
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	_ = shutdown.Register(graceful.StopIntake, "http", srv.Shutdown)

	// Also register: worker pools in Drain, database pools in CloseDeps,
	// tracing and metrics providers' Shutdown in Telemetry.

	shutdown.Go("http", func() error {
		if err := srv.ListenAndServe(); !stderrors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	h.MarkStarted()

	if err := shutdown.Wait(ctx); err != nil {
		log.Fatal(err) // non-zero exit after a failure or an unclean shutdown
	}
}
