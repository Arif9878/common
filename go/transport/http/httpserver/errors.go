package httpserver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/requestid"
)

// StatusClientClosedRequest is the non-standard status (from nginx) used
// when the client canceled the request.
const StatusClientClosedRequest = 499

// StatusFor returns the HTTP status code for err:
//
//	InvalidArgument 400   Unauthorized 401   Forbidden 403   NotFound 404
//	Conflict 409          RateLimited 429    Canceled 499    Unavailable 503
//	Timeout 504           Internal, Unknown 500
//
// An *http.MaxBytesError (request body too large) maps to 413. StatusFor
// returns 200 for a nil error.
func StatusFor(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return http.StatusRequestEntityTooLarge
	}
	switch errors.KindOf(err) {
	case errors.InvalidArgument:
		return http.StatusBadRequest
	case errors.Unauthorized:
		return http.StatusUnauthorized
	case errors.Forbidden:
		return http.StatusForbidden
	case errors.NotFound:
		return http.StatusNotFound
	case errors.Conflict:
		return http.StatusConflict
	case errors.RateLimited:
		return http.StatusTooManyRequests
	case errors.Canceled:
		return StatusClientClosedRequest
	case errors.Unavailable:
		return http.StatusServiceUnavailable
	case errors.Timeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// Problem is the RFC 9457 problem document written by [WriteError].
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	// Detail is errors.PublicMessage of the error: never internal details.
	Detail string `json:"detail,omitempty"`
	// Code is the error kind, such as "not_found".
	Code      string `json:"code"`
	RequestID string `json:"request_id,omitempty"`
	// Errors lists invalid fields, from errors.WithFields (the validation
	// package attaches them).
	Errors []errors.FieldError `json:"errors,omitempty"`
}

// WriteError writes err as a problem+json response with the status from
// [StatusFor], and attaches err to the request's access-log line. If err
// has a RetryAfter() time.Duration method (rate-limit errors do), the
// Retry-After header is set. Nothing is written if err is nil.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	WriteErrorStatus(w, r, StatusFor(err), err)
}

// WriteErrorStatus is like [WriteError] with an explicit status code, for
// adapters whose errors already carry one.
func WriteErrorStatus(w http.ResponseWriter, r *http.Request, status int, err error) {
	if err == nil {
		return
	}
	recordError(r.Context(), err)

	p := Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: errors.PublicMessage(err),
		Code:   errors.KindOf(err).String(),
		Errors: errors.Fields(err),
	}
	if status == http.StatusRequestEntityTooLarge {
		p.Code, p.Detail = errors.InvalidArgument.String(), "request body too large"
	}
	if status == StatusClientClosedRequest {
		p.Title = "Client Closed Request"
	}
	if p.Title == "" {
		p.Title = "Error"
	}
	p.RequestID, _ = requestid.FromContext(r.Context())

	if ra, ok := errors.AsType[interface {
		error
		RetryAfter() time.Duration
	}](err); ok && ra.RetryAfter() > 0 {
		secs := int((ra.RetryAfter() + time.Second - 1) / time.Second) // round up
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}
