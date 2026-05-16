// Package echolog provides Echo middleware tailored for docker-agent's
// HTTP servers. It currently exposes a single helper, RedactedRequestLogger,
// that mirrors the default echo middleware.RequestLogger() but never
// records request bodies, raw query strings, or other places that may
// carry secrets.
package echolog

import (
	"log/slog"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// RedactedRequestLogger returns an Echo request-logging middleware that
// matches middleware.RequestLogger() in spirit (one structured slog line
// per request) but logs the URL path only, not the full request URI.
//
// Echo's default logger emits v.URI, which includes the query string.
// Some clients pass authentication material through query parameters,
// which would otherwise be persisted to whichever sink slog points at.
// We therefore log v.URIPath (req.URL.Path) and explicitly drop the
// query.
func RedactedRequestLogger() echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogLatency:       true,
		LogRemoteIP:      true,
		LogHost:          true,
		LogMethod:        true,
		LogURIPath:       true,
		LogRequestID:     true,
		LogUserAgent:     true,
		LogStatus:        true,
		LogError:         true,
		LogContentLength: true,
		LogResponseSize:  true,
		HandleError:      true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			msg := "REQUEST"
			attrs := []slog.Attr{
				slog.String("method", v.Method),
				slog.String("path", v.URIPath),
				slog.Int("status", v.Status),
				slog.Duration("latency", v.Latency),
				slog.String("host", v.Host),
				slog.String("bytes_in", v.ContentLength),
				slog.Int64("bytes_out", v.ResponseSize),
				slog.String("user_agent", v.UserAgent),
				slog.String("remote_ip", v.RemoteIP),
				slog.String("request_id", v.RequestID),
			}
			if v.Error != nil {
				level = slog.LevelError
				msg = "REQUEST_ERROR"
				attrs = append(attrs, slog.String("error", v.Error.Error()))
			}
			slog.LogAttrs(c.Request().Context(), level, msg, attrs...)
			return nil
		},
	})
}
