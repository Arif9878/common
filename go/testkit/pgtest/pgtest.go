// Package pgtest gives tests a PostgreSQL connection from the
// POSTGRES_TEST_URL environment variable
// (postgres://user:password@host:port/database). Tests using it are skipped
// when the variable is unset. It is a separate package so tests that do not
// need PostgreSQL do not compile pgx.
package pgtest

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/testkit"
)

// EnvVar names the connection URL variable.
const EnvVar = "POSTGRES_TEST_URL"

// Config returns a postgres.Config for the database in POSTGRES_TEST_URL,
// with TLS disabled, or skips the test if the variable is unset.
func Config(t testing.TB) postgres.Config {
	t.Helper()
	u, err := url.Parse(testkit.Getenv(t, EnvVar))
	if err != nil {
		t.Fatalf("pgtest: %s: %v", EnvVar, err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	return postgres.Config{
		Host:     u.Hostname(),
		Port:     port,
		Database: strings.TrimPrefix(u.Path, "/"),
		User:     u.User.Username(),
		Password: config.Secret(pw),
		SSLMode:  "disable",
		MaxConns: 30,
	}
}

// DB connects to the database in POSTGRES_TEST_URL (or skips the test) and
// closes the pool when the test ends. Logs are discarded unless opts set a
// logger.
func DB(t testing.TB, opts ...postgres.Option) *postgres.DB {
	t.Helper()
	cfg := Config(t)
	db, err := postgres.New(context.Background(), cfg,
		append([]postgres.Option{postgres.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)...)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}
