package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
)

func init() {
	dir := os.TempDir()
	ConfigPath = filepath.Join(dir, "handler-test-config.yaml")
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{BasePath: dir, Items: []config.RunnerItem{}},
	}
	_ = cfg.Save(ConfigPath)
}

// /health is still the liveness probe: it returns 200 whenever the process is alive and checks no dependencies.
// status is still "ok" (StatusHealthy in health-kit is this same string),
// It only adds a service field, so checks on status == "ok" remain compatible.
func TestHealth(t *testing.T) {
	e := echo.New()
	e.GET("/health", Health)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, expected ok (load balancers and monitoring may read this value)", m["status"])
	}
	if m["service"] != "runner-deck" {
		t.Errorf("service = %v, expected runner-deck", m["service"])
	}
	// This endpoint is always unauthenticated, so it must not expose per-item check results
	if _, ok := m["checks"]; ok {
		t.Errorf("/health should not contain checks: %v", m)
	}
}

func TestVersionInfo(t *testing.T) {
	Version = "test-1.0"
	defer func() { Version = "" }()
	e := echo.New()
	e.GET("/version", VersionInfo)
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	var m map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != "test-1.0" {
		t.Errorf("version = %q", m["version"])
	}
}

func TestVersionInfo_EmptyDefaultsToDev(t *testing.T) {
	Version = ""
	defer func() { Version = "" }()
	e := echo.New()
	e.GET("/version", VersionInfo)
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	var m map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != "dev" {
		t.Errorf("version = %q, want dev", m["version"])
	}
}

func TestAddRunner_InvalidName(t *testing.T) {
	e := echo.New()
	e.POST("/api/runners", AddRunner)
	body := map[string]string{
		"name":        "bad..name",
		"target_type": "org",
		"target":      "myorg",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/runners", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid name, got %d", rec.Code)
	}
}

func TestUpdateRunner_NameImmutable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Runners: config.RunnersConfig{
			BasePath: dir,
			Items:    []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}},
		},
	}
	_ = cfg.Save(cfgPath)
	ConfigPath = cfgPath
	defer func() { ConfigPath = filepath.Join(os.TempDir(), "handler-test-config.yaml") }()

	e := echo.New()
	e.PUT("/api/runners/:name", UpdateRunner)
	body := map[string]any{"name": "r2", "target_type": "org", "target": "o1"}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/runners/r1", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when body name differs from URL, got %d", rec.Code)
	}
}

func TestAddRunner_WhitespaceName(t *testing.T) {
	e := echo.New()
	e.POST("/api/runners", AddRunner)
	body := map[string]string{
		"name":        "   ",
		"target_type": "org",
		"target":      "myorg",
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/runners", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for whitespace name, got %d", rec.Code)
	}
}

func TestUpdateRunner_TrimmedBodyNameAccepted(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Runners: config.RunnersConfig{
			BasePath: dir,
			Items:    []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}},
		},
	}
	_ = cfg.Save(cfgPath)
	ConfigPath = cfgPath
	defer func() { ConfigPath = filepath.Join(os.TempDir(), "handler-test-config.yaml") }()

	e := echo.New()
	e.PUT("/api/runners/:name", UpdateRunner)
	body := map[string]any{"name": "  r1  ", "target_type": "org", "target": "o1"}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/runners/r1", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when body name trims to URL name, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStartRunner_ProbeFailureFallsBackToInstalledStatus(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	installDir := filepath.Join(dir, "r1")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Mark as registered
	if err := os.WriteFile(filepath.Join(installDir, ".runner"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Runners: config.RunnersConfig{
			BasePath:      dir,
			ContainerMode: true,
			Items: []config.RunnerItem{
				{Name: "r1", TargetType: "org", Target: "o1"},
			},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	oldConfigPath := ConfigPath
	ConfigPath = cfgPath
	defer func() { ConfigPath = oldConfigPath }()

	// Make the container status probe fail (docker command is not executable)
	t.Setenv("PATH", "")
	// The start path takes the container-mode fast-fail branch; reaching here means no early 400 rejection happened
	t.Setenv("DOCKER_HOST", "tcp://runner-dind:2375")

	e := echo.New()
	e.POST("/api/runners/:name/start", StartRunner)
	req := httptest.NewRequest(http.MethodPost, "/api/runners/r1/start", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when start is attempted after probe failure, got %d body=%s", rec.Code, rec.Body.String())
	}
}

type ctxKey string

// TestLifecycleContext_KeepsRunningAfterRequestCancel guards the rule that start/stop does not follow request cancellation.
//
// Regression background: after clicking "Start" in the UI, if the page refreshes (a successful add-runner triggers an auto-refresh after 5 seconds,
// and the message box's close button also reloads), the browser cancels in-flight requests. If the start/stop context is derived from
// c.Request().Context(), the cancellation propagates through exec.CommandContext into a SIGKILL to the docker subprocess,
// so docker create is killed midway, leaving only "output: (no output): signal: killed",
// while the container may already exist on the daemon side, so the next start hits a name conflict.
func TestLifecycleContext_KeepsRunningAfterRequestCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey("trace"), "abc"))
	ctx, done := lifecycleContext(parent, 30*time.Second)
	defer done()

	cancel() // Simulate the request being interrupted by a browser refresh

	select {
	case <-ctx.Done():
		t.Fatalf("start/stop context was cancelled along with the request: %v", ctx.Err())
	case <-time.After(50 * time.Millisecond):
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("start/stop context must have a timeout, otherwise it is held forever when docker hangs")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("timeout is not as expected: %v", remaining)
	}
	if v, _ := ctx.Value(ctxKey("trace")).(string); v != "abc" {
		t.Fatalf("value in the request context should be preserved, got %q", v)
	}
}

// TestLifecycleContext_TimesOut checks that the timeout still applies: it does not wait forever when docker hangs.
func TestLifecycleContext_TimesOut(t *testing.T) {
	ctx, done := lifecycleContext(context.Background(), 10*time.Millisecond)
	defer done()

	select {
	case <-ctx.Done():
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("err = %v, want DeadlineExceeded", ctx.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout did not take effect")
	}
}

// TestRecreateRunner_RejectsNonContainerMode: in non-container mode there is no runner container, so recreation is meaningless
func TestRecreateRunner_RejectsNonContainerMode(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	installDir := filepath.Join(dir, "r1")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".runner"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{
			BasePath: dir,
			Items:    []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	oldConfigPath := ConfigPath
	ConfigPath = cfgPath
	defer func() { ConfigPath = oldConfigPath }()

	e := echo.New()
	e.POST("/api/runners/:name/recreate", RecreateRunner)
	req := httptest.NewRequest(http.MethodPost, "/api/runners/r1/recreate", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

// TestRecreateRunner_RejectsUnregistered: an unregistered runner has no container, so do not try to delete it
func TestRecreateRunner_RejectsUnregistered(t *testing.T) {
	// Assert on the i18n key rather than the wording of one language: messages are now translated per request language,
	// asserting the Chinese source text would pin this test to a single language.
	defer withI18n(map[string]string{
		"api.recreate_requires_registered": "ONLY-REGISTERED %s",
	})()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.MkdirAll(filepath.Join(dir, "r1"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{
			BasePath:      dir,
			ContainerMode: true,
			Items:         []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	oldConfigPath := ConfigPath
	ConfigPath = cfgPath
	defer func() { ConfigPath = oldConfigPath }()

	e := echo.New()
	e.POST("/api/runners/:name/recreate", RecreateRunner)
	req := httptest.NewRequest(http.MethodPost, "/api/runners/r1/recreate", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ONLY-REGISTERED") {
		t.Fatalf("should state that only registered runners can be recreated, actual: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "new") {
		t.Fatalf("message should include the current state, actual: %s", rec.Body.String())
	}
}
