package orders

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/Arif9878/common/go/errors"
)

// RegisterRoutes adds the API to the Echo server from commonfx.EchoServer,
// which already applies the standard middleware and error responses.
func RegisterRoutes(e *echo.Echo, s *Service) {
	e.POST("/orders", func(c echo.Context) error {
		var req struct {
			CustomerID string `json:"customer_id"`
			Amount     int64  `json:"amount"`
		}
		if err := c.Bind(&req); err != nil {
			return errors.WithPublicMessage(errors.InvalidArgument.Wrap(err, "decode order"), "invalid JSON body")
		}
		o, err := s.Create(c.Request().Context(), req.CustomerID, req.Amount)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, o)
	})
	e.GET("/orders/:id", func(c echo.Context) error {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return errors.WithPublicMessage(errors.InvalidArgument.Wrap(err, "order id"), "the order id must be a number")
		}
		o, err := s.Get(c.Request().Context(), id)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, o)
	})
}
