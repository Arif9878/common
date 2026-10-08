package grpcserver_test

import (
	"context"
	"log"
	"log/slog"

	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/transport/grpc/grpcserver"
)

func Example() {
	logger := slog.Default()
	shutdown := graceful.New()
	checks := health.New()

	srv := grpcserver.New(grpcserver.WithLogger(logger), grpcserver.WithHealth(checks))
	// orderspb.RegisterOrdersServer(srv, &ordersService{})

	if err := grpcserver.Serve(shutdown, srv, ":9090"); err != nil {
		log.Fatal(err)
	}
	checks.MarkStarted()
	_ = shutdown.Register(graceful.Unready, "readiness", checks.Drain)
	if err := shutdown.Wait(context.Background()); err != nil {
		log.Fatal(err)
	}
}
