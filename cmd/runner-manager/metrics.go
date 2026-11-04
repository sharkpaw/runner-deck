package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	metrics "github.com/soulteary/metrics-kit/v3"
)

// metricsPath is the Prometheus scrape endpoint.
//
// Deliberately **not** added to basicAuthMiddleware's Skipper: this endpoint exposes call volume and
// latency distributions for all endpoints, which is operational data and should not be more public than /api. Prometheus' scrape_config
// supports basic_auth, so configuring it is enough.
const metricsPath = "/metrics"

// httpMetricsConfig is kept separately: HTTPMetrics does not expose its config,
// but the middleware needs its SkipPaths and TransformPath.
var httpMetricsConfig = metrics.HTTPMetricsConfig{
	Namespace: "runner_deck",
	Subsystem: "http",
	SkipPaths: []string{metricsPath, "/health", "/ready"},

	// Echo's c.Path() returns the **route template** (/api/runners/:name), not the request URL,
	// so cardinality is naturally bounded. The kit's note on this option describes exactly this case:
	// "Only safe where every path is already bounded -- a router that reports
	//  its route pattern rather than the request URL, say."
	//
	// Conversely, if the default DefaultPathNormalize were applied to a path that is already a template,
	// it would mean guessing a normalization against :name again, which is pointless.
	DisablePathNormalization: true,
}

// httpMetrics is this process's HTTP metrics.
//
// Its Registry must be kept: NewHTTPMetrics creates its own when cfg.Registry is nil,
// but metrics.Handler() serves the **default** registry. With Handler(), every metric recorded here
// would never be scraped. The kit's own comments cover this, so HandlerFor is used below.
var httpMetrics = metrics.NewHTTPMetrics(httpMetricsConfig)

// metricsHandler serves the /metrics endpoint.
func metricsHandler() echo.HandlerFunc {
	return echo.WrapHandler(metrics.HandlerFor(httpMetrics.Registry))
}

// notFoundPathLabel is the path label used when no route matches.
//
// It must be a fixed string. On 404, Echo's c.Path() is empty; falling back to c.Request().URL.Path,
// anyone scanning random URLs could create a time series per URL and overwhelm Prometheus:
// this is exactly the case the kit names in its PathTransformFunc comment.
const notFoundPathLabel = "<not-found>"

// metricsMiddleware records the call volume and latency of each request.
//
// metrics-kit v3 only ships a Fiber middleware (in the fiberadapter subpackage),
// and the README for the net/http side says to call RecordRequest yourself, so this layer is hand-written.
// Normalization still goes through the kit's TransformPath rather than a separate local implementation.
func metricsMiddleware() echo.MiddlewareFunc {
	skip := make(map[string]bool, len(httpMetricsConfig.SkipPaths))
	for _, p := range httpMetricsConfig.SkipPaths {
		skip[p] = true
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if skip[c.Request().URL.Path] {
				return next(c)
			}
			start := time.Now()
			err := next(c)

			// The status code must be read after next, and HTTPErrorHandler must be considered:
			// it writes the response only after the handler chain returns, so c.Response().Status is not yet updated at that point.
			status := c.Response().Status
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok {
					status = he.Code
				} else {
					status = http.StatusInternalServerError
				}
			}

			httpMetrics.RecordRequest(
				c.Request().Method,
				routeLabel(c),
				strconv.Itoa(status),
				time.Since(start),
			)
			return err
		}
	}
}

// routeLabel is the path label for this request: the route template, or the fixed <not-found> if nothing matched.
func routeLabel(c echo.Context) string {
	p := c.Path()
	if p == "" {
		return notFoundPathLabel
	}
	return httpMetricsConfig.TransformPath(p)
}
