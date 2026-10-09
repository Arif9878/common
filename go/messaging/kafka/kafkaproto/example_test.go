package kafkaproto_test

import (
	"context"
	"log"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/kafka/kafkaproto"
)

// wrapperspb.StringValue stands in for a generated message such as
// *orderspb.OrderCreated.
func Example() {
	ctx := context.Background()
	rc, err := kafkaproto.NewRegistry(kafkaproto.Config{URLs: []string{"http://redpanda:8081"}})
	if err != nil {
		log.Fatal(err)
	}
	// Registers the message's schema under orders.created-value (or checks
	// it is already there) and keeps its ID.
	serde, err := kafkaproto.New[*wrapperspb.StringValue](ctx, rc, kafkaproto.ValueSubject("orders.created"))
	if err != nil {
		log.Fatal(err)
	}

	// Producer side.
	var producer *kafka.Producer // from kafka.NewProducer
	r, err := kafka.Encode("orders.created", []byte("o-1"), wrapperspb.String("o-1"), serde)
	if err != nil {
		log.Fatal(err)
	}
	if err := producer.Publish(ctx, r); err != nil {
		log.Fatal(err)
	}

	// Consumer side: Typed decodes each record before the handler sees it.
	handler := kafka.Typed(serde, func(ctx context.Context, r *kgo.Record, v *wrapperspb.StringValue) error {
		log.Printf("order created: %s", v.GetValue())
		return nil
	})
	_ = handler // kafka.NewConsumer(ctx, cfg, "billing", []string{"orders.created"}, handler)
}
