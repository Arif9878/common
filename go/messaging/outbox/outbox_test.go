package outbox_test

import (
	"context"
	"crypto/rand"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/outbox"
	"github.com/Arif9878/common/go/testkit"
	"github.com/Arif9878/common/go/testkit/pgtest"
)

type env struct {
	db      *postgres.DB
	box     *outbox.Outbox
	table   string
	cluster *kfake.Cluster
	cfg     kafka.Config
}

// newEnv returns a database with a fresh outbox table and an in-process
// Kafka cluster with topic "orders".
func newEnv(t *testing.T, kopts ...kfake.Opt) *env {
	t.Helper()
	db := pgtest.DB(t)
	table := "outbox_test_" + strings.ToLower(rand.Text()[:8])
	ctx := context.Background()
	if _, err := db.Exec(ctx, strings.Replace(outbox.Schema, "kafka_outbox", table, 1)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DROP TABLE `+table) })
	c, err := kfake.NewCluster(append([]kfake.Opt{kfake.NumBrokers(1), kfake.SeedTopics(2, "orders")}, kopts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &env{db: db, box: outbox.New(outbox.WithTable(table)), table: table, cluster: c,
		cfg: kafka.Config{Brokers: c.ListenAddrs()}}
}

func (e *env) producer(t *testing.T, opts ...kafka.Option) *kafka.Producer {
	t.Helper()
	logger, _ := testkit.NewLogger(t)
	p, err := kafka.NewProducer(context.Background(), e.cfg, append([]kafka.Option{kafka.WithLogger(logger)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

// run starts relay and stops it at cleanup.
func run(t *testing.T, relay *outbox.Relay) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- relay.Run(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := relay.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
}

// write commits records in one transaction, or rolls it back.
func (e *env) write(t *testing.T, commit bool, records ...*kgo.Record) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.box.Write(ctx, tx, records...); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if commit {
		err = tx.Commit(ctx)
	} else {
		err = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) pending(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), `SELECT count(*) FROM `+e.table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// reader collects what is published to "orders".
type reader struct {
	mu   sync.Mutex
	recs []*kgo.Record
}

func (e *env) read(t *testing.T) *reader {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(e.cfg.Brokers...), kgo.ConsumeTopics("orders"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	rd := &reader{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			cl.PollFetches(ctx).EachRecord(func(r *kgo.Record) {
				rd.mu.Lock()
				defer rd.mu.Unlock()
				rd.recs = append(rd.recs, r)
			})
		}
	}()
	t.Cleanup(func() { cancel(); <-done; cl.Close() })
	return rd
}

func (rd *reader) values() []string {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	out := make([]string, len(rd.recs))
	for i, r := range rd.recs {
		out[i] = string(r.Value)
	}
	return out
}

func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func rec(key, value string, headers ...kgo.RecordHeader) *kgo.Record {
	return &kgo.Record{Topic: "orders", Key: []byte(key), Value: []byte(value), Headers: headers}
}

func TestPublishesCommittedRecordsInOrder(t *testing.T) {
	e := newEnv(t)
	mp, metrics := testkit.NewMetrics(t)
	run(t, e.box.NewRelay(e.db, e.producer(t), outbox.WithMeterProvider(mp)))
	rd := e.read(t)

	e.write(t, true, rec("k", "1"), rec("k", "2", kgo.RecordHeader{Key: "trace", Value: []byte("t-1")}))
	e.write(t, false, rec("k", "rolled-back"))
	e.write(t, true, rec("k", "3"), rec("k", "4", kgo.RecordHeader{Key: outbox.EventIDHeader, Value: []byte("mine")}))

	testkit.Eventually(t, 10*time.Second, "4 records", func() bool { return len(rd.values()) >= 4 })
	time.Sleep(200 * time.Millisecond)
	if got := rd.values(); !slices.Equal(got, []string{"1", "2", "3", "4"}) {
		t.Fatalf("published %v, want [1 2 3 4] (same key, so in order; nothing rolled back)", got)
	}
	rd.mu.Lock()
	ids := map[string]bool{}
	for _, r := range rd.recs {
		id := header(r, outbox.EventIDHeader)
		if id == "" || ids[id] {
			t.Errorf("record %s: event-id %q missing or repeated", r.Value, id)
		}
		ids[id] = true
	}
	if got := header(rd.recs[1], "trace"); got != "t-1" {
		t.Errorf("header trace = %q, want t-1", got)
	}
	if got := header(rd.recs[3], outbox.EventIDHeader); got != "mine" {
		t.Errorf("an existing event-id was replaced: %q", got)
	}
	rd.mu.Unlock()

	testkit.Eventually(t, 5*time.Second, "the outbox to empty", func() bool { return e.pending(t) == 0 })
	if n := metrics.Sum("outbox.records.published", attribute.String("topic", "orders")); n != 4 {
		t.Errorf("outbox.records.published = %v, want 4", n)
	}
	if n := metrics.HistogramCount("outbox.publish.lag"); n != 4 {
		t.Errorf("outbox.publish.lag count = %v, want 4", n)
	}
}

func TestNotificationWakesTheRelay(t *testing.T) {
	e := newEnv(t)
	// Polling alone would take a minute; the commit's NOTIFY must wake it.
	run(t, e.box.NewRelay(e.db, e.producer(t), outbox.WithPollInterval(time.Minute)))
	rd := e.read(t)
	time.Sleep(500 * time.Millisecond) // let the relay go idle
	start := time.Now()
	e.write(t, true, rec("k", "now"))
	testkit.Eventually(t, 10*time.Second, "the record", func() bool { return len(rd.values()) == 1 })
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("published after %v; the notification did not wake the relay", d)
	}
}

func TestOneRelayAtATime(t *testing.T) {
	e := newEnv(t)
	mp, metrics := testkit.NewMetrics(t)
	p := e.producer(t)
	for range 3 {
		run(t, e.box.NewRelay(e.db, p, outbox.WithMeterProvider(mp), outbox.WithBatchSize(7),
			outbox.WithPollInterval(50*time.Millisecond)))
	}
	rd := e.read(t)
	var want []string
	for i := range 100 {
		v := strconv.Itoa(1000 + i)
		want = append(want, v)
		e.write(t, true, rec("k", v))
	}
	testkit.Eventually(t, 20*time.Second, "100 records", func() bool { return len(rd.values()) >= 100 })
	time.Sleep(300 * time.Millisecond)
	if got := rd.values(); !slices.Equal(got, want) {
		t.Errorf("published %d records, want the 100 once each, in order", len(got))
	}
	if n := metrics.Sum("outbox.relay.active"); n != 1 {
		t.Errorf("outbox.relay.active = %v, want 1", n)
	}
}

func TestFailedPublishKeepsRecords(t *testing.T) {
	e := newEnv(t)
	mp, metrics := testkit.NewMetrics(t)
	p := e.producer(t, kafka.WithDeliveryTimeout(time.Second))
	run(t, e.box.NewRelay(e.db, p, outbox.WithMeterProvider(mp), outbox.WithRetryDelay(100*time.Millisecond)))

	// Topic "later" does not exist yet, so publishing fails.
	e.write(t, true, &kgo.Record{Topic: "later", Value: []byte("survives")})
	testkit.Eventually(t, 15*time.Second, "a failed round", func() bool {
		return metrics.Has("outbox.relay.failures") && metrics.Sum("outbox.relay.failures") > 0
	})
	if e.pending(t) != 1 {
		t.Fatal("the record left the outbox although it was not published")
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(e.cfg.Brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := kadm.NewClient(cl).CreateTopic(context.Background(), 1, 1, nil, "later"); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 15*time.Second, "the outbox to empty", func() bool { return e.pending(t) == 0 })
	if n := metrics.Sum("outbox.records.published", attribute.String("topic", "later")); n != 1 {
		t.Errorf("published = %v, want 1", n)
	}
}

func TestStopWaitsForTheRelay(t *testing.T) {
	e := newEnv(t)
	relay := e.box.NewRelay(e.db, e.producer(t))
	done := make(chan error, 1)
	go func() { done <- relay.Run(context.Background()) }()
	time.Sleep(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := relay.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("Run still running after Stop returned")
	}
	// The lock is released: another relay takes over at once.
	rd := e.read(t)
	run(t, e.box.NewRelay(e.db, e.producer(t), outbox.WithPollInterval(50*time.Millisecond)))
	e.write(t, true, rec("k", "next"))
	testkit.Eventually(t, 10*time.Second, "the record", func() bool { return len(rd.values()) == 1 })
}

func TestWriteValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.box.Write(ctx, e.db, &kgo.Record{Value: []byte("x")}); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("no topic: err = %v, want invalid_argument", err)
	}
	if err := e.box.Write(ctx, e.db); err != nil {
		t.Errorf("no records: %v", err)
	}
}
