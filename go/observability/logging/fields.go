package logging

import (
	"context"
	"log/slog"
	"time"

	"github.com/Arif9878/common/go/errors"
)

// Standard field names.
const (
	KeyService     = "service"
	KeyEnvironment = "environment"
	KeyVersion     = "version"

	KeyTraceID   = "trace_id"
	KeySpanID    = "span_id"
	KeyRequestID = "request_id"

	KeyTopic     = "topic"
	KeyPartition = "partition"
	KeyOffset    = "offset"

	KeyDurationMS = "duration_ms"
	KeyError      = "error"
	KeyErrorType  = "error_type"
)

// Err returns the attributes error (the error text) and error_type (the
// [errors.Kind] of err, such as "unavailable"). It returns an empty
// attribute, which slog ignores, if err is nil.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.Group("",
		slog.String(KeyError, err.Error()),
		slog.String(KeyErrorType, errors.KindOf(err).String()),
	)
}

// Duration returns the attribute duration_ms with d in fractional
// milliseconds.
func Duration(d time.Duration) slog.Attr {
	return slog.Float64(KeyDurationMS, float64(d)/float64(time.Millisecond))
}

type attrsKey struct{}

// ContextWithAttrs returns a copy of ctx carrying attrs in addition to any
// attributes already attached. The handler adds them as top-level fields to
// every record logged with that context.
//
// Use it for identifiers of the current unit of work, such as topic,
// partition and offset, so code deeper in the call chain does not need to
// pass them along. Keep the set small; every record pays for it.
func ContextWithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	prev := attrsFromContext(ctx)
	merged := make([]slog.Attr, 0, len(prev)+len(attrs))
	merged = append(merged, prev...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, attrsKey{}, merged)
}

func attrsFromContext(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	return attrs
}
