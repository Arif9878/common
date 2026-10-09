package logs_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/Arif9878/common/go/observability/internal/otlptest"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/observability/logs"
	"github.com/Arif9878/common/go/observability/otlp"
)

func records(rc *otlptest.Receiver) []*logspb.LogRecord {
	var out []*logspb.LogRecord
	for _, r := range rc.Requests("/v1/logs") {
		for _, rl := range r.Logs.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				out = append(out, sl.GetLogRecords()...)
			}
		}
	}
	return out
}

func attr(r *logspb.LogRecord, key string) (string, bool) {
	for _, a := range r.GetAttributes() {
		if a.GetKey() == key {
			return a.GetValue().GetStringValue(), true
		}
	}
	return "", false
}

func TestLoggerWritesStdoutAndOTLP(t *testing.T) {
	rc := otlptest.New(t)
	ctx := context.Background()
	lp, err := logs.Init(ctx, logs.Config{Exporter: logs.ExporterOTLP, Service: "orders"},
		logs.WithOTLP(otlp.Config{Protocol: "http", Endpoint: rc.URL, Headers: "api-key=k"}))
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	logger, err := logging.New(logging.Config{Level: "info", Output: logging.OutputBoth, TraceIDKey: "trace.id", SpanIDKey: "span.id"},
		logging.WithWriter(&stdout), logging.WithLoggerProvider(lp.LoggerProvider()))
	if err != nil {
		t.Fatal(err)
	}

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(ctx, "charge")
	logger.InfoContext(ctx, "charged", "order_id", "o-1", "password", "hunter2")
	logger.DebugContext(ctx, "below the level")
	span.End()
	if err := lp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	traceID := span.SpanContext().TraceID()

	// Standard output: JSON with the configured trace keys.
	var line map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &line); err != nil {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if line["trace.id"] != traceID.String() || line["span.id"] == nil || line["trace_id"] != nil {
		t.Errorf("stdout trace fields: %v", line)
	}

	// OTLP: one record (debug filtered), redacted, trace ID native.
	recs := records(rc)
	if len(recs) != 1 {
		t.Fatalf("%d OTLP records, want 1", len(recs))
	}
	r := recs[0]
	if r.GetBody().GetStringValue() != "charged" {
		t.Errorf("body = %v", r.GetBody())
	}
	if v, _ := attr(r, "order_id"); v != "o-1" {
		t.Errorf("order_id = %q", v)
	}
	if v, _ := attr(r, "password"); v != logging.RedactedValue {
		t.Errorf("password = %q, want redacted", v)
	}
	if hex.EncodeToString(r.GetTraceId()) != traceID.String() {
		t.Errorf("record trace ID %x, want %s", r.GetTraceId(), traceID)
	}
	if _, ok := attr(r, "trace.id"); ok {
		t.Error("trace ID duplicated as an attribute")
	}
	if strings.Contains(stdout.String(), "hunter2") {
		t.Error("password logged to stdout")
	}
	if h := rc.Requests("/v1/logs")[0].Header.Get("api-key"); h != "k" {
		t.Errorf("api-key = %q", h)
	}
}

func TestOutputValidation(t *testing.T) {
	if _, err := logging.New(logging.Config{Output: logging.OutputOTLP}); err == nil {
		t.Error("otlp output without a logger provider accepted")
	}
	if _, err := logging.New(logging.Config{Output: "syslog"}); err == nil {
		t.Error("unknown output accepted")
	}
	if _, err := logs.Init(context.Background(), logs.Config{Exporter: "kafka"}); err == nil {
		t.Error("unknown exporter accepted")
	}
	lp, err := logs.Init(context.Background(), logs.Config{}) // exports nothing
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	logger, err := logging.New(logging.Config{Output: logging.OutputOTLP},
		logging.WithWriter(&stdout), logging.WithLoggerProvider(lp.LoggerProvider()))
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("x")
	if stdout.Len() != 0 {
		t.Errorf("otlp-only output wrote to stdout: %q", stdout.String())
	}
}
