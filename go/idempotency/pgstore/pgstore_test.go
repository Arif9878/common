package pgstore_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Arif9878/common/go/testkit/pgtest"

	"github.com/jackc/pgx/v5"

	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/idempotency"
	"github.com/Arif9878/common/go/idempotency/idempotencytest"
	"github.com/Arif9878/common/go/idempotency/pgstore"
)

func newDB(t *testing.T) *postgres.DB {
	t.Helper()
	db := pgtest.DB(t)
	if _, err := db.Exec(context.Background(), pgstore.Schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestStore(t *testing.T) {
	db := newDB(t)
	prefix := fmt.Sprintf("%d:", time.Now().UnixNano())
	idempotencytest.Run(t, pgstore.New(db), time.Sleep, prefix)

	store := pgstore.New(db)
	if _, _, _, err := store.Begin(context.Background(), prefix+"short-lived", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	n, err := store.DeleteExpired(context.Background())
	if err != nil || n < 1 {
		t.Errorf("DeleteExpired = %d, %v", n, err)
	}
}

func TestDoTxExactlyOnce(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	table := fmt.Sprintf("charges_%d", time.Now().UnixNano())
	if _, err := db.Exec(ctx, "CREATE TABLE "+table+" (id serial PRIMARY KEY, order_id text)"); err != nil {
		t.Fatal(err)
	}
	store := pgstore.New(db)
	key := "charge:" + table

	charge := func(ctx context.Context, tx pgx.Tx) (int, error) {
		var id int
		err := tx.QueryRow(ctx, "INSERT INTO "+table+" (order_id) VALUES ('o-1') RETURNING id").Scan(&id)
		time.Sleep(50 * time.Millisecond) // widen the race window
		return id, err
	}

	var wg sync.WaitGroup
	ids := make([]int, 10)
	errs := make([]error, 10)
	for i := range ids {
		wg.Go(func() { ids[i], _, errs[i] = pgstore.DoTx(ctx, db, store, key, time.Hour, charge) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || ids[i] != ids[0] {
			t.Fatalf("call %d: id %d err %v; want all %d", i, ids[i], err, ids[0])
		}
	}
	var rows int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d, %v; want exactly one charge", rows, err)
	}

	// A failing fn rolls back its writes and the claim.
	boom := stderrors.New("card declined")
	_, _, err := pgstore.DoTx(ctx, db, store, key+":2", time.Hour, func(ctx context.Context, tx pgx.Tx) (int, error) {
		if _, err := tx.Exec(ctx, "INSERT INTO "+table+" (order_id) VALUES ('o-2')"); err != nil {
			return 0, err
		}
		return 0, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d after failure; the write was not rolled back", rows)
	}
	v, out, err := pgstore.DoTx(ctx, db, store, key+":2", time.Hour, charge)
	if err != nil || out != idempotency.Executed || v == 0 {
		t.Fatalf("retry after rollback = %v, %v, %v", v, out, err)
	}
}
