package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
)

func serveProbe(t *testing.T, path string, h echo.HandlerFunc) (int, map[string]any) {
	t.Helper()
	e := echo.New()
	e.GET(path, h)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, m
}

// Write a loadable config and point ConfigPath at it; base is a usable runner root directory.
func withWorkingConfig(t *testing.T) (base string) {
	t.Helper()
	dir := t.TempDir()
	base = filepath.Join(dir, "runners")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{BasePath: base, Items: []config.RunnerItem{}},
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	old := ConfigPath
	ConfigPath = cfgPath
	t.Cleanup(func() { ConfigPath = old })
	return base
}

// /health is the liveness probe: it must keep returning 200 even when dependencies are broken.
//
// This check is not filler. A failing K8s livenessProbe means restarting the container, but problems like a broken config or a dropped volume
// won't get better from a restart; they just turn into a crashloop. So all dependency checks go in /ready,
// and /health only answers "is the process still alive".
func TestHealth_StaysOKEvenWhenDependenciesAreBroken(t *testing.T) {
	old := ConfigPath
	ConfigPath = filepath.Join(t.TempDir(), "不存在的目录", "config.yaml")
	defer func() { ConfigPath = old }()

	code, m := serveProbe(t, "/health", Health)
	if code != http.StatusOK {
		t.Fatalf("/health status code %d; the liveness probe should not turn red because of a dependency failure", code)
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, expected ok", m["status"])
	}
}

func TestReady_OKWhenConfigAndBasePathUsable(t *testing.T) {
	withWorkingConfig(t)
	code, m := serveProbe(t, "/ready", Ready)
	if code != http.StatusOK {
		t.Fatalf("/ready status code %d, expected 200; body=%v", code, m)
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, expected ok", m["status"])
	}
}

// This guards WithCriticalChecks. In health-kit, a failing non-critical check only counts as degraded,
// and degraded is mapped to **200** ("Degraded is still functional") --
// If you forget to mark the critical checks, these two probes become useless and never take the instance out of rotation.
func TestReady_503WhenConfigCannotLoad(t *testing.T) {
	old := ConfigPath
	ConfigPath = filepath.Join(t.TempDir(), "不存在的目录", "config.yaml")
	defer func() { ConfigPath = old }()

	code, m := serveProbe(t, "/ready", Ready)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("when the config cannot be loaded, /ready should be 503, got %d; body=%v", code, m)
	}
	if m["status"] == "ok" {
		t.Errorf("status should not be ok: %v", m)
	}
}

// A dropped mount volume is the case we most want to catch here: the directory still exists (the mount point itself), but writes fail.
func TestReady_503WhenBasePathNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("when running as root, a 0500 directory is still writable, so this cannot be detected")
	}
	base := withWorkingConfig(t)
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(base, 0o755) }()

	code, m := serveProbe(t, "/ready", Ready)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("when the root directory is not writable, /ready should be 503, got %d; body=%v", code, m)
	}
}

func TestReady_503WhenBasePathIsAFile(t *testing.T) {
	base := withWorkingConfig(t)
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("不是目录"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, m := serveProbe(t, "/ready", Ready)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("when the root path is a file, /ready should be 503, got %d; body=%v", code, m)
	}
}

// Both endpoints are always unauthenticated, so neither may expose per-check results or internal paths.
// This matters most on failure: that is when paths and raw error text are most likely to leak.
func TestProbes_NeverLeakInternalDetails(t *testing.T) {
	old := ConfigPath
	secret := filepath.Join(t.TempDir(), "机密路径", "config.yaml")
	ConfigPath = secret
	defer func() { ConfigPath = old }()

	for _, tc := range []struct {
		path string
		h    echo.HandlerFunc
	}{{"/health", Health}, {"/ready", Ready}} {
		_, m := serveProbe(t, tc.path, tc.h)
		for _, k := range []string{"checks", "details", "error"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s leaked the %s field: %v", tc.path, k, m)
			}
		}
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), "机密路径") {
			t.Errorf("%s response contains an internal path: %s", tc.path, raw)
		}
	}
}

// /ready is hit by the probe every few seconds, so both checks only read config and filesystem metadata,
// spawning no subprocesses and making no network requests. We cannot directly assert "no exec", so we fall back to this:
// 50 consecutive calls must all succeed reliably -- if it ran docker exec or dialed the network, this would first slow down
// and then become flaky from timeouts or resource exhaustion.
func TestReady_IsCheapEnoughForFrequentProbing(t *testing.T) {
	withWorkingConfig(t)
	// Run once first, so the initial filesystem cold start is not counted
	if code, _ := serveProbe(t, "/ready", Ready); code != http.StatusOK {
		t.Fatalf("warm-up request failed, status code %d", code)
	}
	for i := 0; i < 50; i++ {
		if code, _ := serveProbe(t, "/ready", Ready); code != http.StatusOK {
			t.Fatalf("/ready failed on attempt %d, status code %d", i, code)
		}
	}
}
