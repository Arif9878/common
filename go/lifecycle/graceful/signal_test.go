//go:build unix

package graceful_test

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lifecycle/graceful"
)

// ignoreSIGUSR1 keeps SIGUSR1 from terminating the test binary before the
// manager has registered for it.
func ignoreSIGUSR1(t *testing.T) {
	t.Helper()
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGUSR1)
	t.Cleanup(func() { signal.Stop(ch) })
}

// signalUntil sends SIGUSR1 to the process until done is closed. Wait
// registers for signals asynchronously, so a single signal could be missed.
func signalUntil(t *testing.T, done <-chan struct{}) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	for {
		if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			return
		case <-deadline:
			t.Fatal("signal never observed")
		case <-ticker.C:
		}
	}
}

func TestSignalStartsShutdown(t *testing.T) {
	ignoreSIGUSR1(t)
	m := graceful.New(quiet, graceful.WithSignals(syscall.SIGUSR1))

	result := make(chan error, 1)
	go func() { result <- m.Wait(context.Background()) }()
	signalUntil(t, m.Stopping())

	if err := <-result; err != nil {
		t.Fatalf("Wait = %v", err)
	}
}

func TestSecondSignalAbortsShutdown(t *testing.T) {
	ignoreSIGUSR1(t)
	m := graceful.New(quiet, graceful.WithSignals(syscall.SIGUSR1), graceful.WithTimeout(time.Minute))

	started := make(chan struct{})
	mustRegister(t, m, graceful.Drain, "consumer", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})

	result := make(chan error, 1)
	go func() { result <- m.Wait(context.Background()) }()
	signalUntil(t, m.Stopping())

	// signalUntil may already have delivered the second signal; either way
	// Wait must return an abort error long before the one-minute timeout.
	select {
	case <-started:
		if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
	case err := <-result:
		result <- err
	}

	select {
	case err := <-result:
		if err == nil || errors.KindOf(err) != errors.Timeout && !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want abort error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second signal did not abort the one-minute shutdown")
	}
}
