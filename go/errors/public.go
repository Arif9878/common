package errors

// WithPublicMessage attaches msg as the message that may be shown to callers
// of a service. It does not change the Kind or Error() text of err.
// It returns nil if err is nil.
//
// Only use text that is safe to expose: no identifiers of other users,
// internal hostnames, queries or upstream responses.
func WithPublicMessage(err error, msg string) error {
	if err == nil {
		return nil
	}
	return &publicError{msg: msg, err: err}
}

// PublicMessage returns a message describing err that is safe to send to
// callers: the outermost message attached with WithPublicMessage, or else a
// generic text for KindOf(err), such as "not found". It never returns the
// text of err itself. It returns "" for a nil error.
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	if pe, ok := AsType[*publicError](err); ok {
		return pe.msg
	}
	return KindOf(err).publicMessage()
}

type publicError struct {
	msg string
	err error
}

func (e *publicError) Error() string { return e.err.Error() }
func (e *publicError) Unwrap() error { return e.err }
