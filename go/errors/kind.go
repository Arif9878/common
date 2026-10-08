package errors

import (
	"context"
	"fmt"
)

// Kind is the category of an error. The zero value is [Unknown].
//
// New kinds may be added in minor releases; code switching on Kind should
// have a default case.
type Kind uint8

const (
	// Unknown means the error has not been classified. Transports treat it
	// like Internal.
	Unknown Kind = iota
	// InvalidArgument means the caller sent a malformed or invalid request.
	InvalidArgument
	// NotFound means the requested entity does not exist.
	NotFound
	// Conflict means the request conflicts with the current state, such as a
	// duplicate key or a failed optimistic-concurrency check.
	Conflict
	// Unauthorized means the caller is not authenticated.
	Unauthorized
	// Forbidden means the caller is authenticated but not allowed.
	Forbidden
	// Timeout means an operation did not complete before its deadline.
	Timeout
	// Unavailable means a dependency is temporarily unavailable.
	Unavailable
	// RateLimited means the caller or a dependency exceeded a rate limit.
	RateLimited
	// Canceled means the caller canceled the operation.
	Canceled
	// Internal means a bug or broken invariant in the service.
	Internal

	numKinds
)

var kindInfo = [numKinds]struct {
	name   string // stable, snake_case; used as log field and metric label
	public string // generic message that is safe to send to callers
}{
	Unknown:         {"unknown", "internal error"},
	InvalidArgument: {"invalid_argument", "invalid argument"},
	NotFound:        {"not_found", "not found"},
	Conflict:        {"conflict", "conflict"},
	Unauthorized:    {"unauthorized", "unauthorized"},
	Forbidden:       {"forbidden", "forbidden"},
	Timeout:         {"timeout", "timeout"},
	Unavailable:     {"unavailable", "service unavailable"},
	RateLimited:     {"rate_limited", "rate limited"},
	Canceled:        {"canceled", "canceled"},
	Internal:        {"internal", "internal error"},
}

// String returns the stable snake_case name of k, such as "not_found".
// It is suitable as a log field or a low-cardinality metric label.
// Out-of-range values return "unknown".
func (k Kind) String() string {
	if k >= numKinds {
		return kindInfo[Unknown].name
	}
	return kindInfo[k].name
}

func (k Kind) publicMessage() string {
	if k >= numKinds {
		return kindInfo[Unknown].public
	}
	return kindInfo[k].public
}

// New returns a new error of kind k with message msg. It is typically used to
// declare classified sentinel errors.
func (k Kind) New(msg string) error {
	return &kindError{kind: k, msg: msg}
}

// Errorf formats according to a format specifier and returns an error of
// kind k. A %w verb wraps its operand as with [fmt.Errorf].
func (k Kind) Errorf(format string, args ...any) error {
	return &kindError{kind: k, err: fmt.Errorf(format, args...)}
}

// Wrap classifies err as kind k and annotates it with msg, which may be empty.
// It returns nil if err is nil.
func (k Kind) Wrap(err error, msg string) error {
	if err == nil {
		return nil
	}
	return &kindError{kind: k, msg: msg, err: err}
}

// KindOf returns the Kind of err. See the package documentation for the
// classification rules. It returns Unknown for a nil error.
func KindOf(err error) Kind {
	if err == nil {
		return Unknown
	}
	if ke, ok := AsType[*kindError](err); ok {
		return ke.kind
	}
	switch {
	case Is(err, context.Canceled):
		return Canceled
	case Is(err, context.DeadlineExceeded):
		return Timeout
	}
	if te, ok := AsType[interface {
		error
		Timeout() bool
	}](err); ok && te.Timeout() {
		return Timeout
	}
	return Unknown
}

// IsRetryable reports whether err is of a Kind that is usually transient:
// Timeout, Unavailable or RateLimited. Whether retrying is safe also depends
// on the operation being idempotent, which only the caller knows.
func IsRetryable(err error) bool {
	switch KindOf(err) {
	case Timeout, Unavailable, RateLimited:
		return true
	default:
		return false
	}
}

// kindError attaches a Kind to an error. It is unexported so its
// representation can evolve; use KindOf to inspect it.
type kindError struct {
	kind Kind
	msg  string
	err  error
}

func (e *kindError) Error() string {
	switch {
	case e.err == nil && e.msg == "":
		return e.kind.String()
	case e.err == nil:
		return e.msg
	case e.msg == "":
		return e.err.Error()
	default:
		return e.msg + ": " + e.err.Error()
	}
}

func (e *kindError) Unwrap() error { return e.err }
