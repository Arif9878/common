package commonfx

import (
	"context"

	"github.com/twmb/franz-go/pkg/sr"
	"go.uber.org/fx"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/idempotency/pgstore"
	"github.com/Arif9878/common/go/idempotency/redisstore"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/messaging/kafka/kafkaproto"
	"github.com/Arif9878/common/go/messaging/outbox"
)

// KafkaBatchConsumer is like [KafkaConsumer] for a batch handler: handler
// is a constructor returning a kafka.BatchHandler.
//
//	commonfx.KafkaBatchConsumer("indexer", []string{"orders.created"}, NewIndexer,
//		kafka.WithBatchSize(200)),
func KafkaBatchConsumer(group string, topics []string, handler any, opts ...kafka.Option) fx.Option {
	tag := nameTag("commonfx.kafka.batch-handler." + group)
	return fx.Options(
		fx.Provide(fx.Annotate(handler, fx.ResultTags(tag))),
		fx.Provide(fx.Annotate(
			func(h kafka.BatchHandler, cfg kafka.Config, shared []kafka.Option, t *telemetry) (*kafka.Consumer, error) {
				ctx, cancel := connectCtx()
				defer cancel()
				all := append(append(t.kafka(), shared...), opts...)
				return kafka.NewBatchConsumer(ctx, cfg, group, topics, h, all...)
			},
			fx.ParamTags(tag, ``, KafkaOptions),
			fx.ResultTags(kafkaConsumersGroup),
		)),
	)
}

// RedisIdempotency provides an idempotency.Store in the graph's Redis
// ([Redis]), for [KafkaIdempotency] and for idempotency.Do in handlers.
func RedisIdempotency(opts ...redisstore.Option) fx.Option {
	return fx.Provide(func(c *redis.Client) idempotency.Store { return redisstore.New(c, opts...) })
}

// PostgresIdempotency provides an idempotency.Store in the graph's
// PostgreSQL ([Postgres]). Create its table with pgstore.Schema.
func PostgresIdempotency(opts ...pgstore.Option) fx.Option {
	return fx.Provide(func(db *postgres.DB) idempotency.Store { return pgstore.New(db, opts...) })
}

// KafkaIdempotency makes every consumer from [KafkaConsumer] and
// [KafkaBatchConsumer] skip records already processed, using the graph's
// idempotency.Store (from [RedisIdempotency] or [PostgresIdempotency]).
// cfg.Store must be nil; the other fields are as in kafka.Idempotency:
//
//	commonfx.Redis(),
//	commonfx.RedisIdempotency(),
//	commonfx.KafkaIdempotency(kafka.Idempotency{Key: kafka.HeaderKey("event-id")}),
//
// To make only some consumers idempotent, pass kafka.WithIdempotency to
// those consumers instead.
func KafkaIdempotency(cfg kafka.Idempotency) fx.Option {
	return fx.Provide(fx.Annotate(
		func(store idempotency.Store) (kafka.Option, error) {
			if cfg.Store != nil {
				return nil, errors.InvalidArgument.New("commonfx: KafkaIdempotency takes the store from the graph; leave Store nil")
			}
			c := cfg
			c.Store = store
			return kafka.WithIdempotency(c), nil
		},
		fx.ResultTags(KafkaOptions),
	))
}

// SchemaRegistry provides the Schema Registry client (*sr.Client) from the
// graph's kafkaproto.Config, for kafkaproto.New:
//
//	commonfx.SchemaRegistry(),
//	fx.Provide(func(ctx context.Context, rc *sr.Client) (*kafkaproto.Serde[*orderspb.OrderCreated], error) {
//		return kafkaproto.New[*orderspb.OrderCreated](ctx, rc, kafkaproto.ValueSubject("orders.created"))
//	}),
//
// The client makes no request until it is used.
func SchemaRegistry() fx.Option {
	return fx.Provide(func(cfg kafkaproto.Config) (*sr.Client, error) { return kafkaproto.NewRegistry(cfg) })
}

// Outbox provides *outbox.Outbox, for writing records in the business
// transaction with Outbox.Write. Add [OutboxRelay] to publish them.
func Outbox(opts ...outbox.Option) fx.Option {
	return fx.Provide(func() *outbox.Outbox { return outbox.New(opts...) })
}

// OutboxRelay publishes the records of the graph's *outbox.Outbox ([Outbox])
// with the graph's *postgres.DB ([Postgres]) and *kafka.Producer
// ([KafkaProducer]). The relay starts with the app and stops in
// graceful.StopIntake, before the producer is flushed and closed in
// graceful.Drain.
//
//	commonfx.Postgres(),
//	commonfx.KafkaProducer(),
//	commonfx.Outbox(),
//	commonfx.OutboxRelay(),
func OutboxRelay(opts ...outbox.RelayOption) fx.Option {
	return fx.Module("commonfx.outbox.relay",
		fx.Provide(func(box *outbox.Outbox, db *postgres.DB, p *kafka.Producer, t *telemetry) *outbox.Relay {
			base := []outbox.RelayOption{outbox.WithLogger(t.logger)}
			if t.mp != nil {
				base = append(base, outbox.WithMeterProvider(t.mp))
			}
			return box.NewRelay(db, p, append(base, opts...)...)
		}),
		fx.Invoke(func(lc fx.Lifecycle, g *graceful.Manager, relay *outbox.Relay) {
			lc.Append(fx.Hook{OnStart: func(context.Context) error {
				if err := g.Register(graceful.StopIntake, "outbox relay", relay.Stop); err != nil {
					return err
				}
				g.Go("outbox relay", func() error { return relay.Run(context.Background()) })
				return nil
			}})
		}),
	)
}
