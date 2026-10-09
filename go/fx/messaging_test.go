package commonfx_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sr"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/errors"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/kafka/kafkaproto"
	"github.com/Arif9878/common/go/messaging/outbox"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/metrics"
	"github.com/Arif9878/common/go/observability/tracing"
	"github.com/Arif9878/common/go/testkit"
	"github.com/Arif9878/common/go/testkit/pgtest"
)

type messagingConfig struct {
	Log     logging.Config `envPrefix:"LOG_"`
	Tracing tracing.Config `envPrefix:"TRACING_"`
	Metrics metrics.Config `envPrefix:"METRICS_"`
	Redis   redis.Config   `envPrefix:"REDIS_"`
	Kafka   kafka.Config   `envPrefix:"KAFKA_"`
}

func TestKafkaBatchConsumerWithIdempotency(t *testing.T) {
	m := miniredis.RunT(t)
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "orders"))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	env := map[string]string{
		"LOG_LEVEL":     "error",
		"REDIS_ADDRS":   m.Addr(),
		"KAFKA_BROKERS": strings.Join(cluster.ListenAddrs(), ","),
	}

	var mu sync.Mutex
	var got []string
	var producer *kafka.Producer
	var store idempotency.Store
	app := fxtest.New(t,
		commonfx.Config[messagingConfig](config.WithEnvironment(env)),
		commonfx.ConfigFields[messagingConfig](),
		commonfx.Observability(),
		commonfx.Lifecycle(commonfx.WithShutdownTimeout(10*time.Second)),
		commonfx.Redis(),
		commonfx.RedisIdempotency(),
		commonfx.KafkaIdempotency(kafka.Idempotency{Key: kafka.HeaderKey("event-id")}),
		commonfx.KafkaProducer(),
		commonfx.KafkaBatchConsumer("indexer", []string{"orders"}, func() kafka.BatchHandler {
			return func(_ context.Context, rs []*kgo.Record) error {
				mu.Lock()
				defer mu.Unlock()
				for _, r := range rs {
					got = append(got, string(r.Value))
				}
				return nil
			}
		}, kafka.WithBatchSize(10)),
		fx.Populate(&producer, &store),
		commonfx.Ready(),
	)
	app.RequireStart()
	defer app.RequireStop()

	// Each event is published twice, as a producer retrying would.
	var recs []*kgo.Record
	for _, id := range []string{"a", "b", "c"} {
		r := &kgo.Record{Topic: "orders", Value: []byte(id), Headers: []kgo.RecordHeader{{Key: "event-id", Value: []byte(id)}}}
		dup := *r
		recs = append(recs, r, &dup)
	}
	if err := producer.Publish(context.Background(), recs...); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, "3 events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 3
	})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("handled %v, want each event once", got)
	}
	if store == nil {
		t.Error("RedisIdempotency did not provide an idempotency.Store")
	}
}

func TestKafkaIdempotencyRejectsAStore(t *testing.T) {
	err := fx.New(
		fx.NopLogger,
		fx.Provide(func() idempotency.Store { return idempotency.NewMemoryStore() }),
		commonfx.KafkaIdempotency(kafka.Idempotency{Store: idempotency.NewMemoryStore()}),
		fx.Invoke(fx.Annotate(func([]kafka.Option) {}, fx.ParamTags(commonfx.KafkaOptions))),
	).Err()
	if errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("err = %v, want invalid_argument", err)
	}
}

func TestOutboxRelay(t *testing.T) {
	pg := pgtest.Config(t)
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "orders"))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	table := "fx_outbox_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	setup := pgtest.DB(t)
	ctx := context.Background()
	if _, err := setup.Exec(ctx, strings.Replace(outbox.Schema, "kafka_outbox", table, 1)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = setup.Exec(context.Background(), "DROP TABLE "+table) })

	var db *postgres.DB
	var box *outbox.Outbox
	var rc *sr.Client
	logger, _ := testkit.NewLogger(t)
	app := fxtest.New(t,
		fx.Supply(pg, kafka.Config{Brokers: cluster.ListenAddrs()}, kafkaproto.Config{URLs: []string{"http://localhost:1"}}, logger),
		commonfx.Lifecycle(),
		commonfx.Postgres(),
		commonfx.KafkaProducer(),
		commonfx.Outbox(outbox.WithTable(table)),
		commonfx.OutboxRelay(outbox.WithPollInterval(100*time.Millisecond)),
		commonfx.SchemaRegistry(),
		fx.Populate(&db, &box, &rc),
		commonfx.Ready(),
	)
	app.RequireStart()
	if rc == nil {
		t.Error("SchemaRegistry did not provide a client")
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Write(ctx, tx, &kgo.Record{Topic: "orders", Value: []byte("o-1")}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumeTopics("orders"))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var got []string
	for len(got) == 0 && fctx.Err() == nil {
		cl.PollFetches(fctx).EachRecord(func(r *kgo.Record) { got = append(got, string(r.Value)) })
	}
	if !slices.Equal(got, []string{"o-1"}) {
		t.Fatalf("consumed %v, want [o-1]", got)
	}
	app.RequireStop()
}
