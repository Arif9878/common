package commonfx

import (
	"context"

	"go.uber.org/fx"

	"github.com/Arif9878/common/go/concurrency/schedule"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/lock"
	"github.com/Arif9878/common/go/lock/pglock"
	"github.com/Arif9878/common/go/lock/redislock"
)

// RedisLocker provides a lock.Locker in the graph's Redis ([Redis]), for
// [Scheduler] and for leases in handlers.
func RedisLocker(opts ...redislock.Option) fx.Option {
	return fx.Provide(func(c *redis.Client) lock.Locker { return redislock.New(c, opts...) })
}

// PostgresLocker provides a lock.Locker in the graph's PostgreSQL
// ([Postgres]). Create its table with pglock.Schema.
func PostgresLocker(opts ...pglock.Option) fx.Option {
	return fx.Provide(func(db *postgres.DB) lock.Locker { return pglock.New(db, opts...) })
}

type schedulerIn struct {
	fx.In
	Locker lock.Locker `optional:"true"`
}

// Scheduler provides a *schedule.Scheduler. With a lock.Locker in the
// graph ([RedisLocker], [PostgresLocker]) each job runs on one replica at
// a time; without one, on every replica. Add jobs in fx.Invoke; the
// scheduler starts with the app and stops in graceful.StopIntake, waiting
// for runs in progress.
//
//	commonfx.RedisLocker(),
//	commonfx.Scheduler(),
//	fx.Invoke(func(s *schedule.Scheduler, r *Reports) error {
//		return s.Cron("reports.daily", "0 2 * * *", r.BuildDaily)
//	}),
func Scheduler(opts ...schedule.Option) fx.Option {
	return fx.Module("commonfx.schedule",
		fx.Provide(func(in schedulerIn, t *telemetry) *schedule.Scheduler {
			base := []schedule.Option{schedule.WithLogger(t.logger)}
			if t.mp != nil {
				base = append(base, schedule.WithMeterProvider(t.mp))
			}
			if t.tp != nil {
				base = append(base, schedule.WithTracerProvider(t.tp))
			}
			if in.Locker != nil {
				base = append(base, schedule.WithLocker(in.Locker))
			}
			return schedule.New(append(base, opts...)...)
		}),
		fx.Invoke(func(lc fx.Lifecycle, g *graceful.Manager, s *schedule.Scheduler) {
			lc.Append(fx.Hook{OnStart: func(context.Context) error {
				if err := g.Register(graceful.StopIntake, "scheduler", s.Stop); err != nil {
					return err
				}
				g.Go("scheduler", func() error { return s.Run(context.Background()) })
				return nil
			}})
		}),
	)
}
