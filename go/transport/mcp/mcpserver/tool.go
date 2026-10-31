package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/auth/jwtauth"
	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
)

// Tool is a tool for [AddTool]. In and Out are structs: their JSON schemas
// (from json and jsonschema tags) are the tool's input and output schemas,
// and inputs are validated against them before Handle runs.
type Tool[In, Out any] struct {
	// Name is how the model calls the tool, such as "get_order": unique in
	// the server. Required.
	Name string
	// Title is a name for people, such as "Get order".
	Title string
	// Description tells the model what the tool does and when to use it.
	// It is the model's only documentation: say what the input means and
	// what the result holds.
	Description string
	// Scopes are the OAuth scopes the caller's token must all grant,
	// compared exactly. Without them any authenticated caller may call the
	// tool.
	Scopes []string
	// ReadOnly says the tool changes nothing. Destructive says it may
	// delete or overwrite (only meaningful when not ReadOnly); Idempotent,
	// that calling it again with the same input changes nothing more.
	// They are hints clients show users before calling, not enforced.
	ReadOnly, Destructive, Idempotent bool
	// Handle runs the tool. The caller is in ctx (jwtauth.FromContext).
	// Required.
	Handle func(ctx context.Context, in In) (Out, error)
}

// AddTool adds t to s. It panics if t has no name or handler, its name is
// taken, a scope is empty or has spaces, or In or Out has no JSON schema,
// as the SDK does: these are programming errors found at start-up.
func AddTool[In, Out any](s *Server, t Tool[In, Out]) {
	if t.Name == "" || t.Handle == nil {
		panic("mcpserver: a tool needs a Name and a Handle")
	}
	for _, sc := range t.Scopes {
		if sc == "" || strings.ContainsAny(sc, " \t\n\"\\") {
			panic(fmt.Sprintf("mcpserver: tool %s: invalid scope %q", t.Name, sc))
		}
	}
	s.mu.Lock()
	if _, taken := s.scopes[t.Name]; taken {
		s.mu.Unlock()
		panic("mcpserver: tool " + t.Name + " added twice")
	}
	s.scopes[t.Name] = slices.Clone(t.Scopes)
	s.mu.Unlock()

	annotations := &mcp.ToolAnnotations{Title: t.Title, ReadOnlyHint: t.ReadOnly, IdempotentHint: t.Idempotent}
	if !t.ReadOnly {
		annotations.DestructiveHint = &t.Destructive
	}
	attrs := []attribute.KeyValue{attrMethod, attribute.String("gen_ai.tool.name", t.Name)}
	mcp.AddTool(s.sdk, &mcp.Tool{Name: t.Name, Title: t.Title, Description: t.Description, Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			start := time.Now()
			ctx, span := s.tracer.Start(ctx, "tools/call "+t.Name,
				trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
			defer span.End()

			var out Out
			var err error
			if len(t.Scopes) > 0 {
				err = jwtauth.CheckScopes(ctx, t.Scopes...)
			}
			if err == nil {
				out, err = call(ctx, t.Handle, in)
			}
			s.record(ctx, span, t.Name, attrs, time.Since(start), err)
			if err != nil {
				return nil, out, toolError{err}
			}
			return nil, out, nil
		})
}

var attrMethod = attribute.String("mcp.method.name", "tools/call")

// call runs handle, turning a panic into an Internal error.
func call[In, Out any](ctx context.Context, handle func(context.Context, In) (Out, error), in In) (out Out, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = errors.Internal.Errorf("mcpserver: panic: %v\n%s", p, debug.Stack())
		}
	}()
	return handle(ctx, in)
}

// toolError is what the SDK puts in the tool result: only the kind and
// the public message.
type toolError struct{ err error }

func (e toolError) Error() string {
	return errors.KindOf(e.err).String() + ": " + errors.PublicMessage(e.err)
}

// record ends a tool call's telemetry: metric, span status and log line.
func (s *Server) record(ctx context.Context, span trace.Span, tool string, attrs []attribute.KeyValue, d time.Duration, err error) {
	level := slog.LevelInfo
	if err != nil {
		kind := errors.KindOf(err)
		attrs = append(attrs, attribute.String("error.type", kind.String()))
		span.SetStatus(codes.Error, errors.PublicMessage(err))
		if kind == errors.Internal || kind == errors.Unknown {
			span.RecordError(err)
			level = slog.LevelError
		}
	}
	s.duration.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
	s.o.logger.LogAttrs(ctx, level, "mcp tool call", slog.String("tool", tool), logging.Duration(d), logging.Err(err))
}
