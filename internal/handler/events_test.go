package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
)

func useEventStore(t *testing.T) *eventlog.Store {
	t.Helper()
	orig := Events
	Events = eventlog.New(filepath.Join(t.TempDir(), "e.jsonl"))
	t.Cleanup(func() { Events = orig })
	return Events
}

func codes(events []eventlog.Event) string {
	var out []string
	for i := len(events) - 1; i >= 0; i-- {
		out = append(out, events[i].Code)
	}
	return strings.Join(out, ",")
}

func TestParseJobEvents(t *testing.T) {
	since := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	out := []byte(strings.Join([]string{
		"2026-11-04T09:00:00Z 2026-11-04 09:00:00Z: Running job: old",
		"2026-11-04T10:01:00Z 2026-11-04 10:01:00Z: Listening for Jobs",
		"2026-11-04T10:02:00Z 2026-11-04 10:02:00Z: Running job: verify",
		"2026-11-04T10:03:00Z 2026-11-04 10:03:00Z: Job verify completed with result: Failed",
		"2026-11-04T10:04:00Z 2026-11-04 10:04:00Z: Job build completed with result: Succeeded",
		"2026-11-04T10:05:00Z 2026-11-04 10:05:00Z: Job docker completed with result: Canceled",
		"not a log line",
		"",
	}, "\n"))
	events, last := parseJobEvents("a", out, since)
	var got []string
	for _, e := range events {
		got = append(got, e.Code+":"+e.Detail+":"+e.Level)
	}
	want := "job_started:verify:info job_failed:verify:error job_succeeded:build:info job_canceled:docker:warn"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %v\nwant %s", got, want)
	}
	if !last.Equal(since.Add(5 * time.Minute)) {
		t.Errorf("last = %v", last)
	}
}

func writeWatchConfig(t *testing.T, containerMode bool) func() (*config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "runners:\n  base_path: " + dir + "\n  items:\n    - {name: a, target_type: repo, target: o/r}\n"
	if containerMode {
		yaml = "runners:\n  base_path: " + dir + "\n  container_mode: true\n  volume_host_path: " + dir + "\n  job_docker_backend: none\n  items:\n    - {name: a, target_type: repo, target: o/r}\n"
	}
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() (*config.Config, error) { return config.Load(path) }
}

// installFakeDocker makes runner.ContainerState see state through a docker program on PATH.
func installFakeDocker(t *testing.T, stateFile string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\ninspect) cat \"" + stateFile + "\";;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWatcher_RecordsStateChangesOOMAndJobs(t *testing.T) {
	store := useEventStore(t)
	stateFile := filepath.Join(t.TempDir(), "inspect.json")
	setState := func(status string) {
		if err := os.WriteFile(stateFile, []byte(`[{"State":{"Status":"`+status+`"}}]`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installFakeDocker(t, stateFile)

	oom, logs, exit := "0", "", "0 false"
	stubDocker(t, func(args ...string) ([]byte, error) {
		switch args[0] {
		case "exec":
			return []byte("oom_kill " + oom + "\n"), nil
		case "logs":
			return []byte(logs), nil
		case "inspect":
			return []byte(exit), nil
		}
		return nil, errors.New("unexpected " + args[0])
	})
	w := NewWatcher(writeWatchConfig(t, true))
	w.started = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()

	setState("running")
	w.Pass(ctx)
	if got := store.Read(eventlog.Query{}); len(got) != 0 {
		t.Fatalf("the first pass must only set the baseline: %+v", got)
	}

	oom = "2"
	logs = "2026-11-04T10:02:00Z x: Running job: verify\n2026-11-04T10:03:00Z x: Job verify completed with result: Failed\n"
	w.Pass(ctx)
	w.Pass(ctx)
	setState("exited")
	exit = "137 true"
	w.Pass(ctx)
	oom = "0" // A started container gets a new cgroup, so its counter starts over.
	setState("running")
	w.Pass(ctx)
	setState("exited")
	exit = "1 true"
	w.Pass(ctx)
	setState("running")
	w.Pass(ctx)
	setState("exited")
	exit = "0 false"
	w.Pass(ctx)

	got := codes(store.Read(eventlog.Query{}))
	want := "oom,job_started,job_failed,stopped_oom,started,stopped_error,started,stopped"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestWatcher_ProcessModeAndRemovedRunners(t *testing.T) {
	store := useEventStore(t)
	load := writeWatchConfig(t, false)
	w := NewWatcher(load)
	w.Pass(context.Background())
	w.state["a"].running = true
	w.Pass(context.Background())
	w.state["a"].running = false
	w.state["gone"] = &watchedRunner{}
	w.Pass(context.Background())
	if _, ok := w.state["gone"]; ok {
		t.Error("a removed runner must be forgotten")
	}
	if got := codes(store.Read(eventlog.Query{})); got != "stopped" {
		t.Errorf("got %s", got)
	}
	bad := NewWatcher(func() (*config.Config, error) { return nil, errors.New("no config") })
	bad.Pass(context.Background())
}

func TestWatcher_RunStopsWithContext(t *testing.T) {
	useEventStore(t)
	w := NewWatcher(writeWatchConfig(t, false))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, time.Millisecond)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestExitDetails(t *testing.T) {
	stubDocker(t, func(...string) ([]byte, error) { return []byte("garbage"), nil })
	if code, oom := exitDetails(context.Background(), "c"); code != "" || oom {
		t.Errorf("code=%q oom=%v", code, oom)
	}
	stubDocker(t, func(...string) ([]byte, error) { return nil, errors.New("gone") })
	if code, _ := exitDetails(context.Background(), "c"); code != "" {
		t.Errorf("code=%q", code)
	}
}

func TestListEvents(t *testing.T) {
	store := useEventStore(t)
	store.Append(eventlog.Event{Runner: "a", Level: eventlog.LevelError, Code: "oom"})
	store.Append(eventlog.Event{Runner: "b", Level: eventlog.LevelInfo, Code: "started"})
	for query, want := range map[string]int{"": 2, "?runner=a": 1, "?level=info": 1, "?limit=1": 1, "?limit=99999": 2, "?limit=x": 2} {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/events"+query, nil), rec)
		if err := ListEvents(c); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Events []eventlog.Event `json:"events"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Events) != want {
			t.Errorf("%q: got %d, want %d", query, len(body.Events), want)
		}
	}
}

func TestRunnerLogs(t *testing.T) {
	load := writeWatchConfig(t, true)
	cfg, err := load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Runners.BasePath, "config.yaml")
	orig := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = orig })
	origCombined := dockerRunCombined
	t.Cleanup(func() { dockerRunCombined = origCombined })

	call := func(name, query string) (*httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/x"+query, nil), rec)
		c.SetParamNames("name")
		c.SetParamValues(name)
		return rec, RunnerLogs(c)
	}
	var gotArgs []string
	dockerRunCombined = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("line one\n"), nil
	}
	rec, err := call("a", "?tail=99999")
	if err != nil || !strings.Contains(rec.Body.String(), "line one") {
		t.Fatalf("err=%v body=%s", err, rec.Body.String())
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--tail 5000") {
		t.Errorf("args = %v", gotArgs)
	}
	if _, err := call("a", ""); err != nil || !strings.Contains(strings.Join(gotArgs, " "), "--tail 300") {
		t.Errorf("default tail: err=%v args=%v", err, gotArgs)
	}
	if _, err := call("missing", ""); err == nil {
		t.Error("an unknown runner must be refused")
	}
	if _, err := call("../x", ""); err == nil {
		t.Error("an unsafe name must be refused")
	}
	dockerRunCombined = func(context.Context, ...string) ([]byte, error) {
		return []byte("no such container"), errors.New("exit 1")
	}
	if _, err := call("a", ""); err == nil {
		t.Error("a docker failure must surface")
	}
	ConfigPath = filepath.Join(t.TempDir(), "plain.yaml")
	if err := os.WriteFile(ConfigPath, []byte("runners:\n  base_path: "+t.TempDir()+"\n  items:\n    - {name: a, target_type: repo, target: o/r}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := call("a", ""); err == nil {
		t.Error("process mode has no container logs")
	}
}

func TestWatcher_AutoCleansIdleRunners(t *testing.T) {
	var store *eventlog.Store
	useEventStore(t)
	useWatchConfig(t, true)
	var calls int
	var fail error
	cleanFixture2 := func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		return []byte("oops"), fail
	}
	orig := dockerRunCombined
	dockerRunCombined = cleanFixture2
	t.Cleanup(func() { dockerRunCombined = orig })
	store = Events

	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	w := NewWatcher(nil)
	w.now = func() time.Time { return now }
	st := &watchedRunner{dirtySince: now.Add(-2 * 24 * time.Hour)}

	w.autoClean(context.Background(), "a", st, 3)
	if calls != 0 {
		t.Fatal("two idle days are not three")
	}
	w.autoClean(context.Background(), "a", st, 0)
	if calls != 0 {
		t.Fatal("0 turns it off")
	}
	now = now.Add(2 * 24 * time.Hour)

	fail = exec.Command("sh", "-c", "exit 3").Run()
	w.autoClean(context.Background(), "a", st, 3)
	if calls != 1 || st.dirtySince.IsZero() {
		t.Fatalf("a busy runner stays dirty: calls=%d %+v", calls, st)
	}

	fail = errors.New("no such container")
	w.autoClean(context.Background(), "a", st, 3)
	if st.dirtySince != now {
		t.Errorf("a failure must wait for the next period: %+v", st)
	}
	if evs := store.Read(eventlog.Query{Level: eventlog.LevelError}); len(evs) != 1 || evs[0].Code != "clean_failed" {
		t.Errorf("events = %+v", evs)
	}

	fail = nil
	st.dirtySince = now.Add(-4 * 24 * time.Hour)
	w.autoClean(context.Background(), "a", st, 3)
	if !st.dirtySince.IsZero() {
		t.Errorf("a clean runner must not be cleaned again: %+v", st)
	}
	if evs := store.Read(eventlog.Query{Limit: 1}); evs[0].Code != "work_auto_cleaned" || evs[0].Detail != "3" {
		t.Errorf("events = %+v", evs)
	}
	before := calls
	w.autoClean(context.Background(), "a", st, 3)
	if calls != before {
		t.Error("cleaned twice")
	}
}

func TestWatcher_LastActivity(t *testing.T) {
	store := useEventStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	w := NewWatcher(nil)
	w.now = func() time.Time { return now }
	if got := w.lastActivity("a"); !got.Equal(now) {
		t.Errorf("no history = %v", got)
	}
	at := now.Add(-time.Hour)
	store.Append(eventlog.Event{Time: at, Runner: "a", Level: eventlog.LevelInfo, Code: "started"})
	store.Append(eventlog.Event{Time: at, Runner: "a", Level: eventlog.LevelInfo, Code: "job_succeeded"})
	if got := w.lastActivity("a"); !got.Equal(at) {
		t.Errorf("after a job = %v", got)
	}
	store.Append(eventlog.Event{Time: now, Runner: "a", Level: eventlog.LevelInfo, Code: "work_auto_cleaned"})
	if got := w.lastActivity("a"); !got.IsZero() {
		t.Errorf("after a clean = %v", got)
	}
}
