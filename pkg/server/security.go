package server

import (
	"mime"
	"net/http"

	"github.com/labstack/echo/v4"
)

func sameOrigin() echo.MiddlewareFunc {
	var protection http.CrossOriginProtection
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// GET can initialize tool processes, so even safe methods need checking.
			req := *c.Request()
			req.Method = http.MethodPost
			if err := protection.Check(&req); err != nil {
				return echo.NewHTTPError(http.StatusForbidden, err.Error())
			}
			return next(c)
		}
	}
}

func requireJSON(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		mediaType, params, err := mime.ParseMediaType(c.Request().Header.Get(echo.HeaderContentType))
		if err != nil || mediaType != echo.MIMEApplicationJSON {
			return echo.NewHTTPError(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		}
		// Echo's binder matches media types case-sensitively.
		c.Request().Header.Set(echo.HeaderContentType, mime.FormatMediaType(mediaType, params))
		return next(c)
	}
}
