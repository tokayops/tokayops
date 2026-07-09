package metrics

import (
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

// EchoMiddleware returns an Echo middleware that records HTTP metrics.
func EchoMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()

			err := next(c)

			status := c.Response().Status
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok {
					status = he.Code
				}
			}

			path := c.Path() // route pattern, e.g. /api/v1/teams/:id
			if path == "" {
				path = "unknown"
			}
			method := c.Request().Method
			statusStr := strconv.Itoa(status)

			HTTPRequestsTotal.WithLabelValues(method, path, statusStr).Inc()
			HTTPRequestDuration.WithLabelValues(method, path).Observe(time.Since(start).Seconds())

			return err
		}
	}
}
