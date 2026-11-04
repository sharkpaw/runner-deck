package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// Set up an Echo with the metrics middleware, and a route table shaped like the real service (including param routes such as :name).
func metricsApp() *echo.Echo {
	e := echo.New()
	e.Use(metricsMiddleware())
	e.GET("/api/runners", func(c echo.Context) error { return c.String(http.StatusOK, "list") })
	e.GET("/api/runners/:name", func(c echo.Context) error { return c.String(http.StatusOK, "one") })
	e.GET("/boom", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusBadGateway, "炸了")
	})
	e.GET("/health", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	e.GET("/ready", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	e.GET(metricsPath, metricsHandler())
	return e
}

func get(e *echo.Echo, target string) int {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code
}

// Scrape /metrics once and return the text.
func scrape(t *testing.T, e *echo.Echo) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d, expected 200", rec.Code)
	}
	return rec.Body.String()
}

// counterValue **sums** the samples that match all the labels.
//
// It must sum rather than take the first match: httpMetrics is a package-level variable, and all tests in the same process share one copy of it
// registry. Other tests (the ones in csrf_test and wiring_test) also hit /api/runners/:name,
// just with different status codes. If we only took the first matching line, we would read counts left behind by someone else.
func counterValue(t *testing.T, body, metric string, labels map[string]string) float64 {
	t.Helper()
	sum := 0.0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		ok := true
		for k, v := range labels {
			if !strings.Contains(line, k+`="`+v+`"`) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("cannot parse this line: %s", line)
		}
		sum += v
	}
	return sum
}

// The most basic case: after the request completes, the matching sample really shows up on /metrics.
//
// This test scrapes /metrics instead of reading the registry directly. That also proves
// metricsHandler uses HandlerFor(httpMetrics.Registry), not Handler().
// NewHTTPMetrics builds its own registry when Registry is nil, while Handler() serves the
// **default** registry. If this were written with Handler(), the scrape would return a pile of Go runtime metrics,
// but none of the ones we record.
func TestMetrics_RecordedSamplesAreScrapable(t *testing.T) {
	e := metricsApp()
	before := counterValue(t, scrape(t, e), "runner_deck_http_requests_total",
		map[string]string{"method": "GET", "path": "/api/runners", "status": "200"})

	if code := get(e, "/api/runners"); code != http.StatusOK {
		t.Fatalf("status code %d", code)
	}

	after := counterValue(t, scrape(t, e), "runner_deck_http_requests_total",
		map[string]string{"method": "GET", "path": "/api/runners", "status": "200"})
	if after == 0 {
		t.Fatalf("could not find runner_deck_http_requests_total, /metrics content:\n%s", scrape(t, e))
	}
	if after != before+1 {
		t.Errorf("count %v -> %v, expected +1", before, after)
	}
}

// Cardinality control, the most important check in this PR.
//
// The path label uses Echo's **route template**, not the request URL. Three different runner names
// must land on the same time series. Otherwise every runner created adds another series, and Prometheus would be overwhelmed.
func TestMetrics_PathLabelIsRouteTemplateNotRequestURL(t *testing.T) {
	e := metricsApp()
	tmpl := map[string]string{"path": "/api/runners/:name"}
	before := counterValue(t, scrape(t, e), "runner_deck_http_requests_total", tmpl)

	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if code := get(e, "/api/runners/"+name); code != http.StatusOK {
			t.Fatalf("%s status code %d", name, code)
		}
	}
	body := scrape(t, e)

	if got := counterValue(t, body, "runner_deck_http_requests_total", tmpl) - before; got != 3 {
		t.Errorf("three different runner names should collapse onto the :name template, a net increase of 3, got %v", got)
	}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if strings.Contains(body, `path="/api/runners/`+name+`"`) {
			t.Errorf("the specific runner name %q became a label value, which will blow up cardinality:\n%s", name, body)
		}
	}
}

// 404 is the same, and more dangerous: anyone who scans random URLs can create series.
func TestMetrics_UnmatchedRoutesCollapseToOneLabel(t *testing.T) {
	e := metricsApp()
	nf := map[string]string{"path": notFoundPathLabel}
	before := counterValue(t, scrape(t, e), "runner_deck_http_requests_total", nf)

	for _, p := range []string{"/nope-1", "/nope-2", "/admin/../etc/passwd"} {
		get(e, p)
	}
	body := scrape(t, e)

	if got := counterValue(t, body, "runner_deck_http_requests_total", nf) - before; got != 3 {
		t.Errorf("unmatched routes should collapse into %q, a net increase of 3, got %v", notFoundPathLabel, got)
	}
	for _, p := range []string{"nope-1", "nope-2", "passwd"} {
		if strings.Contains(body, p) {
			t.Errorf("unmatched URL fragment %q leaked into a label:\n%s", p, body)
		}
	}
}

// Error status codes must be recorded as their real status codes.
//
// Echo's HTTPErrorHandler writes the response **after** the handler chain returns,
// at which point c.Response().Status is still 200. Reading it directly would record every failed request as a success,
// so alerts based on status=~"5.." would never fire. The middleware therefore takes the code from the returned error.
func TestMetrics_ErrorStatusIsRecordedNotDefaultedTo200(t *testing.T) {
	e := metricsApp()
	if code := get(e, "/boom"); code != http.StatusBadGateway {
		t.Fatalf("the client saw status code %d, expected 502", code)
	}
	body := scrape(t, e)

	if v := counterValue(t, body, "runner_deck_http_requests_total",
		map[string]string{"path": "/boom", "status": "502"}); v < 1 {
		t.Errorf("502 was not recorded:\n%s", body)
	}
	if v := counterValue(t, body, "runner_deck_http_requests_total",
		map[string]string{"path": "/boom", "status": "200"}); v > 0 {
		t.Errorf("failed requests were recorded as 200, which would keep 5xx alerts from ever firing")
	}
}

// Probes and /metrics itself are not recorded: probes run every few seconds, and recording /metrics would be pointless.
func TestMetrics_SkipsProbesAndItself(t *testing.T) {
	e := metricsApp()
	for i := 0; i < 3; i++ {
		get(e, "/health")
		get(e, "/ready")
		get(e, metricsPath)
	}
	body := scrape(t, e)
	for _, p := range []string{"/health", "/ready", metricsPath} {
		if v := counterValue(t, body, "runner_deck_http_requests_total",
			map[string]string{"path": p}); v != 0 {
			t.Errorf("%s should not be recorded, but it has %v hits", p, v)
		}
	}
}

// The latency histogram must also have actual observations, otherwise there is a count but no distribution.
func TestMetrics_DurationHistogramIsObserved(t *testing.T) {
	e := metricsApp()
	get(e, "/api/runners")
	body := scrape(t, e)

	if !regexp.MustCompile(`runner_deck_http_request_duration_seconds_count\{[^}]*path="/api/runners"[^}]*\}`).MatchString(body) {
		t.Errorf("samples without a latency histogram:\n%s", body)
	}
}

// /metrics is not in Skipper, so it also requires credentials once Basic Auth is configured.
// Metrics expose call volume and latency for every endpoint, so there is no reason for them to be more public than /api.
func TestMetrics_EndpointIsBehindBasicAuthWhenConfigured(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "s3cret")
	t.Setenv("BASIC_AUTH_USER", "")

	e := echo.New()
	if mw, _ := basicAuthMiddleware(); mw != nil {
		e.Use(mw)
	} else {
		t.Fatal("Password was set but the middleware was not installed")
	}
	e.GET(metricsPath, metricsHandler())

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/metrics without credentials should return 401, got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, metricsPath, nil)
	req.SetBasicAuth("admin", "s3cret")
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Errorf("/metrics with correct credentials should return 200, got %d", rec2.Code)
	}
}
