// Package testkit holds test helpers used across many packages and
// services: capturing logs, metrics and spans from the platform packages,
// waiting for asynchronous conditions, and integration-test environment.
//
// It is for tests only; production packages must not import it (a lint
// rule enforces this in this repository). It is deliberately small and is
// not an assertion framework: use the standard testing package (or
// testify, if your team uses it) for assertions.
//
// Other test doubles live next to what they replace: secret.Static,
// featureflag.Static, idempotency.NewMemoryStore, and the conformance
// suites idempotencytest and locktest. For deterministic time, use
// testing/synctest.
//
//	func TestCharge(t *testing.T) {
//		logger, logs := testkit.NewLogger(t)
//		mp, metrics := testkit.NewMetrics(t)
//		svc := payments.New(payments.WithLogger(logger), payments.WithMeterProvider(mp))
//
//		// ... exercise svc ...
//
//		if n := metrics.Sum("payments.charges", attribute.String("outcome", "ok")); n != 1 {
//			t.Errorf("charges = %v", n)
//		}
//		if len(logs.Messages("charge failed")) != 0 {
//			t.Errorf("unexpected failure log:\n%s", logs)
//		}
//	}
package testkit

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// Eventually calls cond every 10ms until it returns true, and fails the
// test (naming what) if timeout passes first. For goroutines whose timing
// can be controlled, prefer testing/synctest, which needs no polling.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// FreeAddr returns a "127.0.0.1:port" address that was free when checked,
// for servers that must listen on a known address. Another process can
// take it before the server binds; prefer passing a listener on port 0
// where the API allows it.
func FreeAddr(t testing.TB) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testkit: free address: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// Getenv returns the environment variable name, or skips the test if it
// is unset or empty. Use it to gate integration tests on real
// infrastructure, for example POSTGRES_TEST_URL or KAFKA_TEST_BROKERS.
func Getenv(t testing.TB, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set", name)
	}
	return v
}
