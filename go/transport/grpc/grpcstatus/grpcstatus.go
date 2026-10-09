// Package grpcstatus maps between error kinds (see the errors package) and
// gRPC status codes, in both directions, so a classified error keeps its
// meaning across service boundaries:
//
//	server: errors.NotFound.New(...)  --ToStatus-->  codes.NotFound, public message
//	client: codes.NotFound            --FromStatus-> error of kind NotFound (still a status)
//
// The grpcserver and grpcclient packages apply these automatically.
package grpcstatus

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Arif9878/common/go/errors"
)

// CodeFor returns the gRPC code for a kind:
//
//	InvalidArgument  InvalidArgument     Unauthorized  Unauthenticated
//	Forbidden        PermissionDenied    NotFound      NotFound
//	Conflict         AlreadyExists       RateLimited   ResourceExhausted
//	Canceled         Canceled            Timeout       DeadlineExceeded
//	Unavailable      Unavailable         Internal, Unknown  Internal
func CodeFor(kind errors.Kind) codes.Code {
	switch kind {
	case errors.InvalidArgument:
		return codes.InvalidArgument
	case errors.Unauthorized:
		return codes.Unauthenticated
	case errors.Forbidden:
		return codes.PermissionDenied
	case errors.NotFound:
		return codes.NotFound
	case errors.Conflict:
		return codes.AlreadyExists
	case errors.RateLimited:
		return codes.ResourceExhausted
	case errors.Canceled:
		return codes.Canceled
	case errors.Timeout:
		return codes.DeadlineExceeded
	case errors.Unavailable:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}

// KindFor returns the kind for a gRPC code. Codes without a direct
// counterpart: FailedPrecondition and OutOfRange map to InvalidArgument,
// Aborted to Conflict, and Unknown, Unimplemented and DataLoss to Internal.
func KindFor(code codes.Code) errors.Kind {
	switch code {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return errors.InvalidArgument
	case codes.Unauthenticated:
		return errors.Unauthorized
	case codes.PermissionDenied:
		return errors.Forbidden
	case codes.NotFound:
		return errors.NotFound
	case codes.AlreadyExists, codes.Aborted:
		return errors.Conflict
	case codes.ResourceExhausted:
		return errors.RateLimited
	case codes.Canceled:
		return errors.Canceled
	case codes.DeadlineExceeded:
		return errors.Timeout
	case codes.Unavailable:
		return errors.Unavailable
	default:
		return errors.Internal
	}
}

// Code returns the code err is sent with by [ToStatus]. It returns OK for
// nil.
func Code(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	if s, ok := passThrough(err); ok {
		return s.Code()
	}
	return CodeFor(errors.KindOf(err))
}

// ToStatus converts err into the error a server returns: a status with
// Code(err) and errors.PublicMessage(err) as message.
//
// An error carrying a gRPC status (status.Error, or an error received from
// another service) is sent with that status, message and details, unless
// err was classified differently on top of it: then the outer
// classification wins, as everywhere in the errors package. It returns nil
// for nil.
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := passThrough(err); ok {
		return s.Err()
	}
	s := status.New(CodeFor(errors.KindOf(err)), errors.PublicMessage(err))
	if fields := errors.Fields(err); len(fields) > 0 {
		br := &errdetails.BadRequest{}
		for _, f := range fields {
			br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{
				Field: f.Field, Description: f.Message, Reason: f.Rule,
			})
		}
		if withDetails, derr := s.WithDetails(br); derr == nil {
			s = withDetails
		}
	}
	return s.Err()
}

// passThrough returns err's status if it should be sent unchanged: err
// carries a status and its kind is unknown or agrees with the status code.
func passThrough(err error) (*status.Status, bool) {
	se, ok := errors.AsType[interface {
		error
		GRPCStatus() *status.Status
	}](err)
	if !ok {
		return nil, false
	}
	s := se.GRPCStatus()
	kind := errors.KindOf(err)
	if kind != errors.Unknown && kind != KindFor(s.Code()) {
		return nil, false
	}
	return s, true
}

// FromStatus classifies an error returned by a gRPC call by its code.
// The result still carries the status, so status.Code, status.FromError
// and errors.Is with the original error keep working. Errors without a
// status (connection setup failures) are classified as Unavailable, and
// context errors keep their kinds. It returns nil for nil.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}
	s, ok := status.FromError(err)
	if !ok {
		if errors.KindOf(err) != errors.Unknown {
			return err
		}
		return errors.Unavailable.Wrap(err, "")
	}
	kinded := KindFor(s.Code()).Wrap(err, "")
	if fields := fieldsOf(s); len(fields) > 0 {
		kinded = errors.WithFields(kinded, fields...)
	}
	return &statusError{status: s, kinded: kinded}
}

// fieldsOf returns the field violations of a status's BadRequest details.
func fieldsOf(s *status.Status) []errors.FieldError {
	var out []errors.FieldError
	for _, d := range s.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				out = append(out, errors.FieldError{Field: v.GetField(), Rule: v.GetReason(), Message: v.GetDescription()})
			}
		}
	}
	return out
}

// statusError is a classified error that still presents its original
// status. status.FromError checks GRPCStatus on the error itself first;
// without this method it would find the wrapped status and replace its
// message with the whole error string.
type statusError struct {
	status *status.Status
	kinded error // kind wrapper around the original error
}

func (e *statusError) Error() string              { return e.kinded.Error() }
func (e *statusError) Unwrap() error              { return e.kinded }
func (e *statusError) GRPCStatus() *status.Status { return e.status }
