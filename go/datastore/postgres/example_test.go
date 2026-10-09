package postgres_test

import (
	"context"
	"log"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
)

func Example() {
	ctx := context.Background()
	shutdown := graceful.New()
	checks := health.New()

	// POSTGRES_HOST, POSTGRES_DATABASE, POSTGRES_USER, POSTGRES_PASSWORD, …
	cfg, err := config.Load[postgres.Config](config.WithPrefix("POSTGRES_"))
	if err != nil {
		log.Fatal(err)
	}
	db, err := postgres.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	_ = shutdown.Register(graceful.CloseDeps, "postgres", db.Close)
	checks.AddReadiness("postgres", db.Ping)

	var status string
	err = db.QueryRow(ctx, "SELECT status FROM orders WHERE id = $1", "o-1").Scan(&status)
	if err = postgres.Classify(err); err != nil {
		log.Println(err) // errors.NotFound for a missing row, Conflict for a unique violation, …
	}
}
