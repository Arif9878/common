package logging

import (
	"bytes"
	"context"
	"log/slog"
	"slices"

	"go.opentelemetry.io/otel/trace"

	"github.com/Arif9878/common/go/requestid"
)

// RedactedValue replaces the value of sensitive attributes.
const RedactedValue = "[REDACTED]"

var defaultRedactKeys = []string{
	"password", "passwd", "secret", "token", "apikey",
	"authorization", "cookie", "privatekey", "credential",
}

// NewHandler wraps next with context enrichment and redaction, as described
// in the package documentation. Only [WithRedactKeys] applies; the other
// options configure the base handler built by [New].
func NewHandler(next slog.Handler, opts ...Option) slog.Handler {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return newHandler(next, o.redactKeys)
}

func newHandler(next slog.Handler, extra []string) *handler {
	patterns := make([][]byte, 0, len(defaultRedactKeys)+len(extra))
	for _, k := range slices.Concat(defaultRedactKeys, extra) {
		if n := normalize(k); len(n) > 0 {
			patterns = append(patterns, n)
		}
	}
	return &handler{next: next, redact: &redactor{patterns: patterns}, traceKey: KeyTraceID, spanKey: KeySpanID}
}

// handler is immutable after construction and safe for concurrent use.
//
// Attributes added before the first WithGroup are passed to next.WithAttrs,
// so the base handler can pre-format them. After a group is opened, groups
// and attributes are kept in goas and applied in Handle; this lets the
// context fields stay top-level instead of landing inside the group.
type handler struct {
	next     slog.Handler
	redact   *redactor
	goas     []groupOrAttrs
	traceKey string
	spanKey  string
}

type groupOrAttrs struct {
	group string      // group name if non-empty
	attrs []slog.Attr // attrs if group is empty
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	attrs = h.redact.attrs(attrs)
	h2 := *h
	if len(h.goas) == 0 {
		h2.next = h.next.WithAttrs(attrs)
		return &h2
	}
	h2.goas = append(slices.Clip(h.goas), groupOrAttrs{attrs: attrs})
	return &h2
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.goas = append(slices.Clip(h.goas), groupOrAttrs{group: name})
	return &h2
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	ctxAttrs := h.contextAttrs(ctx)

	needsRedact := false
	r.Attrs(func(a slog.Attr) bool {
		needsRedact = h.redact.needed(a)
		return !needsRedact
	})

	// Fast path: no groups to apply and nothing to redact.
	if len(h.goas) == 0 && !needsRedact {
		if len(ctxAttrs) > 0 {
			r = r.Clone()
			r.AddAttrs(ctxAttrs...)
		}
		return h.next.Handle(ctx, r)
	}

	attrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, h.redact.attr(a))
		return true
	})
	for i := len(h.goas) - 1; i >= 0; i-- {
		goa := h.goas[i]
		if goa.group != "" {
			attrs = []slog.Attr{{Key: goa.group, Value: slog.GroupValue(attrs...)}}
		} else {
			attrs = slices.Concat(goa.attrs, attrs)
		}
	}

	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(attrs...)
	out.AddAttrs(ctxAttrs...)
	return h.next.Handle(ctx, out)
}

func (h *handler) contextAttrs(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	var buf [3]slog.Attr
	attrs := buf[:0]
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs,
			slog.String(h.traceKey, sc.TraceID().String()),
			slog.String(h.spanKey, sc.SpanID().String()),
		)
	}
	if id, ok := requestid.FromContext(ctx); ok {
		attrs = append(attrs, slog.String(KeyRequestID, id))
	}
	if extra := attrsFromContext(ctx); len(extra) > 0 {
		attrs = append(attrs, h.redact.attrs(extra)...)
	}
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

type redactor struct {
	patterns [][]byte // normalized
}

func (rd *redactor) sensitive(key string) bool {
	var buf [64]byte
	k := appendNormalized(buf[:0], key)
	for _, p := range rd.patterns {
		if bytes.Contains(k, p) {
			return true
		}
	}
	return false
}

// needed reports whether a, or anything nested in it, must be redacted.
func (rd *redactor) needed(a slog.Attr) bool {
	if rd.sensitive(a.Key) {
		return true
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		return slices.ContainsFunc(a.Value.Group(), rd.needed)
	case slog.KindLogValuer:
		return rd.needed(slog.Attr{Value: a.Value.Resolve()})
	}
	return false
}

func (rd *redactor) attr(a slog.Attr) slog.Attr {
	if rd.sensitive(a.Key) {
		return slog.String(a.Key, RedactedValue)
	}
	if a.Value.Kind() == slog.KindLogValuer {
		a.Value = a.Value.Resolve()
	}
	if a.Value.Kind() == slog.KindGroup {
		a.Value = slog.GroupValue(rd.attrs(a.Value.Group())...)
	}
	return a
}

// attrs returns attrs with sensitive values redacted. It returns the input
// slice unchanged when nothing needs redaction.
func (rd *redactor) attrs(attrs []slog.Attr) []slog.Attr {
	if !slices.ContainsFunc(attrs, rd.needed) {
		return attrs
	}
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = rd.attr(a)
	}
	return out
}

func normalize(s string) []byte {
	return appendNormalized(nil, s)
}

// appendNormalized appends s lower-cased with '_', '-' and '.' removed.
func appendNormalized(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_' || c == '-' || c == '.':
			continue
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}
