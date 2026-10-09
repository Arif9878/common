package outbox_test

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/outbox"
)

type OrderCreated struct {
	OrderID string `json:"order_id"`
}

type jsonSerde struct{}

func (jsonSerde) Serialize(v OrderCreated) ([]byte, error) { return json.Marshal(v) }

func Example() {
	ctx := context.Background()
	shutdown := graceful.New()
	var (
		db       *postgres.DB    // from postgres.New; outbox.Schema applied in a migration
		producer *kafka.Producer // from kafka.NewProducer
	)
	box := outbox.New()

	// The order and its event commit together, or neither does.
	createOrder := func(ctx context.Context, id string) error {
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "INSERT INTO orders (id, status) VALUES ($1, 'new')", id); err != nil {
			return postgres.Classify(err)
		}
		r, err := kafka.Encode("orders.created", []byte(id), OrderCreated{OrderID: id}, jsonSerde{})
		if err != nil {
			return err
		}
		if err := box.Write(ctx, tx, r); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	_ = createOrder

	// One relay publishes committed records; replicas take turns.
	relay := box.NewRelay(db, producer, outbox.WithLogger(slog.Default()))
	shutdown.Go("outbox relay", func() error { return relay.Run(ctx) })
	if err := shutdown.Register(graceful.StopIntake, "outbox relay", relay.Stop); err != nil {
		log.Fatal(err)
	}
}
