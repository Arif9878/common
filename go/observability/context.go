package observability

import "context"

type ctxKey struct{}

var (
	// ContextKey the context key for Tracer.
	ContextKey ctxKey = ctxKey(struct{}{})
)

// WithContext wrap ctx with tracer.
func WithContext(ctx context.Context, tracer Tracer) context.Context {
	return context.WithValue(ctx, ContextKey, tracer)
}
