package errors

// FieldError describes one invalid field of a request: which field (as the
// client names it, such as "items[0].quantity"), the rule it broke (such as
// "required" or "min") and a message safe to show the client.
type FieldError struct {
	Field   string `json:"field"`
	Rule    string `json:"rule,omitempty"`
	Message string `json:"message"`
}

// WithFields attaches field errors to err without changing its kind or
// public message. httpserver lists them in the problem response's
// "errors" member, and grpcstatus sends them as a BadRequest detail.
// It returns nil for a nil err.
func WithFields(err error, fields ...FieldError) error {
	if err == nil {
		return nil
	}
	return &fieldsError{err: err, fields: fields}
}

// Fields returns the field errors attached to err with WithFields, or nil.
func Fields(err error) []FieldError {
	if fe, ok := AsType[*fieldsError](err); ok {
		return fe.fields
	}
	return nil
}

type fieldsError struct {
	err    error
	fields []FieldError
}

func (e *fieldsError) Error() string { return e.err.Error() }
func (e *fieldsError) Unwrap() error { return e.err }
