package commonfx

import (
	"go.uber.org/fx"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/idempotency/pgstore"
	"github.com/Arif9878/common/go/idempotency/redisstore"
	"github.com/Arif9878/common/go/messaging/kafka"
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
