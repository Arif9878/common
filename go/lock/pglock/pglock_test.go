package pglock_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/lock/locktest"
	"github.com/Arif9878/common/go/lock/pglock"
)

func TestLocker(t *testing.T) {
	raw := os.Getenv("POSTGRES_TEST_URL")
	if raw == "" {
		t.Skip("POSTGRES_TEST_URL not set")
	}
	u, _ := url.Parse(raw)
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	db, err := postgres.New(context.Background(), postgres.Config{
		Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		User: u.User.Username(), Password: config.Secret(pw), SSLMode: "disable", MaxConns: 30,
	}, postgres.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	if _, err := db.Exec(context.Background(), pglock.Schema); err != nil {
		t.Fatal(err)
	}
	locktest.Run(t, pglock.New(db), time.Sleep, fmt.Sprintf("%d:", time.Now().UnixNano()))
}
