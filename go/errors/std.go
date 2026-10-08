package errors

import stderrors "errors"

// ErrUnsupported is [errors.ErrUnsupported] from the standard library.
var ErrUnsupported = stderrors.ErrUnsupported

// New is [errors.New] from the standard library. The returned error is
// unclassified; use [Kind.New] to create a classified error.
func New(text string) error { return stderrors.New(text) }

// Is is [errors.Is] from the standard library.
func Is(err, target error) bool { return stderrors.Is(err, target) }

// As is [errors.As] from the standard library.
func As(err error, target any) bool { return stderrors.As(err, target) }

// AsType is [errors.AsType] from the standard library.
func AsType[E error](err error) (E, bool) { return stderrors.AsType[E](err) }

// Unwrap is [errors.Unwrap] from the standard library.
func Unwrap(err error) error { return stderrors.Unwrap(err) }

// Join is [errors.Join] from the standard library.
func Join(errs ...error) error { return stderrors.Join(errs...) }
