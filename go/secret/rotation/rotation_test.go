package rotation_test

import (
	"context"
	stderrors "errors"
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Arif9878/common/go/testkit"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/secret"
	"github.com/Arif9878/common/go/secret/rotation"
)

var quiet = rotation.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

// resource is a fake credential-backed client.
type resource struct {
	version string
	closed  atomic.Bool
}

// env is a fake secret provider and resource factory with counters.
type env struct {
	mu        sync.Mutex
	ttl       time.Duration
	version   int
	static    bool // keep the same version on every fetch
	fetchErr  error
	failValid int // fail this many validations
	fetchGate chan struct{}

	fetches, builds, closes atomic.Int32
	built                   []*resource
	closedAt                map[string]time.Time
}

func (e *env) closedTime(version string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closedAt[version]
}

func newEnv(ttl time.Duration) *env { return &env{ttl: ttl, closedAt: map[string]time.Time{}} }

func (e *env) spec() rotation.Spec[*resource] {
	return rotation.Spec[*resource]{
		Fetch: func(ctx context.Context) (secret.Secret, error) {
			if e.fetchGate != nil {
				<-e.fetchGate
			}
			e.fetches.Add(1)
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.fetchErr != nil {
				return secret.Secret{}, e.fetchErr
			}
			if !e.static || e.version == 0 {
				e.version++
			}
			s := secret.New(map[string]string{"password": "pw-" + strconv.Itoa(e.version)})
			s.Version = strconv.Itoa(e.version)
			if e.ttl > 0 {
				s.ExpiresAt = time.Now().Add(e.ttl)
			}
			return s, nil
		},
		Build: func(_ context.Context, s secret.Secret) (*resource, error) {
			e.builds.Add(1)
			r := &resource{version: s.Version}
			e.mu.Lock()
			e.built = append(e.built, r)
			e.mu.Unlock()
			return r, nil
		},
		Validate: func(context.Context, *resource) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.failValid > 0 {
				e.failValid--
				return errors.Unavailable.New("ping failed")
			}
			return nil
		},
		Close: func(_ context.Context, r *resource, _ secret.Secret) error {
			e.closes.Add(1)
			r.closed.Store(true)
			e.mu.Lock()
			e.closedAt[r.version] = time.Now()
			e.mu.Unlock()
			return nil
		},
	}
}

func mustNew(t *testing.T, e *env, opts ...rotation.Option) *rotation.Rotator[*resource] {
	t.Helper()
	r, err := rotation.New(context.Background(), "db", e.spec(), append([]rotation.Option{quiet}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func closeR(t *testing.T, r *rotation.Rotator[*resource]) {
	t.Helper()
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewFailsFast(t *testing.T) {
	e := newEnv(0)
	e.fetchErr = errors.Unauthorized.New("vault: permission denied")
	if _, err := rotation.New(context.Background(), "db", e.spec(), quiet); errors.KindOf(err) != errors.Unauthorized {
		t.Errorf("fetch failure: %v", err)
	}

	e = newEnv(0)
	e.failValid = 1
	if _, err := rotation.New(context.Background(), "db", e.spec(), quiet); errors.KindOf(err) != errors.Unavailable {
		t.Errorf("validation failure: %v", err)
	}
	if e.closes.Load() != 1 {
		t.Error("resource that failed validation was not closed")
	}

	if _, err := rotation.New(context.Background(), "db", rotation.Spec[*resource]{}); errors.KindOf(err) != errors.InvalidArgument {
		t.Errorf("empty spec: %v", err)
	}
}

func TestScheduledRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(100 * time.Second)
		r := mustNew(t, e, rotation.WithRenewAt(0.7, 0))
		start := time.Now()

		time.Sleep(69 * time.Second)
		synctest.Wait()
		if r.Current().version != "1" {
			t.Fatal("rotated early")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if r.Current().version != "2" {
			t.Fatalf("not rotated at 70%% of TTL; version %s", r.Current().version)
		}
		if at := e.closedTime("1"); at.Sub(start) != 70*time.Second {
			t.Errorf("old resource closed at %v", at.Sub(start))
		}
		time.Sleep(70 * time.Second)
		synctest.Wait()
		if r.Current().version != "3" {
			t.Fatalf("second rotation missing; version %s", r.Current().version)
		}
		closeR(t, r)
	})
}

func TestFailedRotationKeepsCurrentAndRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(100 * time.Second)
		r := mustNew(t, e, rotation.WithRenewAt(0.7, 0), rotation.WithBackoff(time.Second, time.Second))
		e.mu.Lock()
		e.failValid = 3
		e.mu.Unlock()

		time.Sleep(70 * time.Second)
		synctest.Wait()
		if r.Current().version != "1" {
			t.Fatal("failed rotation replaced the current resource")
		}
		time.Sleep(5 * time.Second) // three retries at 0.5–1s each
		synctest.Wait()
		if r.Current().version != "5" || r.Current().closed.Load() {
			t.Fatalf("current = %+v after retries", r.Current())
		}
		// Versions 2–4 failed validation and were closed; 1 was replaced.
		if e.closes.Load() != 4 {
			t.Errorf("closes = %d, want 4", e.closes.Load())
		}
		closeR(t, r)
	})
}

func TestDrainWaitsForAcquiredUses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(0)
		r := mustNew(t, e)
		old, release, err := r.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Rotate(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if r.Current() == old || old.closed.Load() {
			t.Fatal("old resource closed while acquired")
		}
		time.Sleep(10 * time.Second)
		release()
		release() // idempotent
		synctest.Wait()
		if !old.closed.Load() {
			t.Fatal("old resource not closed after release")
		}
		closeR(t, r)
	})
}

func TestDrainTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(0)
		r := mustNew(t, e, rotation.WithDrainTimeout(5*time.Second))
		old, release, _ := r.Acquire()
		defer release()
		start := time.Now()
		_ = r.Rotate(context.Background())
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if !old.closed.Load() || e.closedTime("1").Sub(start) != 5*time.Second {
			t.Fatalf("leaked use did not hit the drain timeout")
		}
		closeR(t, r)
	})
}

func TestNoUseAfterCloseUnderLoad(t *testing.T) {
	e := newEnv(0)
	r := mustNew(t, e)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var uses atomic.Int64
	for range 16 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, release, err := r.Acquire()
				if err != nil {
					t.Error(err)
					return
				}
				if res.closed.Load() {
					t.Error("acquired a closed resource")
				}
				uses.Add(1)
				runtime.Gosched() // let rotations happen mid-use
				if res.closed.Load() {
					t.Error("resource closed while in use")
				}
				release()
			}
		})
	}
	for range 1000 {
		if err := r.Rotate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	closeR(t, r)
	if e.builds.Load() != e.closes.Load() {
		t.Errorf("built %d resources, closed %d", e.builds.Load(), e.closes.Load())
	}
	t.Logf("%d uses across %d rotations", uses.Load(), e.builds.Load()-1)
}

func TestConcurrentRotateRunsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(0)
		r := mustNew(t, e)
		e.fetchGate = make(chan struct{})
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				if err := r.Rotate(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}
		synctest.Wait()
		close(e.fetchGate)
		wg.Wait()
		if e.fetches.Load() != 2 { // initial + one shared rotation
			t.Errorf("fetches = %d, want 2", e.fetches.Load())
		}
		e.fetchGate = nil
		closeR(t, r)
	})
}

func TestRefreshIntervalSkipsUnchangedVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(0)
		e.static = true
		r := mustNew(t, e, rotation.WithRefreshInterval(10*time.Second))
		time.Sleep(35 * time.Second)
		synctest.Wait()
		if e.fetches.Load() != 4 || e.builds.Load() != 1 {
			t.Fatalf("fetches %d builds %d; want refetch every 10s without rebuilding", e.fetches.Load(), e.builds.Load())
		}
		e.mu.Lock()
		e.static = false // the secret changed upstream
		e.mu.Unlock()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if r.Current().version != "2" {
			t.Fatalf("changed version not picked up: %s", r.Current().version)
		}
		closeR(t, r)
	})
}

func TestNoExpiryNoInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(0)
		r := mustNew(t, e)
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		if e.fetches.Load() != 1 {
			t.Fatalf("fetches = %d without expiry or interval", e.fetches.Load())
		}
		closeR(t, r)
	})
}

func TestClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(time.Hour)
		r := mustNew(t, e)
		res, release, _ := r.Acquire()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		err := r.Close(ctx)
		if errors.KindOf(err) != errors.Timeout || time.Since(start) != 3*time.Second || !res.closed.Load() {
			t.Fatalf("Close = %v after %v, closed %v", err, time.Since(start), res.closed.Load())
		}
		release()

		if _, _, err := r.Acquire(); !errors.Is(err, rotation.ErrClosed) {
			t.Errorf("Acquire after Close = %v", err)
		}
		if err := r.Rotate(context.Background()); !errors.Is(err, rotation.ErrClosed) {
			t.Errorf("Rotate after Close = %v", err)
		}
		if err2 := r.Close(context.Background()); err2 != err { //nolint:errorlint // same result
			t.Errorf("second Close = %v", err2)
		}
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if e.fetches.Load() != 1 {
			t.Error("rotation loop still running after Close")
		}
	})
}

func TestRotateRacingCloseLeaksNothing(t *testing.T) {
	for range 50 {
		e := newEnv(0)
		r := mustNew(t, e)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 20 {
					if err := r.Rotate(context.Background()); errors.Is(err, rotation.ErrClosed) {
						return
					}
				}
			})
		}
		closeR(t, r)
		wg.Wait()
		if e.builds.Load() != e.closes.Load() {
			t.Fatalf("built %d, closed %d: a resource leaked", e.builds.Load(), e.closes.Load())
		}
	}
}

func TestObservability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger, logs := testkit.NewLogger(t)
		mp, m := testkit.NewMetrics(t)
		e := newEnv(10 * time.Second)
		r := mustNew(t, e,
			rotation.WithLogger(logger),
			rotation.WithMeterProvider(mp),
			rotation.WithRenewAt(0.7, 0), rotation.WithBackoff(time.Minute, time.Minute))

		e.mu.Lock()
		e.fetchErr = stderrors.New("vault sealed")
		e.mu.Unlock()
		time.Sleep(20 * time.Second) // rotation fails at 7s; credential expires at 10s
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()

		out := logs.String()
		if logs.Contains("pw-") {
			t.Fatalf("secret value logged:\n%s", out)
		}
		if !strings.Contains(out, `"level":"WARN","msg":"credential rotation failed"`) ||
			!strings.Contains(out, `"level":"ERROR","msg":"credential rotation failed"`) {
			t.Errorf("want WARN before expiry and ERROR after:\n%s", out)
		}

		res := attribute.String("resource", "db")
		success := m.Sum("rotation.attempts", res, attribute.String("outcome", "success"))
		fetchErrors := m.Sum("rotation.attempts", res, attribute.String("outcome", "fetch_error"))
		ttl := m.Gauge("rotation.credential.ttl", res)
		if success != 1 || fetchErrors < 2 || ttl >= 0 {
			t.Errorf("success %v, fetch_error %v, ttl %v; want 1, >=2, <0", success, fetchErrors, ttl)
		}
		closeR(t, r)
	})
}
