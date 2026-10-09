package migrate_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/datastore/postgres/migrate"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/testkit"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// newDB connects to POSTGRES_TEST_URL. It does not use testkit/pgtest,
// which depends on this module.
func newDB(t *testing.T) *postgres.DB {
	t.Helper()
	u, err := url.Parse(testkit.Getenv(t, "POSTGRES_TEST_URL"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	db, err := postgres.New(context.Background(), postgres.Config{
		Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		User: u.User.Username(), Password: config.Secret(pw), SSLMode: "disable", MaxConns: 10,
	}, postgres.WithLogger(quiet))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// fixture returns migrations creating a table unique to the test, the
// version table to use, and drops both at the end.
func fixture(t *testing.T, db *postgres.DB, second string) (fstest.MapFS, string, string) {
	t.Helper()
	suffix := strings.ToLower(rand.Text()[:8])
	table, versions := "orders_"+suffix, "versions_"+suffix
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DROP TABLE IF EXISTS `+table+`, `+versions)
	})
	if second == "" {
		second = "ALTER TABLE " + table + " ADD COLUMN amount bigint NOT NULL DEFAULT 0;"
	}
	return fstest.MapFS{
		// The sleep keeps the first migration running while other
		// replicas try to migrate.
		"00001_create.sql": {Data: []byte("-- +goose Up\nSELECT pg_sleep(0.5);\nCREATE TABLE " + table + " (id bigserial PRIMARY KEY);\n-- +goose Down\nDROP TABLE " + table + ";\n")},
		"00002_amount.sql": {Data: []byte("-- +goose Up\n" + second + "\n-- +goose Down\nSELECT 1;\n")},
	}, table, versions
}

func TestUpOnceAcrossReplicas(t *testing.T) {
	db := newDB(t)
	fsys, table, versions := fixture(t, db, "")
	ctx := context.Background()

	var mu sync.Mutex
	total := 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 3 { // three replicas starting at once
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			applied, err := migrate.Up(ctx, db, fsys, migrate.WithTableName(versions), migrate.WithLogger(quiet))
			if err != nil {
				t.Errorf("Up: %v", err)
			}
			mu.Lock()
			total += len(applied)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if total != 2 {
		t.Errorf("migrations applied %d times across replicas, want 2", total)
	}
	if _, err := db.Exec(ctx, `INSERT INTO `+table+` (amount) VALUES (5)`); err != nil {
		t.Errorf("schema not migrated: %v", err)
	}
	if pending, err := migrate.Pending(ctx, db, fsys, migrate.WithTableName(versions)); err != nil || pending {
		t.Errorf("Pending after Up = %v, %v", pending, err)
	}
	again, err := migrate.Up(ctx, db, fsys, migrate.WithTableName(versions), migrate.WithLogger(quiet))
	if err != nil || len(again) != 0 {
		t.Errorf("second Up applied %d, %v", len(again), err)
	}
}

func TestFailedMigrationStops(t *testing.T) {
	db := newDB(t)
	fsys, table, versions := fixture(t, db, "ALTER TABLE nope ADD COLUMN x int;")
	ctx := context.Background()

	applied, err := migrate.Up(ctx, db, fsys, migrate.WithTableName(versions), migrate.WithLogger(quiet))
	if err == nil {
		t.Fatal("broken migration succeeded")
	}
	if errors.KindOf(err) == errors.Unknown {
		t.Errorf("error not classified: %v", err)
	}
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Errorf("applied = %+v, want the first migration only", applied)
	}
	if _, err := db.Exec(ctx, `SELECT 1 FROM `+table); err != nil {
		t.Errorf("first migration rolled back: %v", err)
	}
	if pending, _ := migrate.Pending(ctx, db, fsys, migrate.WithTableName(versions)); !pending {
		t.Error("the failed migration is not pending")
	}
}

func TestInvalidMigrations(t *testing.T) {
	db := newDB(t)
	bad := fstest.MapFS{"not_a_migration.sql": {Data: []byte("SELECT 1")}, "00001_x.sql": {Data: []byte("no goose annotations")}}
	if _, err := migrate.Up(context.Background(), db, bad, migrate.WithLogger(quiet)); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("err = %v, want invalid_argument", err)
	}
}
