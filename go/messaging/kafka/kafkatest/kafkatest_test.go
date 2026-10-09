package kafkatest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/kafka/kafkatest"
)

func TestProduceAndConsume(t *testing.T) {
	k := kafkatest.New(t, 2, "events")
	if !strings.HasPrefix(k.Topic("events"), "commontest-TestProduceAndConsume-") || !strings.HasPrefix(k.Group("g"), k.Prefix) {
		t.Errorf("names %q, %q", k.Topic("events"), k.Group("g"))
	}
	p, err := kafka.NewProducer(context.Background(), k.Config, kafka.WithLinger(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Publish(context.Background(),
		&kgo.Record{Topic: k.Topic("events"), Key: []byte("a"), Value: []byte("1")},
		&kgo.Record{Topic: k.Topic("events"), Key: []byte("b"), Value: []byte("2")}); err != nil {
		t.Fatal(err)
	}
	if got := kafkatest.Consume(t, k, k.Topic("events"), 2); len(got) != 2 {
		t.Errorf("consumed %d records", len(got))
	}
}
