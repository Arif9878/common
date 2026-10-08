package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/requestid"
)

func newLogger(t *testing.T, cfg logging.Config, opts ...logging.Option) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := logging.New(cfg, append([]logging.Option{logging.WithWriter(&buf)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return logger, &buf
}

func decode(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func one(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	recs := decode(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(recs), recs)
	}
	return recs[0]
}

func spanContext(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc), sc
}

func TestNewConfig(t *testing.T) {
	t.Run("invalid level", func(t *testing.T) {
		if _, err := logging.New(logging.Config{Level: "loud"}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("invalid format", func(t *testing.T) {
		if _, err := logging.New(logging.Config{Format: "xml"}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("zero value is json at info", func(t *testing.T) {
		logger, buf := newLogger(t, logging.Config{})
		logger.Debug("hidden")
		logger.Info("shown")
		rec := one(t, buf)
		if rec["msg"] != "shown" || rec["level"] != "INFO" {
			t.Fatalf("record = %v", rec)
		}
		if _, ok := rec[logging.KeyService]; ok {
			t.Fatal("empty service should be omitted")
		}
	})
	t.Run("text format", func(t *testing.T) {
		logger, buf := newLogger(t, logging.Config{Format: "TEXT"})
		logger.Info("hello", "password", "hunter2")
		out := buf.String()
		if !strings.Contains(out, "msg=hello") || strings.Contains(out, "hunter2") {
			t.Fatalf("output = %q", out)
		}
	})
	t.Run("static fields", func(t *testing.T) {
		logger, buf := newLogger(t, logging.Config{Service: "orders", Environment: "prod", Version: "1.2.3"})
		logger.WithGroup("g").Info("x", "a", 1)
		rec := one(t, buf)
		if rec["service"] != "orders" || rec["environment"] != "prod" || rec["version"] != "1.2.3" {
			t.Fatalf("record = %v", rec)
		}
	})
	t.Run("level var", func(t *testing.T) {
		var lv slog.LevelVar
		logger, buf := newLogger(t, logging.Config{Level: "warn"}, logging.WithLevelVar(&lv))
		logger.Info("hidden")
		lv.Set(slog.LevelDebug)
		logger.Debug("shown")
		if rec := one(t, buf); rec["msg"] != "shown" {
			t.Fatalf("record = %v", rec)
		}
	})
}

func TestContextFields(t *testing.T) {
	logger, buf := newLogger(t, logging.Config{})
	ctx, sc := spanContext(t)
	ctx = requestid.NewContext(ctx, "req-1")
	ctx = logging.ContextWithAttrs(ctx, slog.String(logging.KeyTopic, "orders"))
	ctx = logging.ContextWithAttrs(ctx, slog.Int(logging.KeyPartition, 3))

	logger.InfoContext(ctx, "handled")
	rec := one(t, buf)

	want := map[string]any{
		logging.KeyTraceID:   sc.TraceID().String(),
		logging.KeySpanID:    sc.SpanID().String(),
		logging.KeyRequestID: "req-1",
		logging.KeyTopic:     "orders",
		logging.KeyPartition: float64(3),
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %v", k, rec[k], v)
		}
	}
}

func TestContextFieldsStayTopLevelInGroups(t *testing.T) {
	logger, buf := newLogger(t, logging.Config{})
	ctx, _ := spanContext(t)

	logger.With("a", 1).WithGroup("req").With("b", 2).WithGroup("inner").
		InfoContext(ctx, "m", "c", 3, "password", "hunter2")
	rec := one(t, buf)

	if _, ok := rec[logging.KeyTraceID]; !ok {
		t.Fatalf("trace_id not top-level: %v", rec)
	}
	if rec["a"] != float64(1) {
		t.Fatalf("a = %v", rec["a"])
	}
	req, _ := rec["req"].(map[string]any)
	inner, _ := req["inner"].(map[string]any)
	if req["b"] != float64(2) || inner["c"] != float64(3) || inner["password"] != logging.RedactedValue {
		t.Fatalf("record = %v", rec)
	}
}

func TestContextAttrsDoNotAlias(t *testing.T) {
	logger, buf := newLogger(t, logging.Config{})
	parent := logging.ContextWithAttrs(context.Background(), slog.String("p", "1"))
	a := logging.ContextWithAttrs(parent, slog.String("child", "a"))
	b := logging.ContextWithAttrs(parent, slog.String("child", "b"))

	logger.InfoContext(a, "a")
	logger.InfoContext(b, "b")
	logger.InfoContext(parent, "parent")
	recs := decode(t, buf)
	if recs[0]["child"] != "a" || recs[1]["child"] != "b" {
		t.Fatalf("records = %v", recs)
	}
	if _, ok := recs[2]["child"]; ok {
		t.Fatalf("parent context gained child attr: %v", recs[2])
	}
}

type creds struct{ user, pass string }

func (c creds) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user", c.user), slog.String("password", c.pass))
}

func TestRedaction(t *testing.T) {
	logger, buf := newLogger(t, logging.Config{}, logging.WithRedactKeys("SSN"))
	ctx := logging.ContextWithAttrs(context.Background(), slog.String("session_token", "ctx-secret"))

	logger.With("db_password", "with-secret").InfoContext(ctx, "m",
		"Authorization", "Bearer abc",
		"X-Api-Key", "k",
		"api.key", "k",
		"refreshToken", "t",
		"client_secret", "s",
		"Set-Cookie", "c",
		"private_key", "pk",
		"aws_credentials", "cr",
		"customer_ssn", "123",
		slog.Group("http", slog.String("method", "GET"), slog.Group("headers", slog.String("authorization", "x"))),
		"creds", creds{user: "svc", pass: "p"},
		"user_id", "u1",
		"status", 200,
	)
	out := buf.String()
	for _, leaked := range []string{"with-secret", "Bearer", "ctx-secret", `"k"`, `"t"`, `"s"`, `"c"`, `"pk"`, `"cr"`, `"123"`, `"x"`, `"p"`} {
		if strings.Contains(out, leaked) {
			t.Errorf("leaked %s in %s", leaked, out)
		}
	}

	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["user_id"] != "u1" || rec["status"] != float64(200) {
		t.Errorf("non-sensitive fields changed: %v", rec)
	}
	httpGroup, _ := rec["http"].(map[string]any)
	if httpGroup["method"] != "GET" {
		t.Errorf("http.method = %v", httpGroup["method"])
	}
	c, _ := rec["creds"].(map[string]any)
	if c["user"] != "svc" || c["password"] != logging.RedactedValue {
		t.Errorf("creds = %v", c)
	}
}

func TestErrAndDuration(t *testing.T) {
	logger, buf := newLogger(t, logging.Config{})
	logger.Info("m",
		logging.Err(errors.Unavailable.Wrap(errors.New("refused"), "query")),
		logging.Duration(1500*time.Microsecond),
	)
	logger.Info("nil error", logging.Err(nil))

	recs := decode(t, buf)
	if recs[0][logging.KeyError] != "query: refused" || recs[0][logging.KeyErrorType] != "unavailable" {
		t.Errorf("record = %v", recs[0])
	}
	if recs[0][logging.KeyDurationMS] != 1.5 {
		t.Errorf("duration_ms = %v", recs[0][logging.KeyDurationMS])
	}
	if _, ok := recs[1][logging.KeyError]; ok {
		t.Errorf("nil error logged: %v", recs[1])
	}
}

func TestConcurrentUse(t *testing.T) {
	logger, buf := newLogger(t, logging.Config{})
	logger = logger.With("shared", 1)
	ctx, _ := spanContext(t)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			l := logger.WithGroup(fmt.Sprintf("g%d", i%4)).With("token", "x")
			for j := range 50 {
				l.InfoContext(ctx, "m", "j", j)
			}
		})
	}
	wg.Wait()

	recs := decode(t, buf)
	if len(recs) != 32*50 {
		t.Fatalf("got %d records", len(recs))
	}
	if strings.Contains(fmt.Sprint(recs), `token:x`) {
		t.Fatal("token leaked under concurrency")
	}
}

func TestSlogConformance(t *testing.T) {
	var buf bytes.Buffer
	slogtest.Run(t,
		func(*testing.T) slog.Handler {
			buf.Reset()
			return logging.NewHandler(slog.NewJSONHandler(&buf, nil))
		},
		func(t *testing.T) map[string]any {
			var m map[string]any
			if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
				t.Fatal(err)
			}
			return m
		},
	)
}

func BenchmarkHandler(b *testing.B) {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))
	ctx = requestid.NewContext(ctx, "req-1")

	base := slog.New(slog.NewJSONHandler(io.Discard, nil)).With("service", "orders")
	wrapped, _ := logging.New(logging.Config{Service: "orders"}, logging.WithWriter(io.Discard))

	log := func(l *slog.Logger) {
		l.InfoContext(ctx, "order created", "order_id", "o-1", "items", 3, "status", "ok", "amount", 12.5)
	}
	b.Run("slog_json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			log(base)
		}
	})
	b.Run("slog_json_same_fields", func(b *testing.B) {
		b.ReportAllocs()
		sc := trace.SpanContextFromContext(ctx)
		for b.Loop() {
			base.InfoContext(ctx, "order created", "order_id", "o-1", "items", 3, "status", "ok", "amount", 12.5,
				"trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String(), "request_id", "req-1")
		}
	})
	b.Run("logging", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			log(wrapped)
		}
	})
	b.Run("logging_redact", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			wrapped.InfoContext(ctx, "login", "user_id", "u-1", "password", "x")
		}
	})
}
