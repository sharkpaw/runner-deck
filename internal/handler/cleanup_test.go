package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
)

func cleanFixture(t *testing.T, fn func(args ...string) ([]byte, error)) *eventlog.Store {
	t.Helper()
	store := useEventStore(t)
	orig := dockerRunCombined
	dockerRunCombined = func(_ context.Context, args ...string) ([]byte, error) { return fn(args...) }
	t.Cleanup(func() { dockerRunCombined = orig })
	return store
}

func cleanRunner(t *testing.T, name string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/x", nil), rec)
	c.SetParamNames("name")
	c.SetParamValues(name)
	return rec, CleanRunnerWork(c)
}

func httpCode(err error) int {
	var he *echo.HTTPError
	if errors.As(err, &he) {
		return he.Code
	}
	return 0
}

func useWatchConfig(t *testing.T, containerMode bool) {
	t.Helper()
	cfg, err := writeWatchConfig(t, containerMode)()
	if err != nil {
		t.Fatal(err)
	}
	orig := ConfigPath
	ConfigPath = filepath.Join(cfg.Runners.BasePath, "config.yaml")
	t.Cleanup(func() { ConfigPath = orig })
}

func TestCleanRunnerWork(t *testing.T) {
	useWatchConfig(t, true)
	var got []string
	result := func(args ...string) ([]byte, error) { got = args; return nil, nil }
	store := cleanFixture(t, func(args ...string) ([]byte, error) { return result(args...) })

	rec, err := cleanRunner(t, "a")
	if err != nil || rec.Code != http.StatusOK {
		t.Fatalf("code=%d err=%v", rec.Code, err)
	}
	if strings.Join(got[:4], " ") != "exec -u 0 github-runner-a" {
		t.Errorf("args = %v", got)
	}
	if evs := store.Read(eventlog.Query{}); len(evs) != 1 || evs[0].Code != "work_cleaned" {
		t.Errorf("events = %+v", evs)
	}

	result = func(...string) ([]byte, error) { return []byte("no such container"), errors.New("exit 1") }
	if _, err := cleanRunner(t, "a"); httpCode(err) != http.StatusBadGateway {
		t.Errorf("failure: %v", err)
	}
	if evs := store.Read(eventlog.Query{Level: eventlog.LevelError}); len(evs) != 1 || evs[0].Code != "clean_failed" {
		t.Errorf("events = %+v", evs)
	}
	if _, err := cleanRunner(t, "missing"); httpCode(err) != http.StatusNotFound {
		t.Errorf("unknown runner: %v", err)
	}
	if _, err := cleanRunner(t, "../x"); httpCode(err) != http.StatusBadRequest {
		t.Errorf("unsafe name: %v", err)
	}
}

func TestCleanRunnerWork_RefusesWhileAJobRuns(t *testing.T) {
	useWatchConfig(t, true)
	busy := exec.Command("sh", "-c", "exit 3").Run()
	cleanFixture(t, func(...string) ([]byte, error) { return nil, busy })
	if _, err := cleanRunner(t, "a"); httpCode(err) != http.StatusConflict {
		t.Fatalf("got %v", err)
	}
}

func TestCleanRunnerWork_ProcessModeRefused(t *testing.T) {
	useWatchConfig(t, false)
	cleanFixture(t, func(...string) ([]byte, error) { return nil, nil })
	if _, err := cleanRunner(t, "a"); httpCode(err) != http.StatusBadRequest {
		t.Fatalf("got %v", err)
	}
}

// The script itself, against a fake /runner tree and process table.
func TestCleanWorkScript_KeepsToolCache(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "_work")
	for _, p := range []string{"_tool/node", "repo/repo", "_actions/a", "_temp/x"} {
		if err := os.MkdirAll(filepath.Join(work, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := strings.ReplaceAll(cleanWorkScript, "/runner/_work", work)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "_tool" {
		t.Fatalf("left %v", entries)
	}
}

func TestCleanDocker(t *testing.T) {
	var got []string
	result := func() ([]byte, error) {
		return []byte("Deleted: x\nTotal reclaimed space: 1.2GB\n"), nil
	}
	store := cleanFixture(t, func(args ...string) ([]byte, error) { got = args; return result() })
	post := func(body string) (*httptest.ResponseRecorder, error) {
		req := httptest.NewRequest(http.MethodPost, "/api/docker/clean", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		return rec, CleanDocker(echo.New().NewContext(req, rec))
	}
	rec, err := post(`{"target":"images"}`)
	if err != nil || !strings.Contains(rec.Body.String(), "1.2GB") {
		t.Fatalf("err=%v body=%s", err, rec.Body.String())
	}
	if strings.Join(got, " ") != "image prune -af --filter until=168h" {
		t.Errorf("args = %v", got)
	}
	result = func() ([]byte, error) { return []byte("ID x\nTotal:\t3.3GB\n"), nil }
	rec, err = post(`{"target":"build-cache"}`)
	if err != nil || !strings.Contains(rec.Body.String(), "3.3GB") {
		t.Fatalf("err=%v body=%s", err, rec.Body.String())
	}
	if strings.Join(got, " ") != "builder prune -af --filter until=24h" {
		t.Errorf("args = %v", got)
	}
	result = func() ([]byte, error) { return []byte("nothing"), nil }
	if rec, err := post(`{"target":"images"}`); err != nil || !strings.Contains(rec.Body.String(), "0B") {
		t.Errorf("err=%v body=%s", err, rec.Body.String())
	}
	for _, bad := range []string{`{"target":"volumes"}`, `{"target":""}`, `not json`} {
		if _, err := post(bad); httpCode(err) != http.StatusBadRequest {
			t.Errorf("%s: %v", bad, err)
		}
	}
	result = func() ([]byte, error) { return []byte("daemon down"), errors.New("exit 1") }
	if _, err := post(`{"target":"images"}`); httpCode(err) != http.StatusBadGateway {
		t.Errorf("failure: %v", err)
	}
	if evs := store.Read(eventlog.Query{Level: eventlog.LevelError}); len(evs) != 1 {
		t.Errorf("events = %+v", evs)
	}
	cleanMu.Lock()
	_, err = post(`{"target":"images"}`)
	cleanMu.Unlock()
	if httpCode(err) != http.StatusConflict {
		t.Errorf("a second cleanup must wait: %v", err)
	}
}

func TestExitCodeOf(t *testing.T) {
	if exitCodeOf(errors.New("plain")) != -1 {
		t.Error("a non-exit error must report -1")
	}
}
