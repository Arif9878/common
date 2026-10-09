package migrate_test

import (
	"context"
	"log"
	"os"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/postgres/migrate"
)

// Up applies the goose migrations in a directory (an embed.FS in a real
// service). Replicas starting together take turns; only the first applies
// anything.
func ExampleUp() {
	ctx := context.Background()
	var db *postgres.DB // from postgres.New

	// migrations/00001_orders.sql:
	//   -- +goose Up
	//   CREATE TABLE orders (id text PRIMARY KEY, status text NOT NULL);
	//   -- +goose Down
	//   DROP TABLE orders;
	applied, err := migrate.Up(ctx, db, os.DirFS("migrations"))
	if err != nil {
		log.Fatal(err)
	}
	for _, a := range applied {
		log.Printf("applied %d %s in %s", a.Version, a.Name, a.Duration)
	}
}
