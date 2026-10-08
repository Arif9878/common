// Package echoadapter applies the httpserver middleware and error responses
// to Echo (github.com/labstack/echo/v4) applications.
//
//	e := echo.New()
//	e.HTTPErrorHandler = echoadapter.ErrorHandler
//	e.Use(echoadapter.Middleware(httpserver.WithLogger(logger)))
//
// Register Middleware with e.Use, not e.Pre: Use middleware runs after
// routing, so the route template (c.Path(), such as "/orders/:id") is known
// and used for spans, metrics and logs.
//
// Handler errors are rendered by e.HTTPErrorHandler inside the middleware,
// so the access log and metrics record the final status. With
// [ErrorHandler], errors classified with the errors package get the same
// problem+json responses as net/http services, and echo.HTTPError values
// keep their status code.
package echoadapter

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

type contextKey struct{}

type call struct {
	c    echo.Context
	next echo.HandlerFunc
}

// Middleware returns Echo middleware running the httpserver.Handler chain
// (request ID, observation, panic recovery, body limit, timeout) with
// Echo's route template. opts are httpserver options; WithRoute is set by
// the adapter.
func Middleware(opts ...httpserver.Option) echo.MiddlewareFunc {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cl := r.Context().Value(contextKey{}).(*call)
		cl.c.SetRequest(r)
		cl.c.Response().Writer = w
		if err := cl.next(cl.c); err != nil {
			cl.c.Error(err)
		}
	})
	chain := httpserver.Handler(inner, append(opts, httpserver.WithRoute(func(r *http.Request) string {
		if cl, ok := r.Context().Value(contextKey{}).(*call); ok {
			return cl.c.Path()
		}
		return ""
	}))...)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			orig := c.Response().Writer
			defer func() { c.Response().Writer = orig }()
			ctx := context.WithValue(req.Context(), contextKey{}, &call{c: c, next: next})
			chain.ServeHTTP(orig, req.WithContext(ctx))
			return nil // errors were rendered by c.Error inside the chain
		}
	}
}

// ErrorHandler is an echo.HTTPErrorHandler that writes httpserver problem
// responses. An *echo.HTTPError keeps its status; its message is used as
// the detail only for 4xx statuses. Other errors go through
// httpserver.WriteError.
func ErrorHandler(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}
	if he, ok := errors.AsType[*echo.HTTPError](err); ok {
		httpserver.WriteErrorStatus(c.Response(), c.Request(), he.Code, fromHTTPError(he))
		return
	}
	httpserver.WriteError(c.Response(), c.Request(), err)
}

// fromHTTPError classifies an echo.HTTPError by its status, for the
// problem's code field and logs.
func fromHTTPError(he *echo.HTTPError) error {
	kind := errors.Internal
	switch he.Code {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusMethodNotAllowed:
		kind = errors.InvalidArgument
	case http.StatusUnauthorized:
		kind = errors.Unauthorized
	case http.StatusForbidden:
		kind = errors.Forbidden
	case http.StatusNotFound:
		kind = errors.NotFound
	case http.StatusConflict:
		kind = errors.Conflict
	case http.StatusTooManyRequests:
		kind = errors.RateLimited
	case http.StatusServiceUnavailable:
		kind = errors.Unavailable
	case http.StatusGatewayTimeout:
		kind = errors.Timeout
	}
	err := kind.Wrap(he, "")
	if msg, ok := he.Message.(string); ok && he.Code >= 400 && he.Code < 500 {
		err = errors.WithPublicMessage(err, msg)
	}
	return err
}
