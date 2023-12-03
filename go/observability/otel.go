package observability

import (
	"context"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type observeOtelImpl struct {
	traceApp *sdktrace.TracerProvider
}

func (impl observeOtelImpl) Tracer() Tracer {
	return impl
}

func (impl observeOtelImpl) Shutdown(ctx context.Context) error {
	impl.traceApp.Shutdown(ctx)
	return nil
}

func (observeOtelImpl) SpanFromContext(ctx context.Context) Span {
	return trace.SpanFromContext(ctx)
}

func (impl observeOtelImpl) Start(ctx context.Context, spanName string, opts ...SpanOptions) (context.Context, Span) {
	span := trace.SpanFromContext(ctx)
	return trace.ContextWithSpan(ctx, span), span
}
