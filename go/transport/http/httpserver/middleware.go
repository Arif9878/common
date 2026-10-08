package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/requestid"
)

// Middleware wraps an http.Handler.
type Middleware = func(http.Handler) http.Handler

// Chain applies middleware so that the first one is outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// RequestID accepts the incoming X-Request-ID header if [requestid.Valid],
// otherwise generates a new ID. It stores the ID in the request context and
// sets it on the response.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestid.Header)
			if !requestid.Valid(id) {
				id = requestid.New()
			}
			w.Header().Set(requestid.Header, id)
			next.ServeHTTP(w, r.WithContext(requestid.NewContext(r.Context(), id)))
		})
	}
}

// Recover turns a panic in next into a 500 response and logs it with its
// stack trace. A panic with http.ErrAbortHandler is re-raised, as net/http
// expects. If the handler had already started the response, the
// connection is aborted instead, since the status can no longer change.
func Recover(logger *slog.Logger) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tw := &trackingWriter{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as net/http does
					panic(rec)
				}
				err := errors.Internal.Errorf("panic: %v", rec)
				logger.ErrorContext(r.Context(), "http handler panicked",
					logging.Err(err), "stack", string(debug.Stack()))
				if tw.wroteHeader {
					recordError(r.Context(), err)
					panic(http.ErrAbortHandler)
				}
				WriteError(tw, r, err)
			}()
			next.ServeHTTP(tw, r)
		})
	}
}

// MaxBytes limits request bodies to n bytes. Requests whose Content-Length
// exceeds n get 413 immediately; for other bodies, reads past n fail with
// an *http.MaxBytesError, which [WriteError] maps to 413.
func MaxBytes(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > n {
				WriteError(w, r, &http.MaxBytesError{Limit: n})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout sets a deadline of d on the request context. Handlers and the
// calls they make must respect it; when it expires they typically return
// context.DeadlineExceeded, which WriteError maps to 504. Unlike
// http.TimeoutHandler it does not buffer responses, so streaming works.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Authenticator checks a request's credentials and returns a context
// carrying the caller's identity. It returns an error of kind Unauthorized
// (missing or invalid credentials) or Forbidden (valid, not allowed);
// other kinds map as in [StatusFor].
type Authenticator func(r *http.Request) (context.Context, error)

// Auth rejects requests that authenticate fails, with the status from the
// error's kind, and otherwise passes the returned context on. Errors with
// no kind are treated as Unauthorized, never as a server error, so a
// misbehaving authenticator does not leak details through a 500.
func Auth(authenticate Authenticator) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := authenticate(r)
			if err != nil {
				if errors.KindOf(err) == errors.Unknown {
					err = errors.Unauthorized.Wrap(err, "authenticate")
				}
				WriteError(w, r, err)
				return
			}
			if ctx == nil {
				ctx = r.Context()
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// trackingWriter records whether the response has started.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *trackingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingWriter) Flush() {
	w.wroteHeader = true
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
