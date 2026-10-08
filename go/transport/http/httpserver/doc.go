// Package httpserver provides the standard HTTP server middleware, error
// responses and server lifecycle for services built on net/http.
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("GET /orders/{id}", getOrder)
//
//	srv := httpserver.NewServer(cfg.HTTP, httpserver.Handler(mux), logger)
//	if err := httpserver.Serve(shutdown, srv); err != nil {
//		return err // e.g. address already in use
//	}
//
// Middleware are plain func(http.Handler) http.Handler values, so they work
// with any router. Echo services use the echoadapter package.
//
// # Middleware and order
//
// [Handler] applies the standard chain. From outermost to innermost:
//
//	RequestID  accept a valid X-Request-ID or generate one; echo it back
//	Observe    trace span, route, metrics and one access-log line
//	Recover    turn panics into 500 responses
//	MaxBytes   reject request bodies above the limit with 413
//	Timeout    put a deadline on the request context
//
// Each middleware is also exported for custom chains. The only ordering
// requirements are: RequestID outside Observe (so access logs carry the
// request ID), and Observe outside Recover (so a panic is logged and
// measured as a 500). Authentication ([Auth]) is per route: wrap the
// handlers that need it.
//
// # Routes
//
// Metrics, spans and logs use the route template ("/orders/{id}"), never
// the raw path, which would leak identifiers and explode metric
// cardinality. When Handler wraps an *http.ServeMux it resolves the
// template itself. With other routers pass [WithRoute]. Unresolved
// requests use "unmatched".
//
// # Errors
//
// Handlers report failures with [WriteError], which maps the error's kind
// (see the errors package) to a status code with [StatusFor] and writes an
// RFC 9457 problem document containing only [errors.PublicMessage]. The
// full error is attached to the access-log line, not to the response.
//
// # What is never logged
//
// Query strings, headers, cookies and bodies. The access log records
// method, route, status, duration, response size and the error, if any.
package httpserver
