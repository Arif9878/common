// Package requestid carries a request ID through a [context.Context].
//
// A request ID identifies one unit of work, such as an HTTP request, a gRPC
// call or a consumed message, in logs. The first service in a call chain
// generates it and every downstream hop propagates it unchanged in the
// [Header] header, so it also acts as a correlation ID across services.
// Distributed tracing (trace_id) serves the same purpose with more detail;
// the request ID remains useful for callers and systems that do not trace.
//
// This package holds only the context plumbing. Reading and writing headers
// is done by the transport middleware, which must pass incoming values
// through [Valid] before trusting them.
package requestid

import (
	"context"
	"crypto/rand"
)

// Header is the HTTP header and gRPC metadata key used to propagate request
// IDs. gRPC metadata keys are lower-cased by the transport.
const Header = "X-Request-ID"

// MaxLen is the maximum length of a request ID accepted by [Valid].
const MaxLen = 128

type ctxKey struct{}

// NewContext returns a copy of ctx carrying id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID stored in ctx, if any.
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}

// New returns a new random request ID with at least 128 bits of entropy.
func New() string {
	return rand.Text()
}

// Valid reports whether id is safe to accept from an untrusted caller: 1 to
// MaxLen characters, each a printable ASCII character other than space.
// This prevents log injection and unbounded values in logs.
func Valid(id string) bool {
	if len(id) == 0 || len(id) > MaxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}
