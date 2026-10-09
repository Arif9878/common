package grpcclient_test

import (
	"context"
	"log"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/transport/grpc/grpcclient"
)

func Example() {
	shutdown := graceful.New()

	conn, err := grpcclient.New("dns:///inventory.internal:9090", // TLS with the system roots
		grpcclient.WithTimeout(3*time.Second), // for calls whose context has no deadline
		grpcclient.WithRetry(grpcclient.RetryPolicy{
			MaxAttempts: 3,
			Codes:       []codes.Code{codes.Unavailable}, // safe to repeat: the server did not process it
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	_ = shutdown.Register(graceful.CloseDeps, "inventory-grpc", func(context.Context) error { return conn.Close() })

	// inventory := inventorypb.NewInventoryClient(conn)
	// _, err = inventory.Reserve(ctx, req) // errors.KindOf(err) works on the result
}
