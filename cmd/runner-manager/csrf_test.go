package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// serveWithCSRF wraps csrfGuardMiddleware around a handler that always returns 200, and returns the actual status code.
// Passing nil for headers means the request carries no origin markers (the shape of curl or a script).
func serveWithCSRF(t *testing.T, method, host string, headers map[string]string, trusted []string) int {
	t.Helper()
	e := echo.New()
	e.HTTPErrorHandler = httpErrorHandler
	e.Use(csrfGuardMiddleware(trusted))
	handler := func(c echo.Context) error { return c.NoContent(http.StatusOK) }
	e.Add(method, "/api/runners", handler)
	e.Add(method, "/api/runners/:name/start", handler)

	req := httptest.NewRequest(method, "/api/runners", strings.NewReader(`{"name":"a"}`))
	req.Host = host
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

func TestCSRFGuardSafeMethodsAlwaysPass(t *testing.T) {
	// Read endpoints don't change state, and cross-site reads are already bound by the same-origin policy, so this should not block them
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			got := serveWithCSRF(t, method, "localhost:8080", map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Origin":         "https://evil.example",
			}, nil)
			if got != http.StatusOK {
				t.Fatalf("%s was rejected (%d), safe methods should always be allowed", method, got)
			}
		})
	}
}

func TestCSRFGuardSecFetchSite(t *testing.T) {
	cases := []struct {
		site     string
		wantCode int
	}{
		{"same-origin", http.StatusOK},
		// same-site is another origin on the same registrable domain (a subdomain or different port); the internal admin side never trusts it
		{"same-site", http.StatusForbidden},
		{"cross-site", http.StatusForbidden},
		// none means the user navigated directly (address bar, bookmark); write requests should not originate this way
		{"none", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.site, func(t *testing.T) {
			got := serveWithCSRF(t, http.MethodPost, "localhost:8080",
				map[string]string{"Sec-Fetch-Site": tc.site}, nil)
			if got != tc.wantCode {
				t.Fatalf("Sec-Fetch-Site=%s got %d, expected %d", tc.site, got, tc.wantCode)
			}
		})
	}
}

// Sec-Fetch-Site is more reliable than Origin (computed locally by the browser, unaffected by a reverse proxy rewriting Host),
// so when it is present it decides: even if Origin looks same-origin, reject cross-site.
func TestCSRFGuardSecFetchSiteWinsOverOrigin(t *testing.T) {
	got := serveWithCSRF(t, http.MethodPost, "localhost:8080", map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Origin":         "http://localhost:8080",
	}, nil)
	if got != http.StatusForbidden {
		t.Fatalf("got %d, expected 403: when Sec-Fetch-Site is present it should be authoritative", got)
	}
}

// Older browsers (before Safari 16.4) don't send Sec-Fetch-Site, but cross-site POSTs always carry Origin
func TestCSRFGuardOriginFallback(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		origin   string
		wantCode int
	}{
		{"同源", "localhost:8080", "http://localhost:8080", http.StatusOK},
		{"同源带末尾斜杠", "localhost:8080", "http://localhost:8080/", http.StatusOK},
		{"端口不同即跨源", "localhost:8080", "http://localhost:9090", http.StatusForbidden},
		{"主机不同", "localhost:8080", "https://evil.example", http.StatusForbidden},
		// A subdomain is not same-origin: evil can get a foothold on sub.localhost
		{"子域", "localhost:8080", "http://sub.localhost:8080", http.StatusForbidden},
		{"Origin 无法解析", "localhost:8080", "://not a url", http.StatusForbidden},
		// Browsers send "null" for opaque origins; it should not be treated as same-origin
		{"null 来源", "localhost:8080", "null", http.StatusForbidden},
		// The protocol is not part of the comparison: when a reverse proxy terminates TLS, Host has no scheme to compare
		{"仅协议不同", "localhost:8080", "https://localhost:8080", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serveWithCSRF(t, http.MethodPost, tc.host,
				map[string]string{"Origin": tc.origin}, nil)
			if got != tc.wantCode {
				t.Fatalf("Host=%s Origin=%s got %d, expected %d", tc.host, tc.origin, got, tc.wantCode)
			}
		})
	}
}

// A request with neither Sec-Fetch-Site nor Origin was not sent by a browser: it does not hold the user's
// Basic Auth cache, so it cannot form a CSRF. Blocking it would only make the API unscriptable.
func TestCSRFGuardNonBrowserRequestPasses(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			if got := serveWithCSRF(t, method, "localhost:8080", nil, nil); got != http.StatusOK {
				t.Fatalf("%s without origin markers got %d, expected allow", method, got)
			}
		})
	}
}

func TestCSRFGuardTrustedOriginsEscapeHatch(t *testing.T) {
	trusted := []string{"https://ui.example.com"}
	cases := []struct {
		name     string
		headers  map[string]string
		wantCode int
	}{
		{"白名单来源即便被判跨站也放行",
			map[string]string{"Origin": "https://ui.example.com", "Sec-Fetch-Site": "cross-site"},
			http.StatusOK},
		{"白名单比对忽略末尾斜杠",
			map[string]string{"Origin": "https://ui.example.com/", "Sec-Fetch-Site": "cross-site"},
			http.StatusOK},
		{"不在白名单上的来源照旧拒绝",
			map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
			http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serveWithCSRF(t, http.MethodPost, "localhost:8080", tc.headers, trusted)
			if got != tc.wantCode {
				t.Fatalf("got %d, expected %d", got, tc.wantCode)
			}
		})
	}
}

// The 403 must use the unified error format so the UI can display the reason.
//
// Use newEchoServer instead of hand-building an echo: the rejection reason is now looked up from a table (handler.Trf),
// a hand-built server has no I18nLoader installed, so tr falls back to the key itself, and this case would end up asserting
// "api.csrf_cross_site", a string that tells a reader nothing, and assert against it with no complaint.
func TestCSRFGuardRejectionBodyNamesTheCause(t *testing.T) {
	t.Setenv("TRUSTED_ORIGINS", "")
	e := newEchoServer()

	req := httptest.NewRequest(http.MethodPost, "/api/runners", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status code %d, expected 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"message"`) {
		t.Fatalf("response should be {\"message\": ...}: %s", body)
	}
	if !strings.Contains(body, "TRUSTED_ORIGINS") {
		t.Fatalf("rejection reason should point to the escape hatch TRUSTED_ORIGINS: %s", body)
	}
}

func TestTrustedOrigins(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{"未设置", "", nil},
		{"只有空白", "   ", nil},
		{"单个", "https://a.example", []string{"https://a.example"}},
		{"多个并去掉空白", " https://a.example , https://b.example ",
			[]string{"https://a.example", "https://b.example"}},
		{"去掉末尾斜杠", "https://a.example/", []string{"https://a.example"}},
		{"跳过空项", "https://a.example,,", []string{"https://a.example"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRUSTED_ORIGINS", tc.env)
			got := trustedOrigins()
			if len(got) != len(tc.want) {
				t.Fatalf("trustedOrigins() = %v, expected %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("trustedOrigins() = %v, expected %v", got, tc.want)
				}
			}
		})
	}
}

// ---- Is the middleware actually attached ----

// writeRoutes are all the routes that change state and do not trigger a CORS preflight.
// Preflighted methods (PUT / DELETE) browsers cannot send as cross-site simple requests anyway, but watch them too,
// so nobody changes them to POST later and forgets this layer.
var writeRoutes = []struct {
	method string
	path   string
}{
	{http.MethodPost, "/api/runners"},
	{http.MethodPost, "/api/runners/demo/start"},
	{http.MethodPost, "/api/runners/demo/stop"},
	{http.MethodPost, "/api/runners/demo/recreate"},
	{http.MethodPut, "/api/runners/demo"},
	{http.MethodDelete, "/api/runners/demo"},
}

// newEchoServer mutates package-level globals in handler (other than ConfigPath, I18nLoader),
// it is enough that tests do not interfere with each other; no need to restore.
func serveThroughRealServer(t *testing.T, method, path string, headers map[string]string) int {
	t.Helper()
	e := newEchoServer()
	req := httptest.NewRequest(method, path, strings.NewReader(`{"name":"demo","target_type":"repo","target":"o/r"}`))
	req.Host = "localhost:8080"
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

// This is the thing this layer really has to guard: a correct middleware does not mean it is attached.
func TestCSRFGuardIsMountedOnWriteRoutes(t *testing.T) {
	t.Setenv("TRUSTED_ORIGINS", "")
	for _, r := range writeRoutes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			got := serveThroughRealServer(t, r.method, r.path, map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Origin":         "https://evil.example",
			})
			if got != http.StatusForbidden {
				t.Fatalf("%s %s cross-site call returned %d, expected 403 — CSRF middleware did not block this route",
					r.method, r.path, got)
			}
		})
	}
}

// Conversely: same-origin calls should not be blocked by this layer. Past the handler it will likely fail without real config,
// that is a different matter; here we only assert "not blocked by CSRF".
func TestCSRFGuardLetsSameOriginThrough(t *testing.T) {
	t.Setenv("TRUSTED_ORIGINS", "")
	for _, r := range writeRoutes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			got := serveThroughRealServer(t, r.method, r.path, map[string]string{
				"Sec-Fetch-Site": "same-origin",
				"Origin":         "http://localhost:8080",
			})
			if got == http.StatusForbidden {
				t.Fatalf("%s %s same-origin call was blocked with 403", r.method, r.path)
			}
		})
	}
}

// Read endpoints are unaffected, /health must also stay reachable for probes
func TestReadRoutesUnaffectedByCSRFGuard(t *testing.T) {
	t.Setenv("TRUSTED_ORIGINS", "")
	for _, path := range []string{"/health", "/ready", "/version"} {
		t.Run(path, func(t *testing.T) {
			got := serveThroughRealServer(t, http.MethodGet, path, map[string]string{
				"Sec-Fetch-Site": "cross-site",
			})
			if got != http.StatusOK {
				t.Fatalf("GET %s returned %d, expected 200", path, got)
			}
		})
	}
}
