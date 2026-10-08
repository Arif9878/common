package echoadapter_test

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/observability/logging"
	"github.com/Arif9878/common/go/transport/http/echoadapter"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func newEcho(t *testing.T, logs *bytes.Buffer) *echo.Echo {
	t.Helper()
	logger, err := logging.New(logging.Config{}, logging.WithWriter(logs))
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.HTTPErrorHandler = echoadapter.ErrorHandler
	e.Use(echoadapter.Middleware(httpserver.WithLogger(logger)))

	e.GET("/orders/:id", func(c echo.Context) error {
		switch c.Param("id") {
		case "missing":
			return errors.NotFound.Wrap(stderrors.New("no rows"), "load order")
		case "bad":
			return echo.NewHTTPError(http.StatusBadRequest, "id must be numeric")
		case "boom":
			panic("nil pointer")
		}
		return c.JSON(http.StatusOK, map[string]string{"id": c.Param("id")})
	})
	return e
}

func serve(t *testing.T, e *echo.Echo, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

func accessLogs(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m["msg"] == "http request" {
			out = append(out, m)
		}
	}
	return out
}

func TestMiddleware(t *testing.T) {
	var logs bytes.Buffer
	e := newEcho(t, &logs)

	if rec := serve(t, e, "/orders/o-42"); rec.Code != 200 || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("ok: %d %v", rec.Code, rec.Header())
	}
	rec := serve(t, e, "/orders/missing")
	var p httpserver.Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 404 || p.Code != "not_found" || strings.Contains(rec.Body.String(), "no rows") {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body)
	}
	rec = serve(t, e, "/orders/bad")
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 400 || p.Detail != "id must be numeric" || p.Code != "invalid_argument" {
		t.Fatalf("bad: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(t, e, "/orders/boom"); rec.Code != 500 {
		t.Fatalf("panic: %d", rec.Code)
	}
	if rec := serve(t, e, "/nope"); rec.Code != 404 {
		t.Fatalf("unrouted: %d", rec.Code)
	}

	access := accessLogs(t, &logs)
	if len(access) != 5 {
		t.Fatalf("got %d access logs:\n%s", len(access), logs.String())
	}
	wantStatus := []float64{200, 404, 400, 500, 404}
	for i, l := range access[:4] {
		if l["route"] != "/orders/:id" || l["status"] != wantStatus[i] {
			t.Errorf("log %d = %v", i, l)
		}
	}
	if access[1]["error_type"] != "not_found" || access[3]["error_type"] != "internal" {
		t.Errorf("error fields: %v / %v", access[1], access[3])
	}
	if strings.Contains(logs.String(), "o-42") {
		t.Errorf("raw path logged:\n%s", logs.String())
	}
}

func TestErrorHandlerKeepsHTTPErrorStatus(t *testing.T) {
	e := echo.New()
	for _, code := range []int{400, 401, 403, 404, 405, 409, 418, 422, 429, 500, 502, 503, 504} {
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), rec)
		echoadapter.ErrorHandler(echo.NewHTTPError(code, "handler message"), c)

		var p httpserver.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		if rec.Code != code || p.Status != code {
			t.Errorf("status %d (problem %d), want %d", rec.Code, p.Status, code)
		}
		exposed := strings.Contains(rec.Body.String(), "handler message")
		if code < 500 && !exposed || code >= 500 && exposed {
			t.Errorf("%d: message exposed = %v", code, exposed)
		}
	}
}

func TestErrorHandlerSkipsCommittedResponse(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), rec)
	_ = c.String(http.StatusOK, "partial")
	echoadapter.ErrorHandler(errors.Internal.New("late"), c)
	if rec.Code != 200 || rec.Body.String() != "partial" {
		t.Errorf("committed response changed: %d %q", rec.Code, rec.Body)
	}
}
