package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

// termIgnored and termExitsAfterDelay are two ways run.sh reacts to SIGTERM,
// used to tell apart "exits, but needs some time to wrap up" from "ignores SIGTERM entirely".
//
// The wind-down must take observable time, otherwise these cases cannot prove that shutdown really waits: a
// a bash with `trap 'exit 0' TERM` takes only 0.7 ms from receiving the signal until cmdline in /proc becomes empty,
// which is shorter than one polling interval, so an implementation that "returns right after sending the signal" would also pass.
func termIgnored(string) string { return "" }

// termExitsAfterDelay makes the trap first block on a FIFO for the given duration, then exit.
//
// It uses `exec 3<>fifo` + `read -t` instead of `sleep N`: the latter forks, and between fork and execve
// the child's cmdline is still `/bin/bash <dir>/run.sh`, which runnerproc would mistake for a second
// watchdog script (see the note on writeBlockingRunScript). Opening the FIFO read-write does not block in open,
// so the later read is what hits the -t timeout, and the whole delay spawns no child process.
func termExitsAfterDelay(d time.Duration) func(dir string) string {
	return func(dir string) string {
		return `exec 3<>"` + filepath.Join(dir, ".test-delay") + `"; ` +
			`read -r -t ` + strconv.Itoa(int(d/time.Second)) + ` -u 3; exit 0`
	}
}

// termExitsNow exits immediately. It is only used in the case where "no runner is running to begin with":
// it disappears in 0.7 ms, so it cannot reveal waiting behavior.
func termExitsNow(string) string { return "exit 0" }

// newTrappingRunnerDir is the same as newRunnerDir, except run.sh has one extra TERM trap.
// The body blocks the same way as writeBlockingRunScript: opening a FIFO with no writer blocks in place,
// read is a builtin and never forks, so the process table holds only one watchdog script.
func newTrappingRunnerDir(t *testing.T, trap func(dir string) string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".test-block", ".test-delay"} {
		if err := syscall.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
			t.Fatalf("failed to create FIFO %s: %v", name, err)
		}
	}
	script := "#!/bin/bash\ntrap '" + trap(dir) + "' TERM\n" +
		": > \"" + filepath.Join(dir, ".test-ready") + "\"\n" +
		"read -r -t 120 < \"" + filepath.Join(dir, ".test-block") + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNNER_INSTALL_DIR", dir)
	// The variant that ignores SIGTERM can only be ended by SIGKILL, otherwise it hangs in the FIFO's open and never returns,
	// and drags down t.TempDir() cleanup with it
	t.Cleanup(func() {
		for _, pid := range runnerproc.Find(dir) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return dir
}

// startAndWait waits for the script to emit a ready marker after installing the trap; appearing in the process table alone is not enough.
func startAndWait(t *testing.T, dir string) {
	t.Helper()
	if err := startRunner(dir); err != nil {
		t.Fatalf("failed to start run.sh: %v", err)
	}
	if !waitUntil(func() bool {
		_, err := os.Stat(filepath.Join(dir, ".test-ready"))
		return err == nil && runnerproc.Running(dir)
	}) {
		t.Fatal("setup stage: run.sh should already be running")
	}
}

// The 30-second grace period of `docker stop -t 30` never reached the Runner: the Agent is PID 1 in the container,
// it does not subscribe to SIGTERM, so the Go runtime exits the process at once, and the kernel immediately sends SIGKILL to
// everything else left in the namespace. shutdown must spend this grace period actually waiting for the Runner to wrap up.
//
// Mutation: remove the wait in shutdown (return right after sending SIGTERM), and this case turns red --
// run.sh is still wrapping up in the trap, so stopped is false.
func TestShutdown_StopsRunnerAndWaits(t *testing.T) {
	requireLinux(t)
	const windDown = 1 * time.Second
	dir := newTrappingRunnerDir(t, termExitsAfterDelay(windDown))
	startAndWait(t, dir)

	const grace = 10 * time.Second
	start := time.Now()
	stopped := shutdown(context.Background(), dir, grace)
	elapsed := time.Since(start)

	if !stopped {
		t.Fatalf("run.sh should exit within %s, and shutdown should report stopped (took %s)", windDown, elapsed)
	}
	if runnerproc.Running(dir) {
		t.Fatal("shutdown returned stopped, but a runner is still in the process table")
	}
	// It really waited for it to finish wrapping up: if it returned right after sending the signal, this would only take a few milliseconds
	if elapsed < windDown*7/10 {
		t.Fatalf("returned without waiting for run.sh to wrap up: took %s, wrap-up needs %s", elapsed, windDown)
	}
	// And it did not wait the full grace period: that would mean it was not polling, just sleeping out the time
	if elapsed >= grace {
		t.Fatalf("should return as soon as run.sh exits, but it used the full grace period %s", elapsed)
	}
}

// Conversely: it must not wait forever when the Runner refuses to leave. If it kept waiting, once docker stop's timer fires,
// the Agent itself gets SIGKILLed, and not even a line saying "still has not stopped" would be left in the log.
//
// Mutation: remove the timeout in shutdown, and this case hangs until go test's timeout fails it,
// so this package must run with -timeout (the Makefile's check uses go test -race, default 10 minutes).
func TestShutdown_GivesUpAfterGrace(t *testing.T) {
	requireLinux(t)
	// trap '' TERM sets SIGTERM to SIG_IGN, so the script is not interrupted
	dir := newTrappingRunnerDir(t, termIgnored)
	startAndWait(t, dir)

	const grace = 500 * time.Millisecond
	start := time.Now()
	stopped := shutdown(context.Background(), dir, grace)
	elapsed := time.Since(start)

	if stopped {
		t.Fatal("run.sh ignored SIGTERM, so shutdown should not report stopped")
	}
	if elapsed < grace {
		t.Fatalf("should wait the full grace period before giving up, but only waited %s", elapsed)
	}
	// Leave the limit ample headroom: the poll interval is 200ms, and one extra round on a slow machine is normal
	if elapsed > grace+4*runnerPollInterval+2*time.Second {
		t.Fatalf("Returned too late, past the grace period: %s (grace period %s)", elapsed, grace)
	}
}

// If no Runner is running, shutdown should return immediately instead of waiting out a grace period.
// This is the normal case in a container: idle Runners get stopped, and only then is the container docker-stopped.
func TestShutdown_NoRunnerReturnsImmediately(t *testing.T) {
	requireLinux(t)
	dir := newTrappingRunnerDir(t, termExitsNow)

	start := time.Now()
	if !shutdown(context.Background(), dir, 10*time.Second) {
		t.Fatal("should report stopped right away when no runner is running")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("should not wait when no runner is running, got %s", elapsed)
	}
}

// Regression guard: an http.Server without timeouts can wait forever on a peer that has stopped talking,
// and a half-open connection can hold a goroutine and an fd until the process exits. Runner containers are long-lived.
// Nothing at compile time enforces these fields, so deleting a line still builds and runs. Pin them here.
func TestNewAgentServer_HasTimeouts(t *testing.T) {
	srv := newAgentServer("8081", newAgentMux())

	if srv.Addr != ":8081" {
		t.Fatalf("listen address should be :8081, got %q", srv.Addr)
	}
	for _, tc := range []struct {
		name string
		got  time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout},
		{"ReadTimeout", srv.ReadTimeout},
		{"WriteTimeout", srv.WriteTimeout},
		{"IdleTimeout", srv.IdleTimeout},
	} {
		if tc.got <= 0 {
			t.Errorf("%s should be positive, got %v", tc.name, tc.got)
		}
	}
	if srv.MaxHeaderBytes <= 0 {
		t.Errorf("MaxHeaderBytes should be positive, got %d", srv.MaxHeaderBytes)
	}
	// Agent handlers all return immediately, so WriteTimeout has none of the concerns it has on the Manager side.
	if srv.WriteTimeout > time.Minute {
		t.Errorf("WriteTimeout is set too loose: %v", srv.WriteTimeout)
	}
}

// Separate mux rather than DefaultServeMux: the latter is a package-level global,
// and registering the same path twice panics, so tests can only build the server once.
func TestNewAgentMux_ServesHealthAndIsReusable(t *testing.T) {
	t.Setenv("AGENT_TOKEN", "a-token")
	mux := newAgentMux()

	req, err := http.NewRequest(http.MethodGet, "/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	if h, pattern := mux.Handler(req); pattern == "" || h == nil {
		t.Fatal("/health should have a handler")
	}
	// Build it again: with DefaultServeMux this would panic
	if second := newAgentMux(); second == nil {
		t.Fatal("newAgentMux should be buildable repeatedly")
	}
}
