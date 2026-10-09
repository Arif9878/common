package logging

import (
	"context"
	"log/slog"
	"slices"
)

// fanout sends each record to every handler that is enabled for it.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	return slices.ContainsFunc(f, func(h slog.Handler) bool { return h.Enabled(ctx, level) })
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range f {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// otlpOutput adapts the OpenTelemetry bridge handler: it applies the
// logger's level and drops the trace fields, which OTLP records carry as
// native trace and span IDs from the context.
type otlpOutput struct {
	next  slog.Handler
	level slog.Leveler
	drop  []string
}

func (o *otlpOutput) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= o.level.Level() && o.next.Enabled(ctx, level)
}

func (o *otlpOutput) Handle(ctx context.Context, r slog.Record) error {
	keep := true
	r.Attrs(func(a slog.Attr) bool {
		keep = !slices.Contains(o.drop, a.Key)
		return keep
	})
	if !keep {
		out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
		r.Attrs(func(a slog.Attr) bool {
			if !slices.Contains(o.drop, a.Key) {
				out.AddAttrs(a)
			}
			return true
		})
		r = out
	}
	return o.next.Handle(ctx, r)
}

func (o *otlpOutput) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &otlpOutput{next: o.next.WithAttrs(attrs), level: o.level, drop: o.drop}
}

func (o *otlpOutput) WithGroup(name string) slog.Handler {
	return &otlpOutput{next: o.next.WithGroup(name), level: o.level, drop: o.drop}
}
