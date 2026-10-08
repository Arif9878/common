package postgres_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/postgres"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestClassify(t *testing.T) {
	tests := []struct {
		err  error
		want errors.Kind
	}{
		{pgx.ErrNoRows, errors.NotFound},
		{fmt.Errorf("scan: %w", pgx.ErrNoRows), errors.NotFound},
		{&pgconn.PgError{Code: "23505"}, errors.Conflict},
		{&pgconn.PgError{Code: "23503"}, errors.InvalidArgument},
		{&pgconn.PgError{Code: "40001"}, errors.Unavailable},
		{&pgconn.PgError{Code: "40P01"}, errors.Unavailable},
		{&pgconn.PgError{Code: "57014"}, errors.Timeout},
		{&pgconn.PgError{Code: "28P01"}, errors.Unauthorized},
		{&pgconn.PgError{Code: "42P01"}, errors.Internal}, // undefined table: a bug
		{context.DeadlineExceeded, errors.Timeout},
		{context.Canceled, errors.Canceled},
		{io.ErrUnexpectedEOF, errors.Unavailable},
	}
	for _, tt := range tests {
		if got := errors.KindOf(postgres.Classify(tt.err)); got != tt.want {
			t.Errorf("Classify(%v) kind = %v, want %v", tt.err, got, tt.want)
		}
	}
	if postgres.Classify(nil) != nil {
		t.Error("Classify(nil) != nil")
	}
	if !errors.IsRetryable(postgres.Classify(&pgconn.PgError{Code: "40001"})) {
		t.Error("serialization failure not retryable")
	}
}

func TestConfigValidation(t *testing.T) {
	_, err := postgres.New(context.Background(), postgres.Config{Database: "app", SSLMode: "sometimes"})
	if errors.KindOf(err) != errors.InvalidArgument || !strings.Contains(err.Error(), "SSLMode") {
		t.Errorf("invalid sslmode: %v", err)
	}
	_, err = postgres.New(context.Background(), postgres.Config{Database: "app", Port: 70000})
	if errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("invalid port: %v", err)
	}
}

func TestUnreachableDoesNotLeakPassword(t *testing.T) {
	const pw = "s3cr3t-pw-value"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := postgres.New(ctx, postgres.Config{
		Host: "127.0.0.1", Port: 1, Database: "app", User: "app", Password: config.Secret(pw),
		SSLMode: "disable", ConnectTimeout: time.Second,
	}, postgres.WithLogger(quiet))
	if err == nil {
		t.Fatal("connected to port 1")
	}
	if errors.KindOf(err) != errors.Unavailable || strings.Contains(err.Error(), pw) {
		t.Fatalf("err = %v (kind %v)", err, errors.KindOf(err))
	}
}

// testConfig returns a Config from POSTGRES_TEST_URL
// (postgres://user:pass@host:port/db) or skips the test.
func testConfig(t *testing.T) postgres.Config {
	t.Helper()
	raw := os.Getenv("POSTGRES_TEST_URL")
	if raw == "" {
		t.Skip("POSTGRES_TEST_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	return postgres.Config{
		Host: u.Hostname(), Port: port, Database: strings.TrimPrefix(u.Path, "/"),
		User: u.User.Username(), Password: config.Secret(pw), SSLMode: "disable", MaxConns: 5,
	}
}

func newDB(t *testing.T, cfg postgres.Config, opts ...postgres.Option) *postgres.DB {
	t.Helper()
	db, err := postgres.New(context.Background(), cfg, append([]postgres.Option{postgres.WithLogger(quiet)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

func TestQueriesAndTelemetry(t *testing.T) {
	cfg := testConfig(t)
	reader := sdkmetric.NewManualReader()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	db := newDB(t, cfg,
		postgres.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
		postgres.WithTracerProvider(tp))
	// Query spans are children of the caller's span, as inside a request.
	ctx, parent := tp.Tracer("test").Start(context.Background(), "request")
	defer parent.End()

	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TEMP TABLE kv (k text PRIMARY KEY, v text)`); err != nil {
		t.Fatal(err)
	}
	// Temp tables are per connection; use one transaction.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE kv2 (k text PRIMARY KEY, v text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kv2 VALUES ($1, $2)`, "a", "secret-param-value"); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO kv2 VALUES ($1, $2)`, "a", "x")
	if errors.KindOf(postgres.Classify(err)) != errors.Conflict {
		t.Errorf("duplicate key: %v", err)
	}
	_ = tx.Rollback(ctx)

	var v string
	err = db.QueryRow(ctx, `SELECT v FROM kv WHERE k = $1`, "missing").Scan(&v)
	if errors.KindOf(postgres.Classify(err)) != errors.NotFound {
		t.Errorf("no rows: %v", err)
	}

	var sawSQL bool
	for _, s := range spans.Ended() {
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), "secret-param-value") {
				t.Fatalf("query parameter recorded in span %q", s.Name())
			}
			if strings.Contains(kv.Value.String(), "INSERT INTO kv2") {
				sawSQL = true
			}
		}
	}
	if !sawSQL {
		t.Error("no span with the SQL statement")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "db.client.connection.max" {
				found = m.Data.(metricdata.Sum[int64]).DataPoints[0].Value == 5
			}
		}
	}
	if !found {
		t.Error("pool max-connections metric missing")
	}
}

func TestStatementTimeout(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatementTimeout = 100 * time.Millisecond
	db := newDB(t, cfg)
	_, err := db.Exec(context.Background(), `SELECT pg_sleep(2)`)
	if errors.KindOf(postgres.Classify(err)) != errors.Timeout {
		t.Fatalf("err = %v", err)
	}
}

func TestWrongPassword(t *testing.T) {
	cfg := testConfig(t)
	cfg.Password = "wrong-password-value"
	_, err := postgres.New(context.Background(), cfg, postgres.WithLogger(quiet))
	if errors.KindOf(err) != errors.Unauthorized || strings.Contains(err.Error(), "wrong-password-value") {
		t.Fatalf("err = %v (kind %v)", err, errors.KindOf(err))
	}
}

// roles issues a new login role per fetch, like Vault's database engine,
// and drops it on revoke.
type roles struct {
	admin   *postgres.DB
	seq     atomic.Int32
	mu      sync.Mutex
	revoked []string
}

func (r *roles) fetch(ctx context.Context) (secret.Secret, error) {
	n := r.seq.Add(1)
	user := fmt.Sprintf("rot_user_%d_%d", os.Getpid(), n)
	pw := fmt.Sprintf("pw-%d-%d", time.Now().UnixNano(), n)
	if _, err := r.admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, user, pw)); err != nil {
		return secret.Secret{}, err
	}
	s := secret.New(map[string]string{"username": user, "password": pw})
	s.ExpiresAt = time.Now().Add(time.Hour)
	return s, nil
}

func (r *roles) revoke(ctx context.Context, s secret.Secret) error {
	user := s.Field("username")
	r.mu.Lock()
	r.revoked = append(r.revoked, user)
	r.mu.Unlock()
	_, err := r.admin.Exec(ctx, fmt.Sprintf(`DROP ROLE %s`, user))
	return err
}

func TestCredentialRotation(t *testing.T) {
	cfg := testConfig(t)
	r := &roles{admin: newDB(t, cfg)}
	cfg.User, cfg.Password = "", ""
	db := newDB(t, cfg, postgres.WithCredentials(r.fetch, r.revoke,
		rotation.WithLogger(quiet), rotation.WithDrainTimeout(10*time.Second)))
	ctx := context.Background()

	currentUser := func() string {
		var u string
		if err := db.QueryRow(ctx, `SELECT current_user`).Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	first := currentUser()

	// A transaction on the old pool survives the rotation.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Constant load while rotating.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var queries, failures atomic.Int64
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				var one int
				if err := db.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
					failures.Add(1)
					t.Errorf("query during rotation: %v", err)
				}
				queries.Add(1)
			}
		})
	}
	for range 3 {
		if err := db.Rotate(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if u := currentUser(); u == first {
		t.Fatalf("still connected as %s after rotation", u)
	}
	var txUser string
	if err := tx.QueryRow(ctx, `SELECT current_user`).Scan(&txUser); err != nil || txUser != first {
		t.Fatalf("open transaction broke: user %q, %v", txUser, err)
	}
	r.mu.Lock()
	for _, u := range r.revoked {
		if u == first {
			t.Fatalf("role %s revoked while its transaction was open", first)
		}
	}
	r.mu.Unlock()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// With the transaction done, the first pool closes and its role is revoked.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.revoked)
		r.mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.revoked) != 3 || r.revoked[2] != first {
		t.Errorf("revoked %v, want the 3 replaced roles, %s last (after its transaction)", r.revoked, first)
	}
	t.Logf("%d queries, %d failures across 3 rotations", queries.Load(), failures.Load())
}

func TestCloseWithLeakedConnection(t *testing.T) {
	cfg := testConfig(t)
	db, err := postgres.New(context.Background(), cfg, postgres.WithLogger(quiet))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Pool().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = db.Close(ctx)
	if errors.KindOf(err) != errors.Timeout || time.Since(start) > time.Second {
		t.Fatalf("Close = %v after %v", err, time.Since(start))
	}
	conn.Release() // lets the background close finish
}

func TestRotateWithoutCredentials(t *testing.T) {
	db := newDB(t, testConfig(t))
	if err := db.Rotate(context.Background()); errors.KindOf(err) != errors.InvalidArgument {
		t.Fatalf("Rotate = %v", err)
	}
}
