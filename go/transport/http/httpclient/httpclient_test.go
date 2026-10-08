package httpclient_test

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Arif9878/common/go/testkit"

	"go.opentelemetry.io/otel/propagation"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/resilience/circuitbreaker"
	"github.com/Arif9878/common/go/resilience/retry"
	"github.com/Arif9878/common/go/transport/http/httpclient"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fake is a scripted transport. Each call uses the next step; the last
// step repeats.
type fake struct {
	mu         sync.Mutex
	steps      []func(*http.Request) (*http.Response, error)
	requests   []*http.Request
	bodies     []string
	responses  []*trackedBody
	closedIdle bool
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func status(code int, body string, header ...string) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		for i := 0; i+1 < len(header); i += 2 {
			h.Set(header[i], header[i+1])
		}
		return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
}

func netErr(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "connection refused"}}
}

func (f *fake) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, body)
	step := f.steps[min(len(f.requests), len(f.steps))-1]
	resp, err := step(r)
	if resp != nil {
		tb := &trackedBody{Reader: resp.Body}
		resp.Body = tb
		f.responses = append(f.responses, tb)
	}
	return resp, err
}

func (f *fake) CloseIdleConnections() { f.closedIdle = true }

func (f *fake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newClient(f *fake, opts ...httpclient.Option) *http.Client {
	return httpclient.New(httpclient.Config{}, append([]httpclient.Option{
		httpclient.WithBaseTransport(f), httpclient.WithLogger(quiet),
	}, opts...)...)
}

func get(t *testing.T, c *http.Client, ctx context.Context, method string, body io.Reader, header ...string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://payments.internal/charges/c-1", body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return c.Do(req)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNoRetryByDefault(t *testing.T) {
	f := &fake{steps: []func(*http.Request) (*http.Response, error){status(503, "busy")}}
	resp, err := get(t, newClient(f), context.Background(), http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 503 || f.calls() != 1 {
		t.Fatalf("status %d, calls %d", resp.StatusCode, f.calls())
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fake{steps: []func(*http.Request) (*http.Response, error){
			status(503, "busy", "Retry-After", "2"),
			status(200, "ok"),
		}}
		c := newClient(f, httpclient.WithRetry(retry.WithoutJitter()))
		start := time.Now()
		resp, err := get(t, c, context.Background(), http.MethodGet, nil)
		if err != nil || resp.StatusCode != 200 || readBody(t, resp) != "ok" {
			t.Fatalf("resp %v, err %v", resp, err)
		}
		if f.calls() != 2 || time.Since(start) != 2*time.Second {
			t.Errorf("calls %d after %v, want 2 after the 2s Retry-After", f.calls(), time.Since(start))
		}
		if !f.responses[0].closed {
			t.Error("discarded 503 body not closed")
		}
	})
}

func TestExhaustedReturnsLastResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fake{steps: []func(*http.Request) (*http.Response, error){status(503, "still busy")}}
		c := newClient(f, httpclient.WithRetry(retry.WithMaxAttempts(3)))
		resp, err := get(t, c, context.Background(), http.MethodGet, nil)
		if err != nil {
			t.Fatalf("err = %v, want the last response", err)
		}
		if resp.StatusCode != 503 || readBody(t, resp) != "still busy" || f.calls() != 3 {
			t.Fatalf("status %d, calls %d", resp.StatusCode, f.calls())
		}
		if !f.responses[0].closed || !f.responses[1].closed {
			t.Error("earlier attempt bodies not closed")
		}
	})
}

func TestOnlySafeRequestsAreRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tests := []struct {
			name   string
			method string
			body   io.Reader
			header []string
			calls  int
		}{
			{"GET", http.MethodGet, nil, nil, 3},
			{"PUT with rewindable body", http.MethodPut, strings.NewReader(`{"a":1}`), nil, 3},
			{"POST", http.MethodPost, strings.NewReader(`{"a":1}`), nil, 1},
			{"POST with Idempotency-Key", http.MethodPost, strings.NewReader(`{"a":1}`), []string{"Idempotency-Key", "k1"}, 3},
			{"PUT with one-shot body", http.MethodPut, io.MultiReader(strings.NewReader("x")), nil, 1},
		}
		for _, tt := range tests {
			f := &fake{steps: []func(*http.Request) (*http.Response, error){status(503, "")}}
			c := newClient(f, httpclient.WithRetry(retry.WithMaxAttempts(3)))
			resp, err := get(t, c, context.Background(), tt.method, tt.body, tt.header...)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			_ = resp.Body.Close()
			if f.calls() != tt.calls {
				t.Errorf("%s: %d calls, want %d", tt.name, f.calls(), tt.calls)
			}
			if tt.body != nil && tt.calls > 1 {
				for i, b := range f.bodies {
					if b != f.bodies[0] || b == "" {
						t.Errorf("%s: attempt %d sent body %q", tt.name, i+1, b)
					}
				}
			}
		}
	})
}

func TestStatusesNotRetried(t *testing.T) {
	for _, code := range []int{400, 404, 409, 500} {
		f := &fake{steps: []func(*http.Request) (*http.Response, error){status(code, "")}}
		resp, err := get(t, newClient(f, httpclient.WithRetry()), context.Background(), http.MethodGet, nil)
		if err != nil || resp.StatusCode != code || f.calls() != 1 {
			t.Errorf("%d: err %v, calls %d", code, err, f.calls())
		}
		_ = resp.Body.Close()
	}
}

func TestNetworkErrorsRetriedAndClassified(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fake{steps: []func(*http.Request) (*http.Response, error){netErr}}
		_, err := get(t, newClient(f, httpclient.WithRetry(retry.WithMaxAttempts(2))), context.Background(), http.MethodGet, nil)
		if f.calls() != 2 || errors.KindOf(err) != errors.Unavailable {
			t.Fatalf("calls %d, err %v (kind %v)", f.calls(), err, errors.KindOf(err))
		}
		if _, ok := errors.AsType[*net.OpError](err); !ok {
			t.Errorf("original error lost: %v", err)
		}
	})
}

func TestCancelDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fake{steps: []func(*http.Request) (*http.Response, error){status(503, "", "Retry-After", "10")}}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(time.Second)
			cancel()
		}()
		resp, err := get(t, newClient(f, httpclient.WithRetry()), ctx, http.MethodGet, nil)
		if resp != nil || errors.KindOf(err) != errors.Canceled {
			t.Fatalf("resp %v, err %v", resp, err)
		}
		if !f.responses[0].closed {
			t.Error("503 body leaked")
		}
	})
}

func TestCircuitBreaker(t *testing.T) {
	cb := circuitbreaker.New("payments", circuitbreaker.WithConsecutiveFailures(2), circuitbreaker.WithLogger(quiet))
	f := &fake{steps: []func(*http.Request) (*http.Response, error){status(404, ""), status(404, ""), status(503, "")}}
	c := newClient(f, httpclient.WithCircuitBreaker(cb))

	for range 4 {
		resp, err := get(t, c, context.Background(), http.MethodGet, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	// 404, 404 (successes), 503, 503: two consecutive failures open it.
	if cb.State() != circuitbreaker.Open {
		t.Fatalf("state = %v", cb.State())
	}
	_, err := get(t, c, context.Background(), http.MethodGet, nil)
	if !errors.Is(err, circuitbreaker.ErrOpen) || errors.KindOf(err) != errors.Unavailable || f.calls() != 4 {
		t.Fatalf("err %v, calls %d", err, f.calls())
	}
}

func TestRequestIDPropagation(t *testing.T) {
	f := &fake{steps: []func(*http.Request) (*http.Response, error){status(200, "")}}
	c := newClient(f)
	ctx := requestid.NewContext(context.Background(), "req-9")

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://x/", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := f.requests[0].Header.Get("X-Request-ID"); got != "req-9" {
		t.Errorf("sent X-Request-ID %q", got)
	}
	if req.Header.Get("X-Request-ID") != "" {
		t.Error("caller's request was modified")
	}

	resp, _ = get(t, c, ctx, http.MethodGet, nil, "X-Request-ID", "explicit")
	_ = resp.Body.Close()
	if got := f.requests[1].Header.Get("X-Request-ID"); got != "explicit" {
		t.Errorf("explicit header overwritten: %q", got)
	}
}

func TestTracingAndMetricsPerAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mp, metrics := testkit.NewMetrics(t)
		tp, _ := testkit.NewTracer(t)
		f := &fake{steps: []func(*http.Request) (*http.Response, error){status(502, ""), status(200, "")}}
		c := newClient(f, httpclient.WithRetry(),
			httpclient.WithTracerProvider(tp),
			httpclient.WithPropagators(propagation.TraceContext{}),
			httpclient.WithMeterProvider(mp))

		ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
		resp, err := get(t, c, ctx, http.MethodGet, nil)
		span.End()
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()

		for i, r := range f.requests {
			tp := r.Header.Get("traceparent")
			if !strings.Contains(tp, span.SpanContext().TraceID().String()) {
				t.Errorf("attempt %d traceparent %q", i+1, tp)
			}
		}
		if f.requests[0].Header.Get("traceparent") == f.requests[1].Header.Get("traceparent") {
			t.Error("attempts share a span; want one span per attempt")
		}

		if attempts := metrics.HistogramCount("http.client.request.duration"); attempts != 2 {
			t.Errorf("recorded %d attempts, want 2", attempts)
		}
	})
}

func TestCloseIdleConnections(t *testing.T) {
	f := &fake{}
	newClient(f).CloseIdleConnections()
	if !f.closedIdle {
		t.Error("CloseIdleConnections not forwarded to the base transport")
	}
}

func TestDefaults(t *testing.T) {
	tr := httpclient.NewTransport(httpclient.Config{})
	if tr.MaxIdleConnsPerHost != 20 || tr.MaxIdleConns != 100 || tr.TLSHandshakeTimeout != 5*time.Second ||
		tr.IdleConnTimeout != 90*time.Second || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("transport = %+v", tr)
	}
	if c := httpclient.New(httpclient.Config{}); c.Timeout != 30*time.Second {
		t.Errorf("client timeout = %v", c.Timeout)
	}
}
