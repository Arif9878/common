package kafka_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/kafka/kafkatest"
	"github.com/Arif9878/common/go/testkit"
)

// failAtPoison is a batch handler that reports the "poison" record with a
// BatchError and records everything it processed.
type failAtPoison struct {
	mu      sync.Mutex
	handled []string
}

func (f *failAtPoison) handle(_ context.Context, rs []*kgo.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range rs {
		if string(r.Value) == "poison" {
			return &kafka.BatchError{Processed: i, Err: errors.InvalidArgument.New("bad record")}
		}
		f.handled = append(f.handled, string(r.Value))
	}
	return nil
}

func (f *failAtPoison) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.handled)
}

func batchOpts() []kafka.Option {
	return []kafka.Option{quiet, kafka.WithBatchSize(10), kafka.WithBatchTimeout(300 * time.Millisecond)}
}

func TestBatchErrorSkipsOnlyTheFailedRecord(t *testing.T) {
	k := kafkatest.New(t, 1, "t")
	publish(t, newProducer(t, k.Config), k.Topic("t"), "k", "0", "k", "poison", "k", "2", "k", "3")
	var h failAtPoison
	mp, metrics := testkit.NewMetrics(t)
	c, err := kafka.NewBatchConsumer(context.Background(), k.Config, k.Group("g"), []string{k.Topic("t")}, h.handle,
		append(batchOpts(), kafka.WithSkipOnFailure(), kafka.WithMeterProvider(mp))...)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	testkit.Eventually(t, 30*time.Second, "records after the poison", func() bool { return len(h.snapshot()) >= 3 })
	if got := h.snapshot(); !slices.Equal(got, []string{"0", "2", "3"}) {
		t.Errorf("handled %v, want [0 2 3]", got)
	}
	if n := metrics.Sum("kafka.consumer.records", attribute.String("outcome", "skipped")); n != 1 {
		t.Errorf("skipped = %v, want 1", n)
	}
}

func TestBatchErrorAtFirstRecordGoesToDLQAlone(t *testing.T) {
	k := kafkatest.New(t, 1, "t", "t.dlq")
	p := newProducer(t, k.Config)
	publish(t, p, k.Topic("t"), "k", "poison", "k", "1", "k", "2")
	var h failAtPoison
	c, err := kafka.NewBatchConsumer(context.Background(), k.Config, k.Group("g"), []string{k.Topic("t")}, h.handle,
		append(batchOpts(), kafka.WithDLQ(p, k.Topic("t.dlq")))...)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	testkit.Eventually(t, 30*time.Second, "records after the poison", func() bool { return len(h.snapshot()) >= 2 })
	if got := h.snapshot(); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("handled %v, want [1 2]", got)
	}
	if dlq := consumeAll(t, k, k.Topic("t.dlq"), 1); string(dlq[0].Value) != "poison" {
		t.Errorf("dlq[0] = %q, want poison", dlq[0].Value)
	}
}

func TestBatchErrorStopsPartitionAtTheFailedRecord(t *testing.T) {
	k := kafkatest.New(t, 1, "t")
	publish(t, newProducer(t, k.Config), k.Topic("t"), "k", "0", "k", "poison", "k", "2")
	var h failAtPoison
	c, err := kafka.NewBatchConsumer(context.Background(), k.Config, k.Group("g"), []string{k.Topic("t")}, h.handle, batchOpts()...)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	testkit.Eventually(t, 30*time.Second, "the first record", func() bool { return len(h.snapshot()) >= 1 })
	time.Sleep(500 * time.Millisecond) // give it a chance to (wrongly) continue
	if got := h.snapshot(); !slices.Equal(got, []string{"0"}) {
		t.Errorf("handled %v; the partition should stop at the poison record", got)
	}
}

func TestBatchPlainErrorFailsTheWholeBatch(t *testing.T) {
	k := kafkatest.New(t, 1, "t", "t.dlq")
	p := newProducer(t, k.Config)
	publish(t, p, k.Topic("t"), "k", "0", "k", "1", "k", "2")
	c, err := kafka.NewBatchConsumer(context.Background(), k.Config, k.Group("g"), []string{k.Topic("t")},
		func(context.Context, []*kgo.Record) error { return errors.InvalidArgument.New("cannot tell which") },
		append(batchOpts(), kafka.WithDLQ(p, k.Topic("t.dlq")))...)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	// Without a BatchError the failed record is unknown: all three go.
	if dlq := consumeAll(t, k, k.Topic("t.dlq"), 3); len(dlq) != 3 {
		t.Errorf("dlq has %d records, want 3", len(dlq))
	}
}
