package ratelimit_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/datastore/redis/ratelimit"
	"github.com/Arif9878/common/go/errors"
	localrl "github.com/Arif9878/common/go/resilience/ratelimit"
	"github.com/Arif9878/common/go/testkit"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var start = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*miniredis.Miniredis, *goredis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(start)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func allow(t *testing.T, l *ratelimit.Limiter, key string) ratelimit.Result {
	t.Helper()
	res, err := l.Allow(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBurstThenRate(t *testing.T) {
	mr, rdb := setup(t)
	l := ratelimit.New(rdb, "api", 2, 3) // 2/s, bursts of 3

	for i := range 3 {
		if res := allow(t, l, "tenant-a"); !res.Allowed || res.Remaining != 2-i {
			t.Fatalf("request %d: %+v, want allowed with %d remaining", i, res, 2-i)
		}
	}
	res := allow(t, l, "tenant-a")
	if res.Allowed || res.RetryAfter != 500*time.Millisecond {
		t.Fatalf("4th request: %+v, want limited for 500ms", res)
	}
	if res := allow(t, l, "tenant-b"); !res.Allowed {
		t.Error("another key is limited too")
	}

	mr.SetTime(start.Add(500 * time.Millisecond))
	if res := allow(t, l, "tenant-a"); !res.Allowed {
		t.Errorf("after 500ms: %+v, want one more allowed", res)
	}
	if res := allow(t, l, "tenant-a"); res.Allowed {
		t.Errorf("second request after 500ms: %+v, want limited", res)
	}
}

func TestReplicasShareTheBudget(t *testing.T) {
	_, rdb := setup(t)
	a, b := ratelimit.New(rdb, "api", 1, 2), ratelimit.New(rdb, "api", 1, 2)
	if !allow(t, a, "k").Allowed || !allow(t, b, "k").Allowed {
		t.Fatal("the burst is not allowed")
	}
	if allow(t, a, "k").Allowed || allow(t, b, "k").Allowed {
		t.Error("two replicas together exceeded the burst")
	}
}

func TestKeysExpireWhenFull(t *testing.T) {
	mr, rdb := setup(t)
	l := ratelimit.New(rdb, "api", 10, 5, ratelimit.WithPrefix("rl:"))
	allow(t, l, "k")
	if ttl := mr.TTL("rl:api:k"); ttl <= 0 || ttl > 100*time.Millisecond {
		t.Errorf("TTL = %s, want the time to refill one token (100ms)", ttl)
	}
}

func TestAllowNAboveBurst(t *testing.T) {
	_, rdb := setup(t)
	l := ratelimit.New(rdb, "api", 10, 5)
	if res, _ := l.AllowN(context.Background(), "k", 6); res.Allowed {
		t.Error("a cost above the burst was allowed")
	}
	if res, _ := l.AllowN(context.Background(), "k", 5); !res.Allowed || res.Remaining != 0 {
		t.Errorf("a cost equal to the burst: %+v", res)
	}
}

func TestZeroRateAllowsNothing(t *testing.T) {
	_, rdb := setup(t)
	if allow(t, ratelimit.New(rdb, "off", 0, 10), "k").Allowed {
		t.Error("a zero rate allowed a request")
	}
}

func TestTakeError(t *testing.T) {
	_, rdb := setup(t)
	l := ratelimit.New(rdb, "api", 1, 1)
	ctx := context.Background()
	if err := l.Take(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	err := l.Take(ctx, "k")
	le, ok := errors.AsType[*localrl.LimitedError](err)
	if errors.KindOf(err) != errors.RateLimited || !ok || le.RetryAfter() != time.Second || !errors.Is(err, localrl.ErrLimited) {
		t.Errorf("Take = %v, want a RateLimited LimitedError retrying after 1s", err)
	}
}

func TestRedisDown(t *testing.T) {
	mr, rdb := setup(t)
	mp, metrics := testkit.NewMetrics(t)
	open := ratelimit.New(rdb, "open", 1, 1, ratelimit.WithMeterProvider(mp), ratelimit.WithLogger(quiet))
	closed := ratelimit.New(rdb, "closed", 1, 1, ratelimit.WithFailClosed(), ratelimit.WithLogger(quiet))
	mr.Close()

	if err := open.Take(context.Background(), "k"); err != nil {
		t.Errorf("fail-open limiter: %v", err)
	}
	if n := metrics.Sum("ratelimit.requests", attribute.String("limiter", "open"), attribute.String("outcome", "error")); n != 1 {
		t.Errorf("error outcome counted %v times", n)
	}
	if err := closed.Take(context.Background(), "k"); errors.KindOf(err) != errors.Unavailable {
		t.Errorf("fail-closed limiter: %v, want Unavailable", err)
	}
}

func TestMiddleware(t *testing.T) {
	_, rdb := setup(t)
	l := ratelimit.New(rdb, "api", 1, 1)
	h := ratelimit.Middleware(l, func(r *http.Request) (string, bool) {
		k := r.Header.Get("X-Tenant-ID")
		return k, k != ""
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	do := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		if tenant != "" {
			req.Header.Set("X-Tenant-ID", tenant)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("t1"); rec.Code != http.StatusNoContent {
		t.Fatalf("first request: %d", rec.Code)
	}
	rec := do("t1")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("second request: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	for range 3 {
		if rec := do(""); rec.Code != http.StatusNoContent {
			t.Errorf("request without a key: %d", rec.Code)
		}
	}
}

// TestOnRedis runs the script on the server in REDIS_TEST_ADDR, so it is
// checked against real Redis (its TIME and number formatting), not only
// miniredis.
func TestOnRedis(t *testing.T) {
	rdb := goredis.NewClient(&goredis.Options{Addr: testkit.Getenv(t, "REDIS_TEST_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("REDIS_TEST_ADDR: %v", err)
	}
	prefix := "commontest:" + strings.ToLower(rand.Text()[:8]) + ":"
	a := ratelimit.New(rdb, "api", 10, 3, ratelimit.WithPrefix(prefix), ratelimit.WithRedisTimeout(time.Second))
	b := ratelimit.New(rdb, "api", 10, 3, ratelimit.WithPrefix(prefix), ratelimit.WithRedisTimeout(time.Second))

	for i, l := range []*ratelimit.Limiter{a, b, a} {
		if res := allow(t, l, "k"); !res.Allowed {
			t.Fatalf("request %d of the burst: %+v", i, res)
		}
	}
	res := allow(t, b, "k")
	if res.Allowed || res.RetryAfter <= 0 || res.RetryAfter > 100*time.Millisecond {
		t.Fatalf("after the burst: %+v, want limited for at most 100ms", res)
	}
	if ttl := rdb.PTTL(ctx, prefix+"api:k").Val(); ttl <= 0 || ttl > 400*time.Millisecond {
		t.Errorf("key TTL = %s, want up to the 300ms refill", ttl)
	}
	time.Sleep(res.RetryAfter + 20*time.Millisecond)
	if res := allow(t, a, "k"); !res.Allowed {
		t.Errorf("after RetryAfter: %+v, want allowed", res)
	}
}
