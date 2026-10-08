package kafka_test

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/idempotency/redisstore"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/resilience/retry"
	"github.com/Arif9878/common/go/testkit"
)

// newRedisStore returns an idempotency store on an in-process Redis.
func newRedisStore(t *testing.T) *redisstore.Store {
	t.Helper()
	rdb := goredis.NewClient(&goredis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return redisstore.New(rdb)
}

// seen counts how often each record value was handled.
type seen struct {
	mu    sync.Mutex
	count map[string]int
	calls [][]string
}

func (s *seen) add(rs ...*kgo.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count == nil {
		s.count = map[string]int{}
	}
	var call []string
	for _, r := range rs {
		s.count[string(r.Value)]++
		call = append(call, string(r.Value))
	}
	s.calls = append(s.calls, call)
}

func (s *seen) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.count {
		n += c
	}
	return n
}

func (s *seen) get(v string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count[v]
}

// byValue identifies records by value, so a second consumer group sees
// the first group's records as already processed, like a redelivery.
func byValue(_ string, r *kgo.Record) string { return "test:" + string(r.Value) }

func TestIdempotencySkipsProcessedRecords(t *testing.T) {
	k := newKafka(t, 2, "t")
	store := newRedisStore(t)
	var kv []string
	for i := range 20 {
		kv = append(kv, "k"+strconv.Itoa(i%4), strconv.Itoa(i))
	}
	publish(t, newProducer(t, k.cfg), k.T("t"), kv...)

	var first seen
	c1, err := kafka.NewConsumer(context.Background(), k.cfg, k.G("g1"), []string{k.T("t")},
		func(_ context.Context, r *kgo.Record) error { first.add(r); return nil },
		quiet, kafka.WithIdempotency(kafka.Idempotency{Store: store, Key: byValue}))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c1)
	testkit.Eventually(t, 30*time.Second, "20 records", func() bool { return first.total() >= 20 })

	// Every record is delivered again (to another group): none is handled,
	// all are committed and counted as duplicates.
	mp, metrics := testkit.NewMetrics(t)
	var second seen
	c2, err := kafka.NewConsumer(context.Background(), k.cfg, k.G("g2"), []string{k.T("t")},
		func(_ context.Context, r *kgo.Record) error { second.add(r); return nil },
		quiet, kafka.WithMeterProvider(mp), kafka.WithIdempotency(kafka.Idempotency{Store: store, Key: byValue}))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c2)
	testkit.Eventually(t, 30*time.Second, "20 duplicates", func() bool {
		return metrics.Has("kafka.consumer.duplicates") &&
			metrics.Sum("kafka.consumer.duplicates", attribute.String("topic", k.T("t"))) >= 20
	})
	if n := second.total(); n != 0 {
		t.Errorf("second delivery handled %d records, want 0", n)
	}
	if n := first.total(); n != 20 {
		t.Errorf("first delivery handled %d records, want 20", n)
	}
}

func TestIdempotencyBatchSkipsDuplicatesAndKeepsPartialProgress(t *testing.T) {
	k := newKafka(t, 1, "t")
	store := newRedisStore(t)
	ctx := context.Background()
	// Records "2" and "5" were processed before (by another replica).
	for _, v := range []string{"2", "5"} {
		if _, err := idempotency.Do(ctx, store, byValue("", &kgo.Record{Value: []byte(v)}),
			func(context.Context) (int, error) { return 0, nil }); err != nil {
			t.Fatal(err)
		}
	}
	var kv []string
	for i := range 10 {
		kv = append(kv, "k", strconv.Itoa(i))
	}
	publish(t, newProducer(t, k.cfg), k.T("t"), kv...)

	var got seen
	var once sync.Once
	c, err := kafka.NewBatchConsumer(ctx, k.cfg, k.G("g"), []string{k.T("t")},
		func(_ context.Context, rs []*kgo.Record) error {
			var err error
			once.Do(func() {
				if len(rs) > 3 { // processed 3 records, then a transient failure
					got.add(rs[:3]...)
					err = &kafka.BatchError{Processed: 3, Err: errors.Unavailable.New("db busy")}
				}
			})
			if err == nil {
				got.add(rs...)
			}
			return err
		},
		quiet, kafka.WithBatchSize(10), kafka.WithBatchTimeout(300*time.Millisecond),
		kafka.WithIdempotency(kafka.Idempotency{Store: store, Key: func(g string, r *kgo.Record) string {
			if string(r.Value) == "3" {
				return "" // no key: only the consumer's offsets keep it from being handled twice
			}
			return byValue(g, r)
		}}))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)

	want := []string{"0", "1", "3", "4", "6", "7", "8", "9"}
	testkit.Eventually(t, 30*time.Second, "all new records", func() bool {
		for _, v := range want {
			if got.get(v) == 0 {
				return false
			}
		}
		return true
	})
	for _, v := range want {
		if n := got.get(v); n != 1 {
			t.Errorf("record %s handled %d times, want 1", v, n)
		}
	}
	for _, v := range []string{"2", "5"} {
		if n := got.get(v); n != 0 {
			t.Errorf("duplicate record %s handled %d times", v, n)
		}
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if len(got.calls) < 2 {
		t.Errorf("handler calls = %v, want the partial failure retried", got.calls)
	}
	for _, call := range got.calls {
		if !slices.IsSorted(call) {
			t.Errorf("batch %v out of order", call)
		}
	}
}

func TestIdempotencyWaitsForRecordInProgress(t *testing.T) {
	k := newKafka(t, 1, "t")
	store := newRedisStore(t)
	ctx := context.Background()
	// Another consumer is still handling record "1".
	token, claimed, _, err := store.Begin(ctx, byValue("", &kgo.Record{Value: []byte("1")}), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	publish(t, newProducer(t, k.cfg), k.T("t"), "k", "0", "k", "1", "k", "2")

	var got seen
	c, err := kafka.NewConsumer(ctx, k.cfg, k.G("g"), []string{k.T("t")},
		func(_ context.Context, r *kgo.Record) error { got.add(r); return nil },
		quiet, kafka.WithRetry(retry.New(retry.WithMaxAttempts(1000), retry.WithConstantBackoff(20*time.Millisecond))),
		kafka.WithIdempotency(kafka.Idempotency{Store: store, Key: byValue}))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)

	testkit.Eventually(t, 30*time.Second, "record 0", func() bool { return got.get("0") == 1 })
	time.Sleep(200 * time.Millisecond)
	if got.get("2") != 0 {
		t.Fatal("record 2 was handled before record 1, breaking partition order")
	}
	// The other consumer finishes: record 1 is a duplicate, and the
	// partition moves on.
	if err := store.Complete(ctx, byValue("", &kgo.Record{Value: []byte("1")}), token, nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 30*time.Second, "record 2", func() bool { return got.get("2") == 1 })
	if n := got.get("1"); n != 0 {
		t.Errorf("record 1 handled %d times, want 0 (completed elsewhere)", n)
	}
}

func TestIdempotencyReleasesClaimOnFailure(t *testing.T) {
	k := newKafka(t, 1, "t")
	store := newRedisStore(t)
	publish(t, newProducer(t, k.cfg), k.T("t"), "k", "0")

	var got seen
	var attempts sync.Map
	c, err := kafka.NewConsumer(context.Background(), k.cfg, k.G("g"), []string{k.T("t")},
		func(_ context.Context, r *kgo.Record) error {
			n, _ := attempts.LoadOrStore(string(r.Value), new(int))
			if *n.(*int)++; *n.(*int) == 1 {
				return errors.Unavailable.New("try again")
			}
			got.add(r)
			return nil
		},
		quiet, kafka.WithIdempotency(kafka.Idempotency{Store: store, Key: byValue}))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	// Had the failed attempt kept its claim, the retry would see the record
	// in progress and never handle it.
	testkit.Eventually(t, 30*time.Second, "record 0 after a retry", func() bool { return got.get("0") == 1 })
}

func TestIdempotencyDefaultKeys(t *testing.T) {
	r := &kgo.Record{Topic: "orders", Partition: 3, Offset: 42,
		Headers: []kgo.RecordHeader{{Key: "event-id", Value: []byte("e-1")}}}
	if got, want := kafka.OffsetKey("billing", r), "kafka:billing:orders:3:42"; got != want {
		t.Errorf("OffsetKey = %q, want %q", got, want)
	}
	if got, want := kafka.HeaderKey("event-id")("billing", r), "kafka:billing:event-id:e-1"; got != want {
		t.Errorf("HeaderKey = %q, want %q", got, want)
	}
	if got, want := kafka.HeaderKey("missing")("billing", r), kafka.OffsetKey("billing", r); got != want {
		t.Errorf("HeaderKey without header = %q, want %q", got, want)
	}

	_, err := kafka.NewConsumer(context.Background(), kafka.Config{Brokers: []string{"localhost:1"}}, "g", []string{"t"},
		func(context.Context, *kgo.Record) error { return nil }, quiet, kafka.WithIdempotency(kafka.Idempotency{}))
	if errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("no store: err = %v, want invalid_argument", err)
	}
}

func TestIdempotencyDuplicatesWithinOneBatch(t *testing.T) {
	k := newKafka(t, 1, "t")
	// Each event is published twice in a row, so both copies land in the
	// same batch.
	var kv []string
	for i := range 10 {
		v := strconv.Itoa(i)
		kv = append(kv, "k", v, "k", v)
	}
	publish(t, newProducer(t, k.cfg), k.T("t"), kv...)

	var got seen
	c, err := kafka.NewBatchConsumer(context.Background(), k.cfg, k.G("g"), []string{k.T("t")},
		func(_ context.Context, rs []*kgo.Record) error { got.add(rs...); return nil },
		quiet, kafka.WithBatchSize(20), kafka.WithBatchTimeout(300*time.Millisecond),
		kafka.WithIdempotency(kafka.Idempotency{Store: newRedisStore(t), Key: byValue}))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	testkit.Eventually(t, 30*time.Second, "10 events", func() bool { return got.total() >= 10 })
	time.Sleep(200 * time.Millisecond)
	for i := range 10 {
		if n := got.get(strconv.Itoa(i)); n != 1 {
			t.Errorf("event %d handled %d times, want 1", i, n)
		}
	}
}
