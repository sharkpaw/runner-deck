package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

// withConfig writes a config and points ConfigPath at it, returning the base directory
func withConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := t.TempDir()
	cfg.Runners.BasePath = dir
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	old := ConfigPath
	ConfigPath = cfgPath
	t.Cleanup(func() { ConfigPath = old })
	return dir
}

func serve(t *testing.T, method, path string, register func(*echo.Echo)) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	register(e)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func stopRoute(e *echo.Echo) { e.POST("/api/runners/:name/stop", StopRunner) }

// ---- StopRunner: input and lookup ----

func TestStopRunnerRejectsUnsafeName(t *testing.T) {
	withConfig(t, &config.Config{})
	// The name is spliced into the install directory path; letting ".." through would allow stopping processes in arbitrary directories
	for _, name := range []string{"..", "a..b", "a%2Fb"} {
		t.Run(name, func(t *testing.T) {
			rec := serve(t, http.MethodPost, "/api/runners/"+name+"/stop", stopRoute)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
				t.Fatalf("got %d; an illegal name should be rejected with 400 or 404", rec.Code)
			}
		})
	}
}

func TestStopRunnerUnknownRunnerIs404(t *testing.T) {
	withConfig(t, &config.Config{
		Runners: config.RunnersConfig{Items: []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}}},
	})
	rec := serve(t, http.MethodPost, "/api/runners/nope/stop", stopRoute)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, expected 404", rec.Code)
	}
}

// If it isn't running there's nothing to stop, and it shouldn't error either. Clicking twice in the UI is normal
func TestStopRunnerIdleRunnerSucceedsWithoutSignalling(t *testing.T) {
	defer withI18n(map[string]string{"api.not_running": "NOT-RUNNING"})()
	base := withConfig(t, &config.Config{
		Runners: config.RunnersConfig{Items: []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}}},
	})
	if err := os.MkdirAll(filepath.Join(base, "r1"), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := serve(t, http.MethodPost, "/api/runners/r1/stop", stopRoute)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s), expected 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "NOT-RUNNING") {
		t.Fatalf("should say it wasn't running to begin with, actual: %s", rec.Body.String())
	}
}

// ---- StopRunner: actually stop the process ----

func TestStopRunnerSignalsRunningProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process detection relies on /proc; only verified on Linux")
	}
	base := withConfig(t, &config.Config{
		Runners: config.RunnersConfig{Items: []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}}},
	})
	installDir := filepath.Join(base, "r1")
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".runner"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// run.sh deliberately does not exec, so bash stays in the process table and the argv shape matches actions/runner
	fifo := filepath.Join(installDir, ".test-block")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("failed to create FIFO: %v", err)
	}
	script := "#!/bin/bash\nread -r -t 120 < \"" + fifo + "\"\n"
	if err := os.WriteFile(filepath.Join(installDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, pid := range runnerproc.Find(installDir) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	if err := runner.Start(installDir); err != nil {
		t.Fatalf("failed to start during preparation: %v", err)
	}
	if !waitFor(func() bool { return runnerproc.Running(installDir) }) {
		t.Fatal("preparation phase: the process should already be running")
	}

	rec := serve(t, http.MethodPost, "/api/runners/r1/stop", stopRoute)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s), expected 200", rec.Code, rec.Body.String())
	}
	if !waitFor(func() bool { return !runnerproc.Running(installDir) }) {
		t.Fatal("the process should exit after a stop request is received")
	}
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// In container mode, still attempt the stop when status can't be probed, and return the probe result along with it:
// "Probe failed" is often exactly when a stop is needed; refusing the action here would lock the user out
func TestStopRunnerContainerModeProbeFailureStillAttempts(t *testing.T) {
	base := withConfig(t, &config.Config{
		Runners: config.RunnersConfig{
			ContainerMode:  true,
			VolumeHostPath: "/host/runners",
			Items:          []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}},
		},
	})
	installDir := filepath.Join(base, "r1")
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".runner"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make docker unavailable: both inspect and stop fail
	t.Setenv("PATH", t.TempDir())

	rec := serve(t, http.MethodPost, "/api/runners/r1/stop", stopRoute)
	// If it can't be stopped, report an error, not a false success like "not running"
	if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "probe") {
		t.Fatalf("should not report stopped when docker is unavailable, actual: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, expected 200 (with probe) or 500", rec.Code)
	}
	// Assert on the key, not the translation: this case doesn't install I18nLoader, so tr falls back to the key itself,
	// and the key is stable across the whole chain. Rewording the English later must not silently disable this guard.
	if strings.Contains(rec.Body.String(), "api.not_running") {
		t.Fatalf("a probe failure must not be treated as 'not running', actual: %s", rec.Body.String())
	}
}

// ---- read endpoints ----

func TestListRunnersReturnsConfiguredRunners(t *testing.T) {
	base := withConfig(t, &config.Config{
		Runners: config.RunnersConfig{Items: []config.RunnerItem{
			{Name: "r1", TargetType: "org", Target: "o1"},
			{Name: "r2", TargetType: "repo", Target: "o/r"},
		}},
	})
	if err := os.MkdirAll(filepath.Join(base, "r1"), 0o700); err != nil {
		t.Fatal(err)
	}

	rec := serve(t, http.MethodGet, "/api/runners", func(e *echo.Echo) {
		e.GET("/api/runners", ListRunners)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		// The response may also be wrapped as {"runners": [...]}; accept both
		var wrapped map[string][]map[string]any
		if err2 := json.Unmarshal(rec.Body.Bytes(), &wrapped); err2 != nil {
			t.Fatalf("response is not a JSON list: %s", rec.Body.String())
		}
		for _, v := range wrapped {
			list = v
		}
	}
	if len(list) != 2 {
		t.Fatalf("should return 2 runners, actual %d: %s", len(list), rec.Body.String())
	}
}

func TestGetRunner(t *testing.T) {
	base := withConfig(t, &config.Config{
		Runners: config.RunnersConfig{Items: []config.RunnerItem{{Name: "r1", TargetType: "org", Target: "o1"}}},
	})
	if err := os.MkdirAll(filepath.Join(base, "r1"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("存在", func(t *testing.T) {
		rec := serve(t, http.MethodGet, "/api/runners/r1", func(e *echo.Echo) {
			e.GET("/api/runners/:name", GetRunner)
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d (%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"r1"`) {
			t.Fatalf("the response should contain the runner name, actual: %s", rec.Body.String())
		}
	})

	t.Run("不存在", func(t *testing.T) {
		rec := serve(t, http.MethodGet, "/api/runners/nope", func(e *echo.Echo) {
			e.GET("/api/runners/:name", GetRunner)
		})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d, expected 404", rec.Code)
		}
	})

	t.Run("名字非法", func(t *testing.T) {
		rec := serve(t, http.MethodGet, "/api/runners/..", func(e *echo.Echo) {
			e.GET("/api/runners/:name", GetRunner)
		})
		if rec.Code == http.StatusOK {
			t.Fatal("an invalid name should not return 200")
		}
	})
}

// ---- resolveLang ----

// The UI in all six languages is selected by this function; a wrong choice means the whole page is in English.
//
// Priority: ?lang= > cookie > Accept-Language > en. The query goes ahead of the cookie:
// The cookie is the long-lived preference on this browser (the language dropdown writes it, then reloads, so no parameter appears in the URL);
// ?lang= is an explicit choice for this visit. Conversely, a link with ?lang=ja sent to someone who earlier chose Chinese
// still shows Chinese. This parameter fails for everyone who has set a preference, and that is exactly when it is most useful.
func TestResolveLang(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		cookie string
		header string
		want   string
	}{
		{"默认英文", "", "", "", "en"},
		{"query 优先于 cookie", "zh", "ja", "ko", "zh"},
		{"没有 query 时看 cookie", "", "ja", "ko", "ja"},
		{"最后 Accept-Language", "", "", "ko", "ko"},
		{"Accept-Language 带权重", "", "", "de-DE,de;q=0.9,en;q=0.8", "de"},
		{"不支持的 query 值跳过，继续看 cookie", "xx", "ja", "", "ja"},
		{"不支持的 cookie 值跳过，继续看 Accept-Language", "", "xx", "ko", "ko"},
		{"都不支持时回落英文", "xx", "yy", "zz", "en"},
		{"大小写不敏感", "ZH", "", "", "zh"},
		{"前后空白会被去掉", "  ja  ", "", "", "ja"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := "/"
			if tc.query != "" {
				// Must escape: the value may contain spaces, and splicing it directly into the URL would break even the request line parsing
				target += "?lang=" + url.QueryEscape(tc.query)
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "lang", Value: tc.cookie})
			}
			if tc.header != "" {
				req.Header.Set("Accept-Language", tc.header)
			}
			c := echo.New().NewContext(req, httptest.NewRecorder())
			if got := resolveLang(c); got != tc.want {
				t.Fatalf("resolveLang = %q, expected %q", got, tc.want)
			}
		})
	}
}

// ---- shortRandomSuffix ----

// Used to generate the suggested name when there is a name conflict; if it collides, the suggestion is useless
func TestShortRandomSuffix(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		s := shortRandomSuffix()
		if len(s) == 0 {
			t.Fatal("suffix must not be empty")
		}
		// It gets concatenated into container and directory names, so it may only be [a-z0-9].
		// Use an allowlist instead of a two-sided range comparison: the range form either trips staticcheck's
		// "!a && !b" (QF1001), or becomes a four-part inequality in reverse, and neither is as clear as
		// copying the character string directly from shortRandomSuffix.
		const allowed = "abcdefghijklmnopqrstuvwxyz0123456789"
		for _, r := range s {
			if !strings.ContainsRune(allowed, r) {
				t.Fatalf("suffix contains illegal characters %q: %s", r, s)
			}
		}
		seen[s] = true
	}
	if len(seen) < 100 {
		t.Fatalf("200 attempts produced only %d distinct suffixes, insufficient randomness", len(seen))
	}
}
