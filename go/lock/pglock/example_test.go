package pglock_test

import (
	"context"
	"log"
	"time"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lock"
	"github.com/Arif9878/common/go/lock/pglock"
)

// A lease in PostgreSQL, for services without Redis. The fence increases
// with every lease, so a store can reject writes from a holder whose lease
// expired.
func Example() {
	ctx := context.Background()
	var db *postgres.DB // from postgres.New; pglock.Schema applied in a migration
	locker := pglock.New(db)

	lease, err := locker.TryAcquire(ctx, "monthly-invoices", 10*time.Minute)
	if errors.Is(err, lock.ErrNotAcquired) {
		return // another replica is running it
	}
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = lease.Release(context.WithoutCancel(ctx)) }()
	_, _ = db.Exec(ctx, "UPDATE invoice_runs SET fence = $1 WHERE month = $2 AND fence < $1", lease.Fence, "2026-10")
}
