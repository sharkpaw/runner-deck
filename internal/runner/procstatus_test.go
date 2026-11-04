package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// newFakeRunnerDir creates a directory that looks like a registered runner: it has a .runner file and a run.sh that stays resident.
// run.sh deliberately does not exec, so bash stays in the process table, with an argv that matches the shape used by actions/runner.
func newFakeRunnerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBlockingRunScript(t, dir)
	return dir
}

func startFakeRunner(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(filepath.Join(dir, "run.sh"))
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start fake run.sh: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd
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

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Process detection relies on /proc, verified on Linux only")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("No usable /proc in this environment")
	}
}

// This is the direct regression case for the "relaunched every 5 minutes" bug:
// While the runner is running, getStatus must report running=true; otherwise the background
// check (info.Status == StatusInstalled && !info.Running) keeps relaunching it, round after round.
func TestGetStatus_RunningRunnerReportsRunning(t *testing.T) {
	requireLinux(t)
	dir := newFakeRunnerDir(t)

	status, running := getStatus(dir)
	if status != StatusInstalled {
		t.Fatalf("With a .runner present, status should be %q, got %q", StatusInstalled, status)
	}
	if running {
		t.Fatal("No process started yet, running should be false")
	}

	startFakeRunner(t, dir)
	if !waitUntil(func() bool { _, r := getStatus(dir); return r }) {
		t.Fatal("run.sh is already running, but getStatus still reports running=false -- the background check keeps relaunching it")
	}
	if status, _ := getStatus(dir); status != StatusInstalled {
		t.Fatalf("Running status should still be %q, got %q", StatusInstalled, status)
	}
}

// actions/runner does not write a pid file; .path holds the PATH string.
// Nothing placed in the directory may cause a runner that is not running to be judged as running.
func TestGetStatus_PidLookalikeFilesDoNotFakeRunning(t *testing.T) {
	requireLinux(t)
	dir := newFakeRunnerDir(t)
	if err := os.WriteFile(filepath.Join(dir, "Runner.Listener.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".path"),
		[]byte("/usr/local/bin:/usr/bin:/bin"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, running := getStatus(dir); running {
		t.Fatal("A pid file in the directory must not make a runner that isn't running look running")
	}
}

// Two runners on the same host must not get mixed up with each other.
func TestGetStatus_RunnersDoNotSeeEachOther(t *testing.T) {
	requireLinux(t)
	a, b := newFakeRunnerDir(t), newFakeRunnerDir(t)
	startFakeRunner(t, a)
	if !waitUntil(func() bool { _, r := getStatus(a); return r }) {
		t.Fatal("a should be judged running")
	}
	if _, running := getStatus(b); running {
		t.Fatal("b has no process and must not be marked running by a's process")
	}
}

// Before the fix, Stop went through readRunnerPid, and the pid file never exists,
// So in default mode, "Stop" in the UI always reports "runner pid file not found or pid invalid"
func TestStop_TerminatesRunningRunner(t *testing.T) {
	requireLinux(t)
	dir := newFakeRunnerDir(t)
	startFakeRunner(t, dir)
	if !waitUntil(func() bool { _, r := getStatus(dir); return r }) {
		t.Fatal("Setup phase: runner should be running")
	}
	if err := Stop(dir); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if !waitUntil(func() bool { _, r := getStatus(dir); return !r }) {
		t.Fatal("Runner still running after Stop")
	}
}

// When nothing is running, Stop must say so clearly rather than succeed silently.
func TestStop_NotRunningReportsError(t *testing.T) {
	requireLinux(t)
	dir := newFakeRunnerDir(t)
	if err := Stop(dir); err == nil {
		t.Fatal("Stop should return an error when no process is running")
	}
}

// List scans /proc only once for the whole batch of runners, so dispatch must be correct:
// the one that is running reports running, and the one that is not must not be marked running by it.
func TestList_PerRunnerRunningStateInDefaultMode(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath: base,
		Items: []config.RunnerItem{
			{Name: "alpha", Path: "alpha"},
			{Name: "beta", Path: "beta"},
			{Name: "gamma", Path: "gamma"}, // Create no directories at all; should be missing.
		},
	}}
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
		writeBlockingRunScript(t, dir)
	}
	startFakeRunner(t, filepath.Join(base, "alpha"))

	if !waitUntil(func() bool {
		for _, info := range List(cfg) {
			if info.Name == "alpha" && info.Running {
				return true
			}
		}
		return false
	}) {
		t.Fatal("alpha started, but List still reports not running")
	}

	byName := map[string]RunnerInfo{}
	for _, info := range List(cfg) {
		byName[info.Name] = info
	}
	if got := byName["alpha"]; got.Status != StatusInstalled || !got.Running {
		t.Fatalf("alpha should be installed/true, got %q/%v", got.Status, got.Running)
	}
	if got := byName["beta"]; got.Status != StatusInstalled || got.Running {
		t.Fatalf("beta should be installed/false, got %q/%v -- must not be marked running by alpha's process", got.Status, got.Running)
	}
	if got := byName["gamma"]; got.Status != StatusMissing || got.Running {
		t.Fatalf("gamma directory does not exist, should be missing/false, got %q/%v", got.Status, got.Running)
	}
}

// An unregistered directory (no .runner) must not be judged installed just because it happens to contain a process.
func TestList_UnregisteredDirStaysNew(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	dir := filepath.Join(base, "alpha")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	writeBlockingRunScript(t, dir)
	startFakeRunner(t, dir)
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath: base,
		Items:    []config.RunnerItem{{Name: "alpha", Path: "alpha"}},
	}}
	list := List(cfg)
	if len(list) != 1 || list[0].Status != StatusNew || list[0].Running {
		t.Fatalf("With no .runner, should be new/false, got %+v", list)
	}
}

// writeBlockingRunScript writes a run.sh that blocks indefinitely and **spawns no child processes at all**.
//
// Do not use `sleep 120`: bash forks a child process to exec sleep, and between fork and execve,
// the child's /proc/<pid>/cmdline still shows bash's argv (`/bin/bash <dir>/run.sh`),
// so it gets counted as a second supervisor script. The window is only a few microseconds, so it almost never hits locally, but on a busy CI machine it does —
// "after 10 /start calls there should be only 1 runner process, but there are 2: [2727 2728]" is exactly how this happened.
//
// read is a builtin, and the redirect is handled by the current shell itself; opening a FIFO with no writer blocks in place,
// Never forks, so the process count is deterministic.
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
