package kafka_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/resilience/retry"
)

var quiet = kafka.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

func newCluster(t *testing.T, partitions int32, topics ...string) kafka.Config {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(partitions, topics...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return kafka.Config{Brokers: c.ListenAddrs()}
}

func newProducer(t *testing.T, cfg kafka.Config, opts ...kafka.Option) *kafka.Producer {
	t.Helper()
	p, err := kafka.NewProducer(context.Background(), cfg, append([]kafka.Option{quiet, kafka.WithLinger(0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func publish(t *testing.T, p *kafka.Producer, topic string, kv ...string) {
	t.Helper()
	var recs []*kgo.Record
	for i := 0; i+1 < len(kv); i += 2 {
		recs = append(recs, &kgo.Record{Topic: topic, Key: []byte(kv[i]), Value: []byte(kv[i+1])})
	}
	if err := p.Publish(context.Background(), recs...); err != nil {
		t.Fatal(err)
	}
}

// running starts c and stops it at cleanup.
func running(t *testing.T, c *kafka.Consumer) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	t.Cleanup(func() {
		_ = c.Close(context.Background())
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// collector records handled values thread-safely.
type collector struct {
	mu     sync.Mutex
	values []string
	byKey  map[string][]string
}

func (c *collector) add(r *kgo.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = map[string][]string{}
	}
	c.values = append(c.values, string(r.Value))
	c.byKey[string(r.Key)] = append(c.byKey[string(r.Key)], string(r.Value))
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.values)
}

func (c *collector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.values)
}

func TestProduceConsumeOrderingAndCommit(t *testing.T) {
	cfg := newCluster(t, 4, "orders")
	p := newProducer(t, cfg)
	var kv []string
	for i := range 200 {
		kv = append(kv, "key-"+strconv.Itoa(i%10), fmt.Sprintf("%04d", i))
	}
	publish(t, p, "orders", kv...)

	var got collector
	c, err := kafka.NewConsumer(context.Background(), cfg, "billing", []string{"orders"},
		func(_ context.Context, r *kgo.Record) error { got.add(r); return nil }, quiet, kafka.WithConcurrency(4))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	waitFor(t, "200 records", func() bool { return got.len() >= 200 })
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	for key, vals := range got.byKey {
		if !slices.IsSorted(vals) {
			t.Errorf("records of %s out of order: %v", key, vals)
		}
	}

	// Offsets were committed: the group sees nothing new.
	var again collector
	c2, err := kafka.NewConsumer(context.Background(), cfg, "billing", []string{"orders"},
		func(_ context.Context, r *kgo.Record) error { again.add(r); return nil }, quiet)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c2)
	publish(t, p, "orders", "key-0", "9999")
	waitFor(t, "the new record", func() bool { return again.len() >= 1 })
	if vals := again.snapshot(); len(vals) != 1 || vals[0] != "9999" {
		t.Errorf("after restart got %v, want only the new record", vals)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	cfg := newCluster(t, 1, "t")
	publish(t, newProducer(t, cfg), "t", "k", "v")
	var attempts atomic.Int32
	var done atomic.Bool
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, func(context.Context, *kgo.Record) error {
		if attempts.Add(1) < 3 {
			return errors.Unavailable.New("db down")
		}
		done.Store(true)
		return nil
	}, quiet, kafka.WithRetry(retry.New(retry.WithMaxAttempts(5), retry.WithConstantBackoff(10*time.Millisecond))))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	waitFor(t, "success", done.Load)
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d", attempts.Load())
	}
}

func consumeAll(t *testing.T, cfg kafka.Config, topic string, n int) []*kgo.Record {
	t.Helper()
	var mu sync.Mutex
	var out []*kgo.Record
	c, err := kafka.NewConsumer(context.Background(), cfg, "reader-"+topic, []string{topic},
		func(_ context.Context, r *kgo.Record) error {
			mu.Lock()
			defer mu.Unlock()
			out = append(out, r)
			return nil
		}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	waitFor(t, fmt.Sprintf("%d records on %s", n, topic), func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(out) >= n
	})
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(out)
}

func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func TestPoisonToDLQ(t *testing.T) {
	cfg := newCluster(t, 1, "events", "events.dlq")
	p := newProducer(t, cfg)
	publish(t, p, "events", "a", "ok-1", "b", "poison", "c", "ok-2", "d", "panic")

	var got collector
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"events"}, func(_ context.Context, r *kgo.Record) error {
		switch string(r.Value) {
		case "poison":
			return errors.InvalidArgument.New("unknown event type")
		case "panic":
			panic("nil deref")
		}
		got.add(r)
		return nil
	}, quiet, kafka.WithDLQ(p, "events.dlq"))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)

	dlq := consumeAll(t, cfg, "events.dlq", 2)
	waitFor(t, "good records", func() bool { return got.len() == 2 })
	if string(dlq[0].Value) != "poison" || header(dlq[0], "dlq.error.kind") != "invalid_argument" ||
		header(dlq[0], "dlq.original.topic") != "events" || header(dlq[0], "dlq.original.offset") != "1" {
		t.Errorf("dlq record = %s %v", dlq[0].Value, dlq[0].Headers)
	}
	if string(dlq[1].Value) != "panic" || header(dlq[1], "dlq.error.kind") != "internal" {
		t.Errorf("panic dlq record = %s %v", dlq[1].Value, dlq[1].Headers)
	}
}

func TestFailureStopsPartitionByDefault(t *testing.T) {
	cfg := newCluster(t, 1, "t")
	p := newProducer(t, cfg)
	publish(t, p, "t", "k", "0", "k", "poison", "k", "2", "k", "3")

	reader := func(seen *collector) kafka.Handler {
		return func(_ context.Context, r *kgo.Record) error {
			seen.add(r)
			if string(r.Value) == "poison" {
				return errors.InvalidArgument.New("bad")
			}
			return nil
		}
	}
	reader1 := &collector{}
	reg := sdkmetric.NewManualReader()
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, reader(reader1), quiet,
		kafka.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reg))))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	waitFor(t, "poison", func() bool { return reader1.len() >= 2 })
	time.Sleep(300 * time.Millisecond) // give it a chance to (wrongly) continue
	if vals := reader1.snapshot(); len(vals) != 2 {
		t.Fatalf("handled %v after the failure; partition should be stopped", vals)
	}
	if v := gauge(t, reg, "kafka.consumer.partitions.stopped"); v != 1 {
		t.Errorf("partitions.stopped = %d", v)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-done

	// Nothing after offset 0 was committed: the next consumer starts at the poison record.
	reader2 := &collector{}
	c2, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, reader(reader2), quiet)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c2)
	waitFor(t, "redelivery", func() bool { return reader2.len() >= 1 })
	if first := reader2.snapshot()[0]; first != "poison" {
		t.Errorf("restart began at %q, want the failed record", first)
	}
}

func TestSkipOnFailure(t *testing.T) {
	cfg := newCluster(t, 1, "t")
	publish(t, newProducer(t, cfg), "t", "k", "1", "k", "poison", "k", "3")
	var got collector
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, func(_ context.Context, r *kgo.Record) error {
		if string(r.Value) == "poison" {
			return errors.InvalidArgument.New("bad")
		}
		got.add(r)
		return nil
	}, quiet, kafka.WithSkipOnFailure())
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	waitFor(t, "records after the skipped one", func() bool { return got.len() == 2 })
}

func TestBatchConsumerWithPartialFailure(t *testing.T) {
	cfg := newCluster(t, 1, "t", "t.dlq")
	p := newProducer(t, cfg)
	var kv []string
	for i := range 25 {
		v := strconv.Itoa(i)
		if i == 13 {
			v = "bad"
		}
		kv = append(kv, "k", v)
	}
	publish(t, p, "t", kv...)

	var mu sync.Mutex
	var sizes []int
	var handled []string
	c, err := kafka.NewBatchConsumer(context.Background(), cfg, "g", []string{"t"}, func(_ context.Context, rs []*kgo.Record) error {
		mu.Lock()
		defer mu.Unlock()
		sizes = append(sizes, len(rs))
		for i, r := range rs {
			if string(r.Value) == "bad" {
				return &kafka.BatchError{Processed: i, Err: errors.InvalidArgument.New("bad record")}
			}
			handled = append(handled, string(r.Value))
		}
		return nil
	}, quiet, kafka.WithBatchSize(10), kafka.WithBatchTimeout(200*time.Millisecond), kafka.WithDLQ(p, "t.dlq"))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)

	// Batch [10..19] processes 10–12, fails at 13; 13–19 go to the DLQ as a unit.
	dlq := consumeAll(t, cfg, "t.dlq", 7)
	if string(dlq[0].Value) != "bad" || string(dlq[6].Value) != "19" {
		t.Errorf("dlq = %s .. %s", dlq[0].Value, dlq[6].Value)
	}
	waitFor(t, "last batch", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) >= 18
	})
	mu.Lock()
	defer mu.Unlock()
	if slices.Max(sizes) > 10 {
		t.Errorf("batch sizes %v exceed 10", sizes)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	cfg := newCluster(t, 8, "t")
	p := newProducer(t, cfg)
	var kv []string
	for i := range 64 {
		kv = append(kv, strconv.Itoa(i), "v")
	}
	publish(t, p, "t", kv...)

	var active, peak, n atomic.Int32
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, func(context.Context, *kgo.Record) error {
		a := active.Add(1)
		for p := peak.Load(); a > p && !peak.CompareAndSwap(p, a); p = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		n.Add(1)
		return nil
	}, quiet, kafka.WithConcurrency(2))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	waitFor(t, "64 records", func() bool { return n.Load() == 64 })
	if peak.Load() != 2 {
		t.Errorf("peak concurrency = %d, want 2", peak.Load())
	}
}

func TestTracePropagation(t *testing.T) {
	cfg := newCluster(t, 1, "t")
	tp := sdktrace.NewTracerProvider()
	prop := kafka.WithPropagators(propagation.TraceContext{})
	p := newProducer(t, cfg, kafka.WithTracerProvider(tp), prop)

	ctx, span := tp.Tracer("test").Start(context.Background(), "http request")
	if err := p.Publish(ctx, &kgo.Record{Topic: "t", Value: []byte("v")}); err != nil {
		t.Fatal(err)
	}
	span.End()

	got := make(chan trace.SpanContext, 1)
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, func(ctx context.Context, _ *kgo.Record) error {
		got <- trace.SpanContextFromContext(ctx)
		return nil
	}, quiet, kafka.WithTracerProvider(tp), prop)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	select {
	case sc := <-got:
		if sc.TraceID() != span.SpanContext().TraceID() {
			t.Errorf("consumer trace %s, producer trace %s", sc.TraceID(), span.SpanContext().TraceID())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("record not consumed")
	}
}

func TestCloseWaitsForInFlight(t *testing.T) {
	cfg := newCluster(t, 1, "t")
	p := newProducer(t, cfg)
	publish(t, p, "t", "k", "1")

	started, release := make(chan struct{}), make(chan struct{})
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, func(context.Context, *kgo.Record) error {
		close(started)
		<-release
		return nil
	}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	<-started
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	<-done

	// The in-flight record finished and was committed.
	var again collector
	c2, _ := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"},
		func(_ context.Context, r *kgo.Record) error { again.add(r); return nil }, quiet)
	running(t, c2)
	publish(t, p, "t", "k", "2")
	waitFor(t, "new record", func() bool { return again.len() >= 1 })
	if vals := again.snapshot(); vals[0] != "2" {
		t.Errorf("redelivered %v after a clean close", vals)
	}
}

func TestCloseDeadlineCancelsHandlers(t *testing.T) {
	cfg := newCluster(t, 1, "t")
	publish(t, newProducer(t, cfg), "t", "k", "1")
	started := make(chan struct{})
	var canceled atomic.Bool
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, func(ctx context.Context, _ *kgo.Record) error {
		close(started)
		<-ctx.Done()
		canceled.Store(true)
		return ctx.Err()
	}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Close(ctx); errors.KindOf(err) != errors.Timeout || time.Since(start) > 5*time.Second {
		t.Fatalf("Close = %v after %v", err, time.Since(start))
	}
	<-done
	waitFor(t, "handler cancellation", canceled.Load)
}

func TestRebalanceProcessesEverything(t *testing.T) {
	cfg := newCluster(t, 6, "t")
	p := newProducer(t, cfg)
	var seen sync.Map
	var total atomic.Int32
	handler := func(_ context.Context, r *kgo.Record) error {
		if _, dup := seen.LoadOrStore(string(r.Value), true); !dup {
			total.Add(1)
		}
		time.Sleep(time.Millisecond)
		return nil
	}
	c1, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, handler, quiet)
	if err != nil {
		t.Fatal(err)
	}
	running(t, c1)
	for i := range 300 {
		publish(t, p, "t", strconv.Itoa(i), strconv.Itoa(i))
		if i == 100 {
			c2, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"}, handler, quiet)
			if err != nil {
				t.Fatal(err)
			}
			running(t, c2)
		}
	}
	waitFor(t, "all 300 records across the rebalance", func() bool { return total.Load() == 300 })
}

func TestTypedJSON(t *testing.T) {
	type order struct {
		ID    string `json:"id"`
		Total int    `json:"total"`
	}
	cfg := newCluster(t, 1, "t")
	p := newProducer(t, cfg)
	r, err := kafka.Encode("t", []byte("o-1"), order{ID: "o-1", Total: 42}, kafka.JSON[order]{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(context.Background(), r, &kgo.Record{Topic: "t", Value: []byte("not json")}); err != nil {
		t.Fatal(err)
	}

	got := make(chan order, 1)
	failed := make(chan error, 1)
	c, err := kafka.NewConsumer(context.Background(), cfg, "g", []string{"t"},
		kafka.Typed(kafka.JSON[order]{}, func(_ context.Context, _ *kgo.Record, o order) error {
			got <- o
			return nil
		}), quiet, kafka.WithSkipOnFailure(),
		kafka.WithRetry(retry.New(retry.WithOnRetry(func(_ int, err error, _ time.Duration) { failed <- err }))))
	if err != nil {
		t.Fatal(err)
	}
	running(t, c)
	if o := <-got; o.Total != 42 {
		t.Errorf("decoded %+v", o)
	}
	select {
	case err := <-failed:
		t.Errorf("decode error was retried: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPublishAsyncAndClose(t *testing.T) {
	cfg := newCluster(t, 1, "t")
	p, err := kafka.NewProducer(context.Background(), cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	var acked atomic.Int32
	for i := range 100 {
		p.PublishAsync(context.Background(), &kgo.Record{Topic: "t", Value: []byte(strconv.Itoa(i))},
			func(_ *kgo.Record, err error) {
				if err == nil {
					acked.Add(1)
				}
			})
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acked.Load() != 100 {
		t.Errorf("acked %d of 100 before Close returned", acked.Load())
	}
}

func TestConfigErrors(t *testing.T) {
	for name, cfg := range map[string]kafka.Config{
		"no brokers": {},
		"bad sasl":   {Brokers: []string{"x:9092"}, SASLMechanism: "kerberos"},
	} {
		if _, err := kafka.NewProducer(context.Background(), cfg); errors.KindOf(err) != errors.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := kafka.NewConsumer(context.Background(), kafka.Config{Brokers: []string{"x:1"}}, "", nil, nil); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("consumer without group: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := kafka.NewProducer(ctx, kafka.Config{Brokers: []string{"127.0.0.1:1"}}, quiet); errors.KindOf(err) != errors.Unavailable {
		t.Errorf("unreachable: %v (kind %v)", err, errors.KindOf(err))
	}
}

func gauge(t *testing.T, r *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data.(metricdata.Gauge[int64]).DataPoints[0].Value
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}
