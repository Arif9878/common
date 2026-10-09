package commonfx

import (
	"context"
	"io/fs"
	"time"

	"github.com/hashicorp/vault/api"
	"go.uber.org/fx"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/postgres/migrate"
	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/health"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/messaging/kafka"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/vault"
)

// Option value groups. Contribute options computed from other
// dependencies (for example credential rotation through Vault) with
// fx.Annotate and fx.ResultTags:
//
//	fx.Provide(fx.Annotate(
//		func(v *vault.Client) postgres.Option {
//			return postgres.WithCredentials(v.Fetcher("database/creds/orders"), v.Revoke)
//		},
//		fx.ResultTags(`group:"commonfx.postgres.options"`),
//	))
const (
	PostgresOptions = `group:"commonfx.postgres.options"`
	RedisOptions    = `group:"commonfx.redis.options"`
	VaultOptions    = `group:"commonfx.vault.options"`
	KafkaOptions    = `group:"commonfx.kafka.options"`
)

func connectCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), ConnectTimeout)
}

type postgresIn struct {
	fx.In
	Config  postgres.Config
	Options []postgres.Option `group:"commonfx.postgres.options"`
}

// Postgres provides *postgres.DB from postgres.Config, connecting while the
// graph is built. It is closed in graceful.CloseDeps and added to the
// readiness checks.
func Postgres() fx.Option {
	return fx.Module("commonfx.postgres",
		fx.Provide(func(in postgresIn, g *graceful.Manager, checks *health.Checker, t *telemetry) (*postgres.DB, error) {
			ctx, cancel := connectCtx()
			defer cancel()
			db, err := postgres.New(ctx, in.Config, append(t.postgres(), in.Options...)...)
			if err != nil {
				return nil, err
			}
			checks.AddReadiness("postgres", db.Ping)
			return db, g.Register(graceful.CloseDeps, "postgres", db.Close)
		}),
	)
}

type redisIn struct {
	fx.In
	Config  redis.Config
	Options []redis.Option `group:"commonfx.redis.options"`
}

// Redis provides *redis.Client from redis.Config, connecting while the
// graph is built. It is stopped in graceful.CloseDeps and added to the
// readiness checks.
func Redis() fx.Option {
	return fx.Module("commonfx.redis",
		fx.Provide(func(in redisIn, g *graceful.Manager, checks *health.Checker, t *telemetry) (*redis.Client, error) {
			ctx, cancel := connectCtx()
			defer cancel()
			c, err := redis.New(ctx, in.Config, append(t.redis(), in.Options...)...)
			if err != nil {
				return nil, err
			}
			checks.AddReadiness("redis", c.HealthCheck)
			return c, g.Register(graceful.CloseDeps, "redis", c.Stop)
		}),
	)
}

type vaultIn struct {
	fx.In
	Config  vault.Config
	Auth    api.AuthMethod `optional:"true"`
	Options []vault.Option `group:"commonfx.vault.options"`
}

// Vault provides *vault.Client and secret.Provider from vault.Config,
// logging in while the graph is built (with the api.AuthMethod in the
// graph, if any, else Config.Token). It is closed in graceful.CloseDeps.
func Vault() fx.Option {
	return fx.Module("commonfx.vault",
		fx.Provide(
			func(in vaultIn, g *graceful.Manager, t *telemetry) (*vault.Client, error) {
				ctx, cancel := connectCtx()
				defer cancel()
				opts := append(t.vault(), in.Options...)
				if in.Auth != nil {
					opts = append(opts, vault.WithAuth(in.Auth))
				}
				c, err := vault.New(ctx, in.Config, opts...)
				if err != nil {
					return nil, err
				}
				return c, g.Register(graceful.CloseDeps, "vault", c.Close)
			},
			func(c *vault.Client) secret.Provider { return c },
		),
	)
}

type kafkaIn struct {
	fx.In
	Config  kafka.Config
	Options []kafka.Option `group:"commonfx.kafka.options"`
}

// KafkaProducer provides *kafka.Producer from kafka.Config. It is flushed
// and closed in graceful.Drain, after consumers stopped in StopIntake.
func KafkaProducer() fx.Option {
	return fx.Module("commonfx.kafka.producer",
		fx.Provide(func(in kafkaIn, g *graceful.Manager, t *telemetry) (*kafka.Producer, error) {
			ctx, cancel := connectCtx()
			defer cancel()
			p, err := kafka.NewProducer(ctx, in.Config, append(t.kafka(), in.Options...)...)
			if err != nil {
				return nil, err
			}
			return p, g.Register(graceful.Drain, "kafka producer", p.Close)
		}),
	)
}

// PostgresMigrations applies the goose migrations in fsys to the graph's
// PostgreSQL ([Postgres]) while the application is built, so it starts
// (and reports ready) only on the migrated schema. Replicas starting
// together take turns through an advisory lock; see the migrate package.
//
//	//go:embed migrations/*.sql
//	var migrations embed.FS
//
//	sub, _ := fs.Sub(migrations, "migrations")
//	commonfx.PostgresMigrations(sub),
func PostgresMigrations(fsys fs.FS, opts ...migrate.Option) fx.Option {
	return fx.Invoke(func(db *postgres.DB, t *telemetry) error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		_, err := migrate.Up(ctx, db, fsys, append([]migrate.Option{migrate.WithLogger(t.logger)}, opts...)...)
		return err
	})
}
