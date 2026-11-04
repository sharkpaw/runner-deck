package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/runner"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

// mkTrappingRunnerDir is the same as mkRunnerDir, except run.sh gets an extra TERM trap,
// to tell apart a runner that exits but needs a moment to wind down from one that ignores SIGTERM entirely.
//
// The wind-down must take observable time. A bash running `trap 'exit 0' TERM` needs
// only 0.7 ms from receiving the signal until its cmdline in /proc is empty, which is less than one polling interval, so an implementation that returns right after sending the signal would also pass.
// That would test nothing.
//
// The delay uses `exec 3<>fifo` with `read -t` instead of `sleep N`, since the latter forks, and between fork and
// execve the child's cmdline is still `/bin/bash <dir>/run.sh`, which runnerproc
// would mistake for a second watchdog script (see the note in writeBlockingRunScript).
func mkTrappingRunnerDir(t *testing.T, base, name string, windDown time.Duration) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".test-block", ".test-delay"} {
		if err := syscall.Mkfifo(filepath.Join(dir, f), 0o600); err != nil {
			t.Fatalf("failed to create FIFO %s: %v", f, err)
		}
	}
	// windDown <= 0 means ignore SIGTERM: trap '' TERM sets it to SIG_IGN
	trap := ""
	if windDown > 0 {
		trap = `exec 3<>"` + filepath.Join(dir, ".test-delay") + `"; ` +
			`read -r -t ` + strconv.Itoa(int(windDown/time.Second)) + ` -u 3; exit 0`
	}
	script := "#!/bin/bash\ntrap '" + trap + "' TERM\n" +
		": > \"" + filepath.Join(dir, ".test-ready") + "\"\n" +
		"read -r -t 120 < \"" + filepath.Join(dir, ".test-block") + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// A version that ignores SIGTERM can only be ended by SIGKILL, otherwise it hangs in open on the FIFO and never returns
	t.Cleanup(func() {
		for _, pid := range runnerproc.Find(dir) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return dir
}

func startRunnerAt(t *testing.T, dir string) {
	t.Helper()
	if err := runner.Start(dir); err != nil {
		t.Fatalf("failed to start run.sh for %s: %v", dir, err)
	}
	if !waitUntil(func() bool {
		_, err := os.Stat(filepath.Join(dir, ".test-ready"))
		return err == nil && runnerproc.Running(dir)
	}) {
		t.Fatalf("setup phase: run.sh for %s should already be running", dir)
	}
}

// In default mode, run.sh is a child of the Manager and shares its PID namespace:
// When the Manager exits, the kernel SIGKILLs every remaining process in the namespace, so the job is not gracefully cancelled.
// The shutdown path must stop the runners first and wait for them to wind down.
//
// Mutation: remove the wait in stopDefaultModeRunners (return right after sending SIGTERM),
// this test turns red, since both run.sh are still winding down in their trap.
func TestStopDefaultModeRunners_StopsRunningAndWaits(t *testing.T) {
	requireLinux(t)
	const windDown = 1 * time.Second
	base := t.TempDir()
	a := mkTrappingRunnerDir(t, base, "a", windDown)
	b := mkTrappingRunnerDir(t, base, "b", windDown)
	startRunnerAt(t, a)
	startRunnerAt(t, b)

	const grace = 10 * time.Second
	start := time.Now()
	stopDefaultModeRunners(context.Background(), cfgWith(base, "a", "b"), grace)
	elapsed := time.Since(start)

	for _, dir := range []string{a, b} {
		if runnerproc.Running(dir) {
			t.Errorf("run.sh for %s is still running", dir)
		}
	}
	// Real wait for the wind-down to finish. If it returned right after sending the signal, this would take only a few milliseconds
	if elapsed < windDown*7/10 {
		t.Fatalf("returned without waiting for run.sh to wind down: took %s, wind-down needs %s", elapsed, windDown)
	}
	if elapsed >= grace {
		t.Fatalf("should return once run.sh exits, but it used the full grace period %s", elapsed)
	}
}

// A runner that refuses to exit must not be waited on forever. If we keep waiting, docker stop's timer fires
// and the Manager itself gets SIGKILLed, leaving not even a log line saying it had not stopped yet.
//
// Mutation: remove the timeout, and this test hangs until go test's own timeout fails it.
func TestStopDefaultModeRunners_GivesUpAfterGrace(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	stubborn := mkTrappingRunnerDir(t, base, "stubborn", 0) // trap '' TERM
	startRunnerAt(t, stubborn)

	const grace = 500 * time.Millisecond
	start := time.Now()
	stopDefaultModeRunners(context.Background(), cfgWith(base, "stubborn"), grace)
	elapsed := time.Since(start)

	if elapsed < grace {
		t.Fatalf("should wait the full grace period before giving up, but only waited %s", elapsed)
	}
	if elapsed > grace+4*runnerPollInterval+2*time.Second {
		t.Fatalf("returned too long after the grace period: %s (grace period %s)", elapsed, grace)
	}
	// Giving up is not the same as cleaning up: the kernel sends SIGKILL itself after the Manager exits, so this code should not do it.
	if !runnerproc.Running(stubborn) {
		t.Fatal("run.sh that ignores SIGTERM should not disappear within the grace period; the test's precondition does not hold")
	}
}

// A runner that is not running should not be treated as something to wait on, otherwise an idle Manager
// would waste a full grace period on every restart
func TestStopDefaultModeRunners_IdleRunnersCostNothing(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	mkTrappingRunnerDir(t, base, "idle", time.Second)

	start := time.Now()
	stopDefaultModeRunners(context.Background(), cfgWith(base, "idle"), 10*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("should not wait when no runner is running, but waited %s", elapsed)
	}
}

// Return immediately when the context is cancelled (the outer fallback timeout fired), instead of holding on for the grace period
func TestStopDefaultModeRunners_HonorsContext(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	stubborn := mkTrappingRunnerDir(t, base, "stubborn", 0)
	startRunnerAt(t, stubborn)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	stopDefaultModeRunners(ctx, cfgWith(base, "stubborn"), time.Minute)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("should return immediately after context cancellation, but took %s", elapsed)
	}
}

// Regression guard: an http.Server without timeouts waits indefinitely on a peer that has stopped talking,
// so a single half-open connection can hold a goroutine and a file descriptor until the process exits.
// These fields have no compile-time constraints, and deleting a line still compiles and runs, so pin them here.
func TestNewHTTPServer_HasTimeouts(t *testing.T) {
	srv := newHTTPServer(":8080", echo.New())

	if srv.Addr != ":8080" {
		t.Fatalf("listen address should be :8080, got %q", srv.Addr)
	}
	for _, tc := range []struct {
		name string
		got  time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout},
		{"ReadTimeout", srv.ReadTimeout},
		{"IdleTimeout", srv.IdleTimeout},
	} {
		if tc.got <= 0 {
			t.Errorf("%s should be positive, got %v", tc.name, tc.got)
		}
	}
	if srv.MaxHeaderBytes <= 0 {
		t.Errorf("MaxHeaderBytes should be positive, got %d", srv.MaxHeaderBytes)
	}
	// WriteTimeout must be left empty: it is measured from the start of writing the response header, while RecreateRunner's
	// lifecycleContext can take up to 90 seconds. Any WriteTimeout shorter than that will cut the connection in the container
	// **after the rebuild has already succeeded**, leaving only a network error in the UI, so the user clicks again.
	if srv.WriteTimeout != 0 {
		t.Errorf("Manager should not set WriteTimeout (see the comment in newHTTPServer), actual %v", srv.WriteTimeout)
	}
}

// Write endpoints only receive very small JSON. Without a limit, a continuously sent request body would be read into memory
// whereas Basic Auth is only checked after that.
func TestNewEchoServer_LimitsRequestBody(t *testing.T) {
	e := newEchoServer()

	// 2MB, exceeding the 1M limit; BodyLimit is attached before CSRF and auth, so there is no need to forge request headers here
	req := httptest.NewRequest(http.MethodPost, "/api/runners", bytes.NewReader(make([]byte, 2<<20)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("2MB request body should be rejected with 413, actual %d: %s", rec.Code, rec.Body.String())
	}
}
