package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Process detection depends on /proc, verified only on Linux")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("No usable /proc in the current environment")
	}
}

// newRunnerDir builds an install directory that looks like a registered runner and sets it as this Agent's RUNNER_INSTALL_DIR.
// run.sh deliberately does not exec, so bash stays in the process table and argv matches the actions/runner shape.
func newRunnerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBlockingRunScript(t, dir)
	t.Setenv("RUNNER_INSTALL_DIR", dir)
	t.Cleanup(func() {
		for _, pid := range runnerproc.Find(dir) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return dir
}

func waitUntil(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func postStart(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handleStart(rec, httptest.NewRequest(http.MethodPost, "/start", nil))
	return rec
}

func TestGetStatus_ReflectsRunningProcess(t *testing.T) {
	requireLinux(t)
	dir := newRunnerDir(t)

	if status, running := getStatus(dir); status != "installed" || running {
		t.Fatalf("Before start it should be installed/false, got %q/%v", status, running)
	}
	if rec := postStart(t); rec.Code != http.StatusOK {
		t.Fatalf("/start returned %d: %s", rec.Code, rec.Body.String())
	}
	if !waitUntil(func() bool { _, r := getStatus(dir); return r }) {
		t.Fatal("run.sh started, but getStatus still reports not running")
	}
}

// Manager checks every 5 minutes for registered-but-not-running runners and starts them.
// Before the fix getStatus always returned false, so this idempotency guard did nothing and every cycle started another run.sh.
func TestHandleStart_IsIdempotent(t *testing.T) {
	requireLinux(t)
	dir := newRunnerDir(t)

	if rec := postStart(t); rec.Code != http.StatusOK {
		t.Fatalf("First /start returned %d: %s", rec.Code, rec.Body.String())
	}
	if !waitUntil(func() bool { _, r := getStatus(dir); return r }) {
		t.Fatal("Should be running after the first /start")
	}

	// Simulate the next 9 scheduled starts
	for i := 0; i < 9; i++ {
		rec := postStart(t)
		if rec.Code != http.StatusOK {
			t.Fatalf("/start #%d returned %d: %s", i+2, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "already running") {
			t.Fatalf("/start #%d should reply already running, got %s", i+2, rec.Body.String())
		}
	}

	if got := runnerproc.Find(dir); len(got) != 1 {
		t.Fatalf("After 10 /start calls there should be exactly 1 runner process, found %d: %v", len(got), got)
	}
}

func TestHandleStop_TerminatesRunner(t *testing.T) {
	requireLinux(t)
	dir := newRunnerDir(t)
	if rec := postStart(t); rec.Code != http.StatusOK {
		t.Fatalf("/start returned %d: %s", rec.Code, rec.Body.String())
	}
	if !waitUntil(func() bool { _, r := getStatus(dir); return r }) {
		t.Fatal("Setup phase: should be running")
	}

	rec := httptest.NewRecorder()
	handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/stop returned %d: %s", rec.Code, rec.Body.String())
	}
	if !waitUntil(func() bool { _, r := getStatus(dir); return !r }) {
		t.Fatal("Still running after /stop")
	}
}

// /stop must fail when nothing is running, not succeed silently
func TestHandleStop_NotRunningReportsError(t *testing.T) {
	requireLinux(t)
	newRunnerDir(t)
	rec := httptest.NewRecorder()
	handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("/stop should not return 200 when no process is running: %s", rec.Body.String())
	}
}

// A directory containing something that looks like a pid file must not skew the status either:
// actions/runner does not write Runner.Listener.pid, and .path holds a PATH string
func TestGetStatus_PidLookalikeFilesAreIgnored(t *testing.T) {
	requireLinux(t)
	dir := newRunnerDir(t)
	if err := os.WriteFile(filepath.Join(dir, "Runner.Listener.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".path"),
		[]byte("/usr/local/bin:/usr/bin:/bin"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, running := getStatus(dir); running {
		t.Fatal("No process is running, so a pid file should not make the status running")
	}
}

// An unregistered directory (no .runner) should report new
func TestGetStatus_UnregisteredDirIsNew(t *testing.T) {
	dir := t.TempDir()
	if status, running := getStatus(dir); status != "new" || running {
		t.Fatalf("Without .runner it should be new/false, got %q/%v", status, running)
	}
}

// writeBlockingRunScript writes a run.sh that blocks forever and **spawns no child processes**.
//
// Do not use `sleep 120`: bash forks a child to exec sleep, and between fork and execve
// the child's /proc/<pid>/cmdline still shows bash's argv (`/bin/bash <dir>/run.sh`),
// so it is counted as a second watchdog script. That window is only a few microseconds, rarely hit locally, but on a busy CI machine it does hit—
// "After 10 /start calls there should be exactly 1 runner process, found 2: [2727 2728]" came from exactly this.
//
// read is a builtin, so the redirect is handled by the current shell; opening a FIFO with no writer blocks in place,
// with no fork at any point, so the process count stays deterministic.
func writeBlockingRunScript(t *testing.T, dir string) {
	t.Helper()
	fifo := filepath.Join(dir, ".test-block")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Failed to create FIFO: %v", err)
	}
	script := "#!/bin/bash\nread -r -t 120 < \"" + fifo + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
