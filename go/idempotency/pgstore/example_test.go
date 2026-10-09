package pgstore_test

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/idempotency/pgstore"
)

type Order struct {
	ID string `json:"id"`
}

// DoTx commits the order and the idempotency record together, so a
// retried request (same Idempotency-Key) never creates a second order.
func ExampleDoTx() {
	ctx := context.Background()
	var db *postgres.DB // from postgres.New; pgstore.Schema applied in a migration
	store := pgstore.New(db)
	key := "create-order:" + "3f1c…" // the request's Idempotency-Key header

	order, outcome, err := pgstore.DoTx(ctx, db, store, key, 24*time.Hour,
		func(ctx context.Context, tx pgx.Tx) (Order, error) {
			o := Order{ID: "o-1"}
			_, err := tx.Exec(ctx, "INSERT INTO orders (id, status) VALUES ($1, 'new')", o.ID)
			return o, err
		})
	if err != nil {
		log.Fatal(err)
	}
	if outcome == idempotency.Duplicate {
		log.Printf("replayed order %s", order.ID) // answer as the first time
	}
}
