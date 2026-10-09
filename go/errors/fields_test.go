package errors_test

import (
	"testing"

	"github.com/Arif9878/common/go/errors"
)

func TestFields(t *testing.T) {
	base := errors.WithPublicMessage(errors.InvalidArgument.New("invalid order"), "the order is invalid")
	err := errors.WithFields(base, errors.FieldError{Field: "amount", Rule: "min", Message: "must be at least 1"})
	if errors.KindOf(err) != errors.InvalidArgument || errors.PublicMessage(err) != "the order is invalid" {
		t.Errorf("kind %v, public %q: WithFields changed them", errors.KindOf(err), errors.PublicMessage(err))
	}
	wrapped := errors.Internal.Wrap(err, "handler") // an outer classification wins, the fields stay
	if f := errors.Fields(wrapped); len(f) != 1 || f[0].Field != "amount" {
		t.Errorf("Fields = %v", f)
	}
	if errors.Fields(base) != nil || errors.WithFields(nil) != nil {
		t.Error("fields without WithFields")
	}
}
