package kafka_test

import (
	"context"
	"log"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/messaging/kafka"
)

type orderCreated struct {
	ID string `json:"id"`
}

func Example() {
	ctx := context.Background()
	shutdown := graceful.New()
	cfg := kafka.Config{Brokers: []string{"kafka-1:9092", "kafka-2:9092"}}

	producer, err := kafka.NewProducer(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}

	consumer, err := kafka.NewConsumer(ctx, cfg, "billing", []string{"orders.created"},
		kafka.Typed(kafka.JSON[orderCreated]{}, func(ctx context.Context, r *kgo.Record, o orderCreated) error {
			return nil // charge o.ID; must be idempotent (at-least-once delivery)
		}),
		kafka.WithConcurrency(16),
		kafka.WithDLQ(producer, "orders.created.dlq"),
	)
	if err != nil {
		log.Fatal(err)
	}

	shutdown.Go("kafka consumer", func() error { return consumer.Run(ctx) })
	_ = shutdown.Register(graceful.StopIntake, "kafka consumer", consumer.Close) // stop polling, finish in-flight
	_ = shutdown.Register(graceful.Drain, "kafka producer", producer.Close)      // flush after consumers stopped
	if err := shutdown.Wait(ctx); err != nil {
		log.Fatal(err)
	}
}
