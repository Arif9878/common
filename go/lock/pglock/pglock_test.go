package pglock_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Arif9878/common/go/testkit/pgtest"

	"github.com/Arif9878/common/go/lock/locktest"
	"github.com/Arif9878/common/go/lock/pglock"
)

func TestLocker(t *testing.T) {
	db := pgtest.DB(t)
	if _, err := db.Exec(context.Background(), pglock.Schema); err != nil {
		t.Fatal(err)
	}
	locktest.Run(t, pglock.New(db), time.Sleep, fmt.Sprintf("%d:", time.Now().UnixNano()))
}
