package schedule_test

import (
	"context"
	"log"
	"time"

	"github.com/Arif9878/common/go/concurrency/schedule"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/lock"
)

func Example() {
	shutdown := graceful.New()
	var locker lock.Locker // redislock.New(…) or pglock.New(…): one replica runs each job

	s := schedule.New(schedule.WithLocker(locker))
	if err := s.Every("expire-carts", 5*time.Minute, expireCarts, schedule.WithTimeout(time.Minute)); err != nil {
		log.Fatal(err)
	}
	if err := s.Cron("daily-report", "0 6 * * *", sendReport); err != nil { // 06:00 every day
		log.Fatal(err)
	}
	go func() { _ = s.Run(context.Background()) }()
	_ = shutdown.Register(graceful.StopIntake, "schedule", s.Stop)
}

func expireCarts(context.Context) error { return nil }

func sendReport(context.Context) error { return nil }
