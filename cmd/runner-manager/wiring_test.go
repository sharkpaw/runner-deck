package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// ---- listenAddr ----

func TestListenAddr(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"未配置端口", &config.Config{}, ":8080"},
		{"端口为负", &config.Config{Server: config.ServerConfig{Port: -1}}, ":8080"},
		{"仅端口", &config.Config{Server: config.ServerConfig{Port: 9000}}, ":9000"},
		{"地址加端口", &config.Config{Server: config.ServerConfig{Addr: "127.0.0.1", Port: 9000}}, "127.0.0.1:9000"},
		{"配置为空", nil, ":8080"},
		// When only addr is set without a port, it still falls back to :8080; addr alone is not a valid listen address
		{"仅地址", &config.Config{Server: config.ServerConfig{Addr: "127.0.0.1"}}, ":8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := listenAddr(tc.cfg); got != tc.want {
				t.Fatalf("listenAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- httpErrorHandler ----

func TestHTTPErrorHandler(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"普通错误按 500", errors.New("boom"), http.StatusInternalServerError, "boom"},
		{"HTTPError 保留状态码与文案",
			echo.NewHTTPError(http.StatusNotFound, "未找到该 runner"), http.StatusNotFound, "未找到该 runner"},
		{"HTTPError 的 Message 不是字符串时回落到 Error()",
			echo.NewHTTPError(http.StatusBadRequest, map[string]string{"a": "b"}), http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
			httpErrorHandler(tc.err, c)
			if rec.Code != tc.wantCode {
				t.Fatalf("status code %d, want %d", rec.Code, tc.wantCode)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %s", rec.Body.String())
			}
			if _, ok := body["message"]; !ok {
				t.Fatalf("response should have a message field: %s", rec.Body.String())
			}
			if tc.wantMsg != "" && body["message"] != tc.wantMsg {
				t.Fatalf("message = %q, want %q", body["message"], tc.wantMsg)
			}
		})
	}
}

// ---- Basic Auth ----

// serveWithAuth mounts the auth middleware (if any) and runs one request
func serveWithAuth(t *testing.T, path, user, pass string, withCreds bool) int {
	t.Helper()
	e := echo.New()
	if mw, _ := basicAuthMiddleware(); mw != nil {
		e.Use(mw)
	}
	e.GET("/health", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	e.GET("/ready", func(c echo.Context) error { return c.String(http.StatusOK, "ready") })
	e.GET("/api/runners", func(c echo.Context) error { return c.String(http.StatusOK, "list") })
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if withCreds {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

// Without BASIC_AUTH_PASSWORD the whole middleware is not mounted, and all endpoints are accessible without credentials.
// This zero-config default is intentional, not an oversight; the case documents it,
// If it is later changed to 'refuse to start without a password', this is the case that fails, which makes the intent clear.
func TestBasicAuthMiddleware_NoPasswordMeansNoAuthAtAll(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "")
	t.Setenv("BASIC_AUTH_USER", "")
	if mw, user := basicAuthMiddleware(); mw != nil || user != "" {
		t.Fatalf("middleware should not be constructed without a password, got mw=%v user=%q", mw != nil, user)
	}
	if code := serveWithAuth(t, "/api/runners", "", "", false); code != http.StatusOK {
		t.Fatalf("endpoints should be accessible without credentials when no password is set, status code %d", code)
	}
}

func TestBasicAuthMiddleware_RejectsWrongCredentials(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "s3cret")
	t.Setenv("BASIC_AUTH_USER", "")
	cases := []struct {
		name      string
		user      string
		pass      string
		withCreds bool
		want      int
	}{
		{"不带凭据", "", "", false, http.StatusUnauthorized},
		{"密码错", "admin", "wrong", true, http.StatusUnauthorized},
		{"用户名错", "root", "s3cret", true, http.StatusUnauthorized},
		{"都对", "admin", "s3cret", true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := serveWithAuth(t, "/api/runners", tc.user, tc.pass, tc.withCreds); code != tc.want {
				t.Fatalf("status code %d, want %d", code, tc.want)
			}
		})
	}
}

// Username defaults to admin; if set, the configured value is used
func TestBasicAuthMiddleware_UserDefaultsToAdmin(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "s3cret")
	t.Setenv("BASIC_AUTH_USER", "")
	if _, user := basicAuthMiddleware(); user != "admin" {
		t.Fatalf("default username should be admin, got %q", user)
	}
	t.Setenv("BASIC_AUTH_USER", "  ops  ") // Leading and trailing whitespace must be trimmed, otherwise nobody can log in
	if _, user := basicAuthMiddleware(); user != "ops" {
		t.Fatalf("username should be ops, got %q", user)
	}
	if code := serveWithAuth(t, "/api/runners", "ops", "s3cret", true); code != http.StatusOK {
		t.Fatalf("custom username should pass, status code %d", code)
	}
}

// /health and /ready are for Ingress / K8s probes and must skip auth; no other paths skip it.
// Probe configs usually cannot carry Basic Auth credentials; missing the exemption means 'all probes fail once auth is enabled'.
func TestBasicAuthMiddleware_ProbesAreAlwaysOpen(t *testing.T) {
	t.Setenv("BASIC_AUTH_PASSWORD", "s3cret")
	t.Setenv("BASIC_AUTH_USER", "")
	for _, path := range []string{"/health", "/ready"} {
		if code := serveWithAuth(t, path, "", "", false); code != http.StatusOK {
			t.Errorf("%s should skip auth, status code %d", path, code)
		}
	}
	if code := serveWithAuth(t, "/api/runners", "", "", false); code != http.StatusUnauthorized {
		t.Fatalf("all endpoints except probes should require auth, status code %d", code)
	}
}

// ---- route table ----

func TestRegisterRoutes_AllEndpointsPresent(t *testing.T) {
	e := echo.New()
	registerRoutes(e)
	got := map[string]bool{}
	for _, r := range e.Routes() {
		got[r.Method+" "+r.Path] = true
	}
	want := []string{
		"GET /health",
		"GET /ready",
		"GET /version",
		"GET /metrics",
		"GET /",
		"GET /login",
		"POST /login",
		"POST /logout",
		"GET /static/*",
		"HEAD /static/*",
		"GET /api/runners",
		"GET /api/runners/:name",
		"POST /api/runners",
		"GET /api/runner-precheck",
		"GET /api/runner-rows",
		"GET /api/system",
		"PUT /api/settings/resources",
		"PUT /api/runners/:name",
		"DELETE /api/runners/:name",
		"POST /api/runners/:name/start",
		"POST /api/runners/:name/stop",
		"POST /api/runners/:name/recreate",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing route: %s", w)
		}
	}
	if len(e.Routes()) != len(want) {
		t.Errorf("route count is %d, want %d; when adding a route, update this case too (and check whether it needs auth)", len(e.Routes()), len(want))
	}
}

// A Runner named check must not steal /api/runner-precheck——
// This is exactly why precheck sits outside /api/runners/:name
func TestRegisterRoutes_PrecheckNotShadowedByRunnerName(t *testing.T) {
	e := echo.New()
	registerRoutes(e)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runner-precheck?name=x", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("precheck route should match, got 404: %s", rec.Body.String())
	}
}
