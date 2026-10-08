package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-oauth2/oauth2/v4/generates"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	uuid "github.com/satori/go.uuid"
)

// ValidateBearerToken verifies an HMAC-signed JWT access token from the request
// using key. On success the parsed *jwt.Token (github.com/golang-jwt/jwt/v5)
// is stored in the echo context under "token".
//
// It panics if key is empty.
func ValidateBearerToken(key []byte) echo.MiddlewareFunc {
	if len(key) == 0 {
		panic("middleware: ValidateBearerToken requires a non-empty signing key")
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Parse and verify jwt access token
			auth, ok := bearerAuth(c.Request())
			if !ok {
				return echo.NewHTTPError(http.StatusUnauthorized, errors.New("parse jwt access token error"))
			}
			token, err := jwt.ParseWithClaims(auth, &generates.JWTAccessClaims{}, func(t *jwt.Token) (interface{}, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, echo.NewHTTPError(http.StatusUnauthorized, errors.New("parse signing method error"))
				}
				return key, nil
			})
			if err != nil {
				return echo.NewHTTPError(http.StatusUnauthorized, err)
			}

			c.Set("token", token)
			return next(c)
		}
	}
}

func CorrelationIdMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {

		req := c.Request()

		id := req.Header.Get(echo.HeaderXCorrelationID)
		if id == "" {
			id = uuid.NewV4().String()
		}

		c.Response().Header().Set(echo.HeaderXCorrelationID, id)
		newReq := req.WithContext(context.WithValue(req.Context(), echo.HeaderXCorrelationID, id))
		c.SetRequest(newReq)

		return next(c)
	}
}

// BearerAuth parse bearer token
func bearerAuth(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	prefix := "Bearer "
	token := ""

	if auth != "" && strings.HasPrefix(auth, prefix) {
		token = auth[len(prefix):]
	} else {
		token = r.FormValue("access_token")
	}
	return token, token != ""
}
