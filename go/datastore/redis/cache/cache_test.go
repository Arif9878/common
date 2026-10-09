package cache_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/datastore/redis/cache"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/testkit"
)

type product struct {
	ID    string `json:"id"`
	Price int    `json:"price"`
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func setup(t *testing.T) (*miniredis.Miniredis, *goredis.Client) {
	t.Helper()
	m := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: m.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return m, rdb
}

func TestHitMissAndDelete(t *testing.T) {
	m, rdb := setup(t)
	mp, metrics := testkit.NewMetrics(t)
	c := cache.New[product](rdb, "products", time.Minute, cache.WithLogger[product](quiet), cache.WithMeterProvider[product](mp))
	ctx := context.Background()
	loads := 0
	load := func(context.Context) (product, error) { loads++; return product{ID: "p1", Price: 100 * loads}, nil }

	for range 3 {
		p, err := c.Get(ctx, "p1", load)
		if err != nil || p.Price != 100 {
			t.Fatalf("Get = %+v, %v", p, err)
		}
	}
	if loads != 1 {
		t.Errorf("%d loads for 3 Gets", loads)
	}
	if !m.Exists("cache:products:p1") {
		t.Error("entry not stored under cache:products:p1")
	}
	if err := c.Delete(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Get(ctx, "p1", load); p.Price != 200 {
		t.Errorf("after Delete: %+v, want a reload", p)
	}
	hit := metrics.Sum("cache.requests", attribute.String("cache", "products"), attribute.String("outcome", "hit"))
	miss := metrics.Sum("cache.requests", attribute.String("cache", "products"), attribute.String("outcome", "miss"))
	if hit != 2 || miss != 2 {
		t.Errorf("hits %v, misses %v; want 2 and 2", hit, miss)
	}
}

func TestOneLoadForConcurrentMisses(t *testing.T) {
	_, rdb := setup(t)
	c := cache.New[product](rdb, "products", time.Minute, cache.WithLogger[product](quiet))
	var loads atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (product, error) {
		loads.Add(1)
		<-release
		return product{ID: "hot"}, nil
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := c.Get(context.Background(), "hot", load); err != nil || p.ID != "hot" {
				t.Errorf("Get = %+v, %v", p, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond) // let them all miss
	close(release)
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Errorf("%d loads for 100 concurrent misses, want 1", n)
	}
}

func TestTTLJitter(t *testing.T) {
	m, rdb := setup(t)
	c := cache.New[int](rdb, "n", 100*time.Second, cache.WithJitter[int](0.2), cache.WithLogger[int](quiet))
	seen := map[time.Duration]bool{}
	for i := range 50 {
		key := string(rune('a' + i))
		_ = c.Set(context.Background(), key, i)
		ttl := m.TTL("cache:n:" + key)
		if ttl < 80*time.Second || ttl > 120*time.Second {
			t.Fatalf("TTL %v outside 100s ± 20%%", ttl)
		}
		seen[ttl.Truncate(time.Second)] = true
	}
	if len(seen) < 5 {
		t.Errorf("only %d distinct TTLs: no jitter", len(seen))
	}
}

func TestMissingIsCached(t *testing.T) {
	m, rdb := setup(t)
	c := cache.New[product](rdb, "products", time.Minute, cache.WithMissingTTL[product](30*time.Second), cache.WithLogger[product](quiet))
	loads := 0
	load := func(context.Context) (product, error) {
		loads++
		return product{}, errors.NotFound.New("no such product")
	}
	for range 3 {
		if _, err := c.Get(context.Background(), "gone", load); errors.KindOf(err) != errors.NotFound {
			t.Fatalf("err = %v, want not_found", err)
		}
	}
	if loads != 1 {
		t.Errorf("%d loads of a missing key, want 1", loads)
	}
	if ttl := m.TTL("cache:products:gone"); ttl != 30*time.Second {
		t.Errorf("missing TTL = %v", ttl)
	}
	// Other errors are not cached.
	failing := cache.New[product](rdb, "other", time.Minute, cache.WithMissingTTL[product](time.Minute), cache.WithLogger[product](quiet))
	_, _ = failing.Get(context.Background(), "k", func(context.Context) (product, error) { return product{}, errors.Unavailable.New("db down") })
	if m.Exists("cache:other:k") {
		t.Error("an Unavailable error was cached")
	}
}

func TestRedisDownServesFromLoader(t *testing.T) {
	m, rdb := setup(t)
	mp, metrics := testkit.NewMetrics(t)
	c := cache.New[product](rdb, "products", time.Minute, cache.WithLogger[product](quiet), cache.WithMeterProvider[product](mp))
	m.Close()
	start := time.Now()
	p, err := c.Get(context.Background(), "p1", func(context.Context) (product, error) { return product{ID: "p1"}, nil })
	if err != nil || p.ID != "p1" {
		t.Fatalf("Get with Redis down = %+v, %v", p, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Get took %v with Redis down", d)
	}
	if n := metrics.Sum("cache.requests", attribute.String("outcome", "error")); n != 1 {
		t.Errorf("error outcomes = %v", n)
	}
}

func TestUndecodableEntryIsReloaded(t *testing.T) {
	m, rdb := setup(t)
	c := cache.New[product](rdb, "products", time.Minute, cache.WithLogger[product](quiet))
	_ = m.Set("cache:products:p1", "{not json")
	p, err := c.Get(context.Background(), "p1", func(context.Context) (product, error) { return product{ID: "fresh"}, nil })
	if err != nil || p.ID != "fresh" {
		t.Errorf("Get = %+v, %v", p, err)
	}
}

func TestCallerCancellationDoesNotFailOthers(t *testing.T) {
	_, rdb := setup(t)
	c := cache.New[product](rdb, "products", time.Minute, cache.WithLogger[product](quiet))
	release := make(chan struct{})
	load := func(ctx context.Context) (product, error) {
		select {
		case <-release:
			return product{ID: "ok"}, nil
		case <-ctx.Done():
			return product{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.Get(ctx, "k", load); first <- err }()
	time.Sleep(50 * time.Millisecond)
	second := make(chan error, 1)
	go func() { _, err := c.Get(context.Background(), "k", load); second <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Errorf("first caller: %v", err)
	}
	close(release)
	if err := <-second; err != nil {
		t.Errorf("second caller failed because the first cancelled: %v", err)
	}
}
