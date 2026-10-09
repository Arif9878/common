package echoadapter_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/labstack/echo/v4"

	"github.com/Arif9878/common/go/errors"
	"github.com/Arif9878/common/go/transport/http/echoadapter"
)

func Example() {
	e := echo.New()
	e.HTTPErrorHandler = echoadapter.ErrorHandler // problem+json from error kinds
	e.Use(echoadapter.Middleware())               // request ID, tracing, metrics, access log, recovery
	e.GET("/orders/:id", func(c echo.Context) error {
		return errors.NotFound.New("order not found")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/orders/42", nil)
	e.ServeHTTP(rec, req)
	fmt.Println(rec.Code, rec.Header().Get("Content-Type"))
	// Output: 404 application/problem+json
}
