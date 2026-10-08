package httpserver_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Arif9878/common/go/testkit"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/lifecycle/graceful"
	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/resilience/ratelimit"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func newRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(t.Context(), method, target, body)
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{nil, 200},
		{errors.InvalidArgument.New("x"), 400},
		{errors.Unauthorized.New("x"), 401},
		{errors.Forbidden.New("x"), 403},
		{errors.NotFound.New("x"), 404},
		{errors.Conflict.New("x"), 409},
		{&http.MaxBytesError{Limit: 1}, 413},
		{errors.RateLimited.New("x"), 429},
		{context.Canceled, 499},
		{errors.Internal.New("x"), 500},
		{stderrors.New("unclassified"), 500},
		{errors.Unavailable.New("x"), 503},
		{context.DeadlineExceeded, 504},
	}
	for _, tt := range tests {
		if got := httpserver.StatusFor(tt.err); got != tt.want {
			t.Errorf("StatusFor(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) httpserver.Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p httpserver.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	return p
}

func TestWriteError(t *testing.T) {
	r := newRequest(t, http.MethodGet, "/", nil)
	r = r.WithContext(requestid.NewContext(r.Context(), "req-1"))

	rec := httptest.NewRecorder()
	internal := errors.NotFound.Wrap(stderrors.New(`sql: no rows for user 42`), "load order")
	httpserver.WriteError(rec, r, errors.WithPublicMessage(internal, "order not found"))
	p := decodeProblem(t, rec)
	if rec.Code != 404 || p.Status != 404 || p.Code != "not_found" || p.Detail != "order not found" || p.RequestID != "req-1" {
		t.Errorf("problem = %+v", p)
	}
	if strings.Contains(rec.Body.String(), "sql") {
		t.Errorf("internal error leaked: %s", rec.Body)
	}

	rec = httptest.NewRecorder()
	httpserver.WriteError(rec, r, &ratelimit.LimitedError{Limiter: "api", After: 1500 * time.Millisecond})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("rate limited: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}

	rec = httptest.NewRecorder()
	httpserver.WriteError(rec, r, nil)
	if rec.Body.Len() != 0 {
		t.Error("nil error wrote a response")
	}
}

func TestRequestID(t *testing.T) {
	var seen string
	h := httpserver.RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = requestid.FromContext(r.Context())
	}))

	for name, tc := range map[string]struct {
		header   string
		keepsOwn bool
	}{
		"valid":     {"abc-123", true},
		"missing":   {"", false},
		"injection": {"abc\nlevel=ERROR", false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRequest(t, http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("X-Request-ID", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if (seen == tc.header) != tc.keepsOwn || !requestid.Valid(seen) {
				t.Errorf("context ID = %q", seen)
			}
			if rec.Header().Get("X-Request-ID") != seen {
				t.Errorf("response header %q, context %q", rec.Header().Get("X-Request-ID"), seen)
			}
		})
	}
}

func TestRecover(t *testing.T) {
	logger, logs := testkit.NewLogger(t)
	h := httpserver.Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("nil map")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, http.MethodGet, "/", nil))
	if p := decodeProblem(t, rec); rec.Code != 500 || p.Code != "internal" || strings.Contains(rec.Body.String(), "nil map") {
		t.Errorf("response %d %s", rec.Code, rec.Body)
	}
	if !logs.Contains("nil map") || !logs.Contains(`"stack"`) {
		t.Errorf("panic not logged with stack: %s", logs)
	}

	t.Run("ErrAbortHandler is re-raised", func(t *testing.T) {
		defer func() {
			if r := recover(); r != http.ErrAbortHandler { //nolint:errorlint // sentinel
				t.Fatalf("recovered %v", r)
			}
		}()
		httpserver.Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		})).ServeHTTP(httptest.NewRecorder(), newRequest(t, http.MethodGet, "/", nil))
	})

	t.Run("after response started, aborts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		defer func() {
			if r := recover(); r != http.ErrAbortHandler { //nolint:errorlint // sentinel
				t.Fatalf("recovered %v", r)
			}
			if rec.Code != 200 || strings.Contains(rec.Body.String(), "problem") {
				t.Errorf("second response written: %d %s", rec.Code, rec.Body)
			}
		}()
		httpserver.Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
			panic("late")
		})).ServeHTTP(rec, newRequest(t, http.MethodGet, "/", nil))
	})
}

func TestMaxBytes(t *testing.T) {
	h := httpserver.MaxBytes(10)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			httpserver.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, http.MethodPost, "/", strings.NewReader("12345678901")))
	if rec.Code != 413 {
		t.Errorf("declared length over limit: %d", rec.Code)
	}

	r := newRequest(t, http.MethodPost, "/", io.MultiReader(strings.NewReader("123456"), strings.NewReader("789012")))
	r.ContentLength = -1 // chunked: only detected while reading
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 413 {
		t.Errorf("streamed body over limit: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, http.MethodPost, "/", strings.NewReader("small")))
	if rec.Code != 204 {
		t.Errorf("small body: %d", rec.Code)
	}
}

func TestTimeout(t *testing.T) {
	h := httpserver.Timeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		httpserver.WriteError(w, r, r.Context().Err())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, http.MethodGet, "/", nil))
	if rec.Code != 504 {
		t.Errorf("status = %d", rec.Code)
	}
}

type userKey struct{}

func TestAuth(t *testing.T) {
	auth := httpserver.Auth(func(r *http.Request) (context.Context, error) {
		switch r.Header.Get("Authorization") {
		case "Bearer good":
			return context.WithValue(r.Context(), userKey{}, "u1"), nil
		case "Bearer readonly":
			return nil, errors.Forbidden.New("read-only token")
		case "":
			return nil, errors.Unauthorized.New("missing token")
		default:
			return nil, stderrors.New("jwt: signature invalid; key id k-7")
		}
	})
	h := auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(userKey{}) != "u1" {
			t.Error("authenticator context not passed on")
		}
		w.WriteHeader(http.StatusOK)
	}))

	for header, want := range map[string]int{"Bearer good": 200, "Bearer readonly": 403, "": 401, "Bearer forged": 401} {
		r := newRequest(t, http.MethodGet, "/", nil)
		r.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%q: status %d, want %d", header, rec.Code, want)
		}
		if strings.Contains(rec.Body.String(), "k-7") {
			t.Errorf("authenticator error leaked: %s", rec.Body)
		}
	}
}

// stack is a full Handler with in-memory logs, metrics and spans.
type stack struct {
	handler http.Handler
	logs    *testkit.Logs
	metrics *testkit.Metrics
	spans   *testkit.Spans
}

func newStack(t *testing.T, mux http.Handler, opts ...httpserver.Option) *stack {
	t.Helper()
	logger, logs := testkit.NewLogger(t)
	mp, metrics := testkit.NewMetrics(t)
	tp, spans := testkit.NewTracer(t)
	s := &stack{logs: logs, metrics: metrics, spans: spans}
	s.handler = httpserver.Handler(mux, append([]httpserver.Option{
		httpserver.WithLogger(logger),
		httpserver.WithMeterProvider(mp),
		httpserver.WithTracerProvider(tp),
		httpserver.WithPropagators(propagation.TraceContext{}),
	}, opts...)...)
	return s
}

func (s *stack) do(t *testing.T, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

func (s *stack) logLines(*testing.T) []testkit.Record { return s.logs.Records() }

func ordersMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			httpserver.WriteError(w, r, errors.NotFound.Wrap(stderrors.New("no rows"), "load order"))
			return
		}
		_, _ = io.WriteString(w, `{"found":true}`)
	})
	mux.HandleFunc("POST /orders", func(http.ResponseWriter, *http.Request) { panic("boom") })
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return mux
}

func TestHandlerEndToEnd(t *testing.T) {
	s := newStack(t, ordersMux(), httpserver.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/live" }))

	r := newRequest(t, http.MethodGet, "/orders/o-123?token=secret", nil)
	r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	r.Header.Set("Authorization", "Bearer secret")
	if rec := s.do(t, r); rec.Code != 200 || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("GET: %d, headers %v", rec.Code, rec.Header())
	}
	if rec := s.do(t, newRequest(t, http.MethodGet, "/orders/missing", nil)); rec.Code != 404 {
		t.Fatalf("missing: %d", rec.Code)
	}
	if rec := s.do(t, newRequest(t, http.MethodPost, "/orders", nil)); rec.Code != 500 {
		t.Fatalf("panic: %d", rec.Code)
	}
	s.do(t, newRequest(t, http.MethodGet, "/nope", nil))
	s.do(t, newRequest(t, "PURGE", "/orders/o-1", nil))
	s.do(t, newRequest(t, http.MethodGet, "/live", nil))

	// Access logs: route template, no secrets, error details on failures.
	if strings.Contains(s.logs.String(), "secret") || strings.Contains(s.logs.String(), "o-123") {
		t.Errorf("raw path, query or headers logged:\n%s", s.logs)
	}
	var access []map[string]any
	for _, l := range s.logLines(t) {
		if l["msg"] == "http request" {
			access = append(access, l)
		}
	}
	if len(access) != 5 {
		t.Fatalf("got %d access logs (live must be filtered):\n%s", len(access), s.logs)
	}
	first := access[0]
	if first["route"] != "/orders/{id}" || first["status"] != float64(200) || first["level"] != "INFO" ||
		first["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || first["request_id"] == nil {
		t.Errorf("access log = %v", first)
	}
	if access[1]["error_type"] != "not_found" || access[1]["error"] != "load order: no rows" {
		t.Errorf("404 log = %v", access[1])
	}
	if access[2]["status"] != float64(500) || access[2]["level"] != "ERROR" || access[2]["error_type"] != "internal" {
		t.Errorf("panic log = %v", access[2])
	}
	if access[3]["route"] != "unmatched" || access[4]["method"] != "_OTHER" {
		t.Errorf("unmatched/method logs = %v / %v", access[3], access[4])
	}

	// Spans: named by route, continuing the incoming trace.
	spans := s.spans.Ended()
	if len(spans) != 5 {
		t.Fatalf("got %d spans (live must be filtered)", len(spans))
	}
	if spans[0].Name() != "GET /orders/{id}" || spans[0].SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("span %q in trace %s", spans[0].Name(), spans[0].SpanContext().TraceID())
	}

	// Metrics: bounded attributes, including the filtered request.
	for _, want := range []struct {
		route  string
		status int
	}{{"/orders/{id}", 200}, {"/orders/{id}", 404}, {"/orders", 500}, {"unmatched", 404}, {"/live", 200}} {
		n := s.metrics.HistogramCount("http.server.request.duration",
			attribute.String("http.route", want.route), attribute.Int("http.response.status_code", want.status))
		if n == 0 {
			t.Errorf("no http.server.request.duration for %s %d", want.route, want.status)
		}
	}
	if n := s.metrics.HistogramCount("http.server.request.duration",
		attribute.String("http.route", "/orders"), attribute.String("error.type", "500")); n != 1 {
		t.Errorf("5xx request without error.type=500 (count %d)", n)
	}
}

func TestServeAndGracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httpserver.NewServer(httpserver.Config{Addr: "127.0.0.1:0"}, httpserver.Handler(mux, httpserver.WithLogger(quiet)), quiet)

	// Bind errors are reported synchronously.
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	g := graceful.New(graceful.WithSignals(), graceful.WithLogger(quiet))
	if err := httpserver.Serve(g, &http.Server{Addr: ln.Addr().String(), ReadHeaderTimeout: time.Second}); err == nil {
		t.Fatal("Serve on a used address succeeded")
	}

	// Pick a free port, then serve on it.
	free, _ := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	srv.Addr = free.Addr().String()
	_ = free.Close()
	if err := httpserver.Serve(g, srv); err != nil {
		t.Fatal(err)
	}

	result := make(chan string, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+srv.Addr+"/slow", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			result <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		result <- string(body)
	}()
	<-started
	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := <-result; got != "done" {
		t.Fatalf("in-flight request got %q, want it drained", got)
	}
}

func TestNewServerDefaults(t *testing.T) {
	srv := httpserver.NewServer(httpserver.Config{}, http.NotFoundHandler(), nil)
	if srv.Addr != ":8080" || srv.ReadHeaderTimeout != 5*time.Second || srv.WriteTimeout != 35*time.Second ||
		srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 1<<20 || srv.ErrorLog == nil {
		t.Errorf("server = %+v", srv)
	}
	if s := httpserver.NewServer(httpserver.Config{WriteTimeout: -1}, http.NotFoundHandler(), nil); s.WriteTimeout != 0 {
		t.Errorf("negative WriteTimeout not disabled: %v", s.WriteTimeout)
	}
}

func TestFlushPassesThrough(t *testing.T) {
	s := newStack(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "event: 1\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through middleware: %v", err)
		}
	}), httpserver.WithTimeout(0))
	rec := s.do(t, newRequest(t, http.MethodGet, "/events", nil))
	if !rec.Flushed {
		t.Error("response was not flushed")
	}
}
