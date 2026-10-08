package resilience_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Arif9878/common/go/resilience/circuitbreaker"
	"github.com/Arif9878/common/go/resilience/ratelimit"
	"github.com/Arif9878/common/go/resilience/retry"
)

func noop(context.Context) error { return nil }

func BenchmarkHotPaths(b *testing.B) {
	ctx := context.Background()
	b.Run("retry/policy_success", func(b *testing.B) {
		p := retry.New(retry.WithName("op"))
		b.ReportAllocs()
		for b.Loop() {
			_ = p.Do(ctx, noop)
		}
	})
	b.Run("retry/do_success", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = retry.Do(ctx, noop, retry.WithName("op"))
		}
	})
	b.Run("circuitbreaker/execute", func(b *testing.B) {
		cb := circuitbreaker.New("dep", circuitbreaker.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = cb.Execute(ctx, noop)
			}
		})
	})
	b.Run("ratelimit/allow", func(b *testing.B) {
		l := ratelimit.New("api", 1e9, 1000)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				l.Allow()
			}
		})
	})
}
