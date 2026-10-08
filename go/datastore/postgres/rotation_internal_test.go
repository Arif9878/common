package postgres

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

	"github.com/Arif9878/common/go/testkit"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
)

// TestOperationRacingRotationIsRetried forces the window a stress test
// rarely hits: an operation reads the current pool, the pool is replaced
// and closed, and only then does the operation acquire from it.
func TestOperationRacingRotationIsRetried(t *testing.T) {
	raw := os.Getenv("POSTGRES_TEST_URL")
	if raw == "" {
		t.Skip("POSTGRES_TEST_URL not set")
	}
	u, _ := url.Parse(raw)
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	creds := func(context.Context) (secret.Secret, error) {
		return secret.New(map[string]string{"username": u.User.Username(), "password": pw}), nil
	}
	db, err := New(context.Background(), Config{
		Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"), SSLMode: "disable",
	}, WithLogger(quiet), WithCredentials(creds, nil, rotation.WithLogger(quiet)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()

	attempts := 0
	_, err = withPool(db, func(p *pgxpool.Pool) (struct{}, error) {
		attempts++
		if attempts == 1 {
			if err := db.Rotate(context.Background()); err != nil {
				t.Fatal(err)
			}
			testkit.Eventually(t, 5*time.Second, "the old pool to close", func() bool {
				return errors.Is(p.Ping(context.Background()), puddle.ErrClosedPool)
			})
		}
		_, err := p.Exec(context.Background(), "SELECT 1")
		return struct{}{}, err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("err = %v after %d attempts; want success on the new pool", err, attempts)
	}
}

func TestAcquisitionFailed(t *testing.T) {
	connectErr := &pgconn.ConnectError{}
	tests := []struct {
		err  error
		want bool
	}{
		{puddle.ErrClosedPool, true},
		{fmt.Errorf("acquire: %w", puddle.ErrClosedPool), true},
		{connectErr, true},
		{fmt.Errorf("query: %w", connectErr), true},
		{&pgconn.PgError{Code: "23505"}, false}, // the statement ran
		{context.DeadlineExceeded, false},
		{io.ErrUnexpectedEOF, false}, // connection broke mid-query: may have run
	}
	for _, tt := range tests {
		if got := acquisitionFailed(tt.err); got != tt.want {
			t.Errorf("acquisitionFailed(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
