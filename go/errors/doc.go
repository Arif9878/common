// Package errors classifies errors by [Kind] so that transports, retry
// policies, logs and metrics can react to them uniformly, without every
// service depending on a shared catalogue of business errors.
//
// Services keep defining their own sentinel and typed errors. This package
// only attaches a category to them:
//
//	var ErrUserNotFound = errors.NotFound.New("user not found")
//
//	func (r *Repo) Get(ctx context.Context, id string) (*User, error) {
//		row, err := r.db.Query(ctx, ...)
//		if err != nil {
//			return nil, errors.Unavailable.Wrap(err, "query user")
//		}
//		...
//	}
//
// The original error is preserved, so [Is], [As], [AsType] and [Unwrap] keep
// working through classified errors. Those functions, along with [New] and
// [Join], are re-exported from the standard library so that importing this
// package does not shadow it.
//
// # Classification rules
//
// [KindOf] walks the error tree in the same order as [As] and returns the
// Kind of the first classified error it finds. The outermost classification
// therefore wins: a layer that re-wraps an error with a different Kind
// deliberately overrides the inner one. Unclassified errors are then checked
// for well-known conditions: [context.Canceled] is [Canceled], and
// [context.DeadlineExceeded] or any error with a Timeout() bool method that
// reports true (net.Error, os.ErrDeadlineExceeded) is [Timeout]. Anything
// else is [Unknown], which transports treat like [Internal].
//
// # Messages
//
// Error() returns the full internal message chain, which may contain
// identifiers, SQL or upstream responses. It is for logs, never for clients.
// [PublicMessage] returns text that is safe to send to a caller: either a
// message attached with [WithPublicMessage] or a generic text derived from
// the Kind.
//
// Mapping a Kind to HTTP status codes or gRPC codes is the responsibility of
// the transport packages, keeping this package free of dependencies.
package errors
