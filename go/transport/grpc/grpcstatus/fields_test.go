package grpcstatus_test

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/grpc/grpcstatus"
)

func TestFieldErrorsRoundTrip(t *testing.T) {
	err := errors.WithFields(errors.InvalidArgument.New("invalid order"),
		errors.FieldError{Field: "amount", Rule: "min", Message: "must be at least 1"},
		errors.FieldError{Field: "customer_id", Rule: "required", Message: "is required"})
	sent := grpcstatus.ToStatus(err)
	if status.Code(sent) != codes.InvalidArgument {
		t.Fatalf("code = %v", status.Code(sent))
	}
	received := grpcstatus.FromStatus(sent) // in the calling service
	f := errors.Fields(received)
	if len(f) != 2 || f[0] != (errors.FieldError{Field: "amount", Rule: "min", Message: "must be at least 1"}) || f[1].Field != "customer_id" {
		t.Errorf("fields after the round trip = %+v", f)
	}
}
