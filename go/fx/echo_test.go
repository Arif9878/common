package commonfx_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/Arif9878/common/go/errors"
	commonfx "github.com/Arif9878/common/go/fx"
	"github.com/Arif9878/common/go/requestid"
	"github.com/Arif9878/common/go/testkit"
	"github.com/Arif9878/common/go/transport/http/httpserver"
)

func TestEchoServer(t *testing.T) {
	addr := testkit.FreeAddr(t)
	logger, logs := testkit.NewLogger(t)
	mp, metrics := testkit.NewMetrics(t)
	app := fxtest.New(t,
		fx.Supply(httpserver.Config{Addr: addr}, logger),
		fx.Provide(func() metric.MeterProvider { return mp }),
		commonfx.Lifecycle(),
		commonfx.EchoServer(),
		fx.Invoke(func(e *echo.Echo) {
			e.GET("/orders/:id", func(c echo.Context) error {
				if c.Param("id") == "404" {
					return errors.NotFound.New("order not found")
				}
				return c.JSON(http.StatusOK, map[string]string{"id": c.Param("id")})
			})
			e.GET("/panic", func(echo.Context) error { panic("boom") })
		}),
		commonfx.Ready(),
	)
	app.RequireStart()

	do := func(path string) (int, http.Header) {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	if code, h := do("/orders/7"); code != http.StatusOK || h.Get(requestid.Header) == "" {
		t.Errorf("/orders/7: %d, request ID %q", code, h.Get(requestid.Header))
	}
	if code, h := do("/orders/404"); code != http.StatusNotFound ||
		!strings.HasPrefix(h.Get("Content-Type"), "application/problem+json") {
		t.Errorf("/orders/404: %d %s, want a 404 problem+json", code, h.Get("Content-Type"))
	}
	if code, _ := do("/panic"); code != http.StatusInternalServerError {
		t.Errorf("/panic: %d, want 500", code)
	}

	// One access log and one measurement per request: the middleware is
	// not applied twice.
	if n := len(logs.Messages("http request")); n != 3 {
		t.Errorf("%d access logs for 3 requests:\n%s", n, logs)
	}
	if n := metrics.HistogramCount("http.server.request.duration", attribute.String("http.route", "/orders/:id")); n != 2 {
		t.Errorf("requests measured on route /orders/:id = %d, want 2", n)
	}

	app.RequireStop()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/orders/7", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Error("server still serving after stop")
	}
}
