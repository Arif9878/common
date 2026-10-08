package redis_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Arif9878/common/go/config"
	"github.com/Arif9878/common/go/datastore/redis"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newClient(t *testing.T, cfg redis.Config, opts ...redis.Option) *redis.Client {
	t.Helper()
	c, err := redis.New(context.Background(), cfg, append([]redis.Option{redis.WithLogger(quiet)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c
}

func TestCommandsAndClassify(t *testing.T) {
	m := miniredis.RunT(t)
	c := newClient(t, redis.Config{Addrs: []string{m.Addr()}})
	ctx := context.Background()

	if err := c.Set(ctx, "k", "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Get(ctx, "k").Result(); err != nil || v != "v" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	_, err := c.Get(ctx, "missing").Result()
	if errors.KindOf(redis.Classify(err)) != errors.NotFound {
		t.Errorf("missing key: %v", redis.Classify(err))
	}
	if err := c.HealthCheck(ctx); err != nil {
		t.Fatal(err)
	}
	// The embedded client is a full go-redis client.
	var _ goredis.UniversalClient = c
}

type redisErr string

func (e redisErr) Error() string { return string(e) }
func (redisErr) RedisError()     {}

func TestClassifyTable(t *testing.T) {
	tests := []struct {
		err  error
		want errors.Kind
	}{
		{goredis.Nil, errors.NotFound},
		{goredis.ErrPoolTimeout, errors.Unavailable},
		{redisErr("WRONGPASS invalid username-password pair or user is disabled."), errors.Unauthorized},
		{redisErr("NOAUTH Authentication required."), errors.Unauthorized},
		{redisErr("NOPERM this user has no permissions to run the 'flushall' command"), errors.Forbidden},
		{redisErr("LOADING Redis is loading the dataset in memory"), errors.Unavailable},
		{redisErr("CLUSTERDOWN The cluster is down"), errors.Unavailable},
		{redisErr("WRONGTYPE Operation against a key holding the wrong kind of value"), errors.Internal},
		{context.DeadlineExceeded, errors.Timeout},
		{io.EOF, errors.Unavailable},
	}
	for _, tt := range tests {
		if got := errors.KindOf(redis.Classify(tt.err)); got != tt.want {
			t.Errorf("Classify(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
	if redis.Classify(nil) != nil {
		t.Error("Classify(nil) != nil")
	}
}

func TestRetriesOffByDefault(t *testing.T) {
	m := miniredis.RunT(t)
	var opts *goredis.UniversalOptions
	newClient(t, redis.Config{Addrs: []string{m.Addr()}}, redis.WithConfigure(func(o *goredis.UniversalOptions) { opts = o }))
	if opts.MaxRetries != -1 {
		t.Errorf("MaxRetries = %d, want -1 (disabled)", opts.MaxRetries)
	}
	newClient(t, redis.Config{Addrs: []string{m.Addr()}, MaxRetries: 2}, redis.WithConfigure(func(o *goredis.UniversalOptions) { opts = o }))
	if opts.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want 2", opts.MaxRetries)
	}
}

func TestStaticAuth(t *testing.T) {
	m := miniredis.RunT(t)
	m.RequireUserAuth("app", "right-password")

	c := newClient(t, redis.Config{Addrs: []string{m.Addr()}, Username: "app", Password: "right-password"})
	if err := c.Set(context.Background(), "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}

	_, err := redis.New(context.Background(), redis.Config{
		Addrs: []string{m.Addr()}, Username: "app", Password: config.Secret("wrong-password-value"),
	}, redis.WithLogger(quiet))
	if errors.KindOf(err) != errors.Unauthorized || strings.Contains(err.Error(), "wrong-password-value") {
		t.Fatalf("err = %v (kind %v)", err, errors.KindOf(err))
	}
}

func TestUnreachable(t *testing.T) {
	_, err := redis.New(context.Background(), redis.Config{Addrs: []string{"127.0.0.1:1"}, DialTimeout: 200 * time.Millisecond}, redis.WithLogger(quiet))
	if errors.KindOf(err) != errors.Unavailable {
		t.Fatalf("err = %v (kind %v)", err, errors.KindOf(err))
	}
}

// users issues a new ACL user per fetch, like Vault's Redis database
// engine, and records revocations. It does not change miniredis users on
// revoke: revocation runs on a timer, and miniredis reads its user table
// in HELLO without the lock RequireUserAuth takes, which the race detector
// reports when a client connects at the same moment.
type users struct {
	m       *miniredis.Miniredis
	seq     atomic.Int32
	mu      sync.Mutex
	revoked []string
	at      []time.Time
}

func (u *users) fetch(context.Context) (secret.Secret, error) {
	n := u.seq.Add(1)
	user, pw := fmt.Sprintf("user%d", n), fmt.Sprintf("pw-%d", n)
	u.m.RequireUserAuth(user, pw)
	s := secret.New(map[string]string{"username": user, "password": pw})
	s.ExpiresAt = time.Now().Add(time.Hour)
	return s, nil
}

func (u *users) revoke(_ context.Context, s secret.Secret) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.revoked = append(u.revoked, s.Field("username"))
	u.at = append(u.at, time.Now())
	return nil
}

func TestCredentialRotation(t *testing.T) {
	m := miniredis.RunT(t)
	u := &users{m: m}
	const lifetime = 300 * time.Millisecond
	c := newClient(t, redis.Config{Addrs: []string{m.Addr()}, ConnMaxLifetime: lifetime},
		redis.WithCredentials(u.fetch, u.revoke, rotation.WithLogger(quiet)))
	ctx := context.Background()

	if err := c.Set(ctx, "k", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	rotatedAt := time.Now()
	if err := c.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	// Invalidate user1 behind the client's back: new connections must
	// already use user2.
	m.RequireUserAuth("user1", "changed")
	time.Sleep(lifetime + 50*time.Millisecond) // old connections expire
	if err := c.Incr(ctx, "k").Err(); err != nil {
		t.Fatalf("command after rotation: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		n := len(u.revoked)
		u.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	u.mu.Lock()
	if len(u.revoked) != 1 || u.revoked[0] != "user1" || u.at[0].Sub(rotatedAt) < lifetime {
		t.Errorf("revoked %v at %v after rotation; want user1 after at least %v", u.revoked, u.at, lifetime)
	}
	u.mu.Unlock()

	// Stop revokes the current credential and pending ones at once.
	if err := c.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if strings.Join(u.revoked, ",") != "user1,user3,user2" && strings.Join(u.revoked, ",") != "user1,user2,user3" {
		t.Errorf("revoked after stop: %v", u.revoked)
	}
}

func TestRotationRejectsBadCredentials(t *testing.T) {
	m := miniredis.RunT(t)
	u := &users{m: m}
	c := newClient(t, redis.Config{Addrs: []string{m.Addr()}},
		redis.WithCredentials(func(ctx context.Context) (secret.Secret, error) {
			if u.seq.Load() >= 1 {
				return secret.New(map[string]string{"username": "ghost", "password": "nope"}), nil
			}
			return u.fetch(ctx)
		}, nil, rotation.WithLogger(quiet)))

	if err := c.Rotate(context.Background()); errors.KindOf(err) != errors.Unauthorized {
		t.Fatalf("Rotate with bad credentials = %v", err)
	}
	if err := c.Set(context.Background(), "k", "v", 0).Err(); err != nil {
		t.Fatalf("current credentials no longer work: %v", err)
	}
	if err := (&redis.Client{}).Rotate(context.Background()); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("Rotate without rotation = %v", err)
	}
}

func TestTelemetryOmitsArguments(t *testing.T) {
	m := miniredis.RunT(t)
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	c := newClient(t, redis.Config{Addrs: []string{m.Addr()}},
		redis.WithTracerProvider(tp), redis.WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))

	ctx, parent := tp.Tracer("test").Start(context.Background(), "request")
	_ = c.Set(ctx, "session:user-42", "secret-session-value", 0).Err()
	parent.End()

	var commandSpans int
	for _, s := range spans.Ended() {
		if s.Name() == "request" {
			continue
		}
		commandSpans++
		for _, kv := range s.Attributes() {
			if v := kv.Value.String(); strings.Contains(v, "secret-session-value") || strings.Contains(v, "user-42") {
				t.Fatalf("span %q records argument %s=%s", s.Name(), kv.Key, v)
			}
		}
	}
	if commandSpans == 0 {
		t.Error("no command span recorded")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	if len(rm.ScopeMetrics) == 0 {
		t.Error("no redis metrics recorded")
	}
}

func TestStopIdempotent(t *testing.T) {
	m := miniredis.RunT(t)
	c, err := redis.New(context.Background(), redis.Config{Addrs: []string{m.Addr()}}, redis.WithLogger(quiet))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Ping(context.Background()).Err(); !stderrors.Is(err, goredis.ErrClosed) {
		t.Errorf("Ping after Stop = %v", err)
	}
}
