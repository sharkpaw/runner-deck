package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Process probing relies on /proc and is verified only on Linux")
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("No usable /proc in the current environment")
	}
}

// mkRunnerDir creates a runner directory. registered controls whether it gets a .runner file (that is, whether it is registered).
// run.sh deliberately does not exec, so bash stays in the process table and its argv matches the shape used by actions/runner.
func mkRunnerDir(t *testing.T, base, name string, registered bool) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if registered {
		if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBlockingRunScript(t, dir)
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

func cfgWith(base string, names ...string) *config.Config {
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: base}}
	for _, n := range names {
		cfg.Runners.Items = append(cfg.Runners.Items, config.RunnerItem{Name: n, Path: n})
	}
	return cfg
}

// Only start runners that are registered and not running. Starting unregistered ones would create directories and containers out of nothing,
// and starting one that is already running would add a second listener to the same Runner
func TestStartIdleRunners_StartsOnlyRegisteredAndIdle(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	idle := mkRunnerDir(t, base, "idle", true)
	busy := mkRunnerDir(t, base, "busy", true)
	unreg := mkRunnerDir(t, base, "unreg", false)

	// Start busy first
	cmd := exec.Command(filepath.Join(busy, "run.sh"))
	cmd.Dir = busy
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	if !waitUntil(func() bool { return runnerproc.Running(busy) }) {
		t.Fatal("Setup phase: busy should be running")
	}
	busyPids := runnerproc.Find(busy)

	startIdleRunners(context.Background(), cfgWith(base, "idle", "busy", "unreg"), "测试拉起")

	if !waitUntil(func() bool { return runnerproc.Running(idle) }) {
		t.Fatal("a registered, idle runner that is not running should be started")
	}
	if got := runnerproc.Find(busy); len(got) != len(busyPids) {
		t.Fatalf("a busy runner that is already running should not be started again: process changed from %v to %v", busyPids, got)
	}
	if runnerproc.Running(unreg) {
		t.Fatal("an unreg runner without .runner should not be started")
	}
}

// With an empty config or no runners, it should return quietly without panicking
func TestStartIdleRunners_HandlesEmptyConfig(t *testing.T) {
	startIdleRunners(context.Background(), nil, "测试拉起")
	startIdleRunners(context.Background(), cfgWith(t.TempDir()), "测试拉起")
}

// A runner whose directory does not exist (status=missing) should not be started,
// since that would create the directory out of nothing and, in container mode, make Docker create the mount source owned by root
func TestStartIdleRunners_SkipsMissingInstallDir(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	cfg := cfgWith(base, "never-created")
	startIdleRunners(context.Background(), cfg, "测试拉起")
	if _, err := os.Stat(filepath.Join(base, "never-created")); !os.IsNotExist(err) {
		t.Fatal("should not create a directory for a nonexistent runner")
	}
}

// Stay consistent with the criteria in runner.List: status and running state both come from the same place
func TestStartIdleRunners_MatchesListStatus(t *testing.T) {
	requireLinux(t)
	base := t.TempDir()
	dir := mkRunnerDir(t, base, "a", true)
	cfg := cfgWith(base, "a")

	list := runner.List(cfg)
	if len(list) != 1 || list[0].Status != runner.StatusInstalled || list[0].Running {
		t.Fatalf("precondition not met: should be installed and not running, got %+v", list)
	}
	startIdleRunners(context.Background(), cfg, "测试拉起")
	if !waitUntil(func() bool {
		l := runner.List(cfg)
		return len(l) == 1 && l[0].Running
	}) {
		t.Fatalf("after starting, List should report running, Find=%v", runnerproc.Find(dir))
	}
}

// writeBlockingRunScript writes a run.sh that blocks indefinitely and **spawns no child processes**.
//
// Do not use `sleep 120`: bash forks a child process to exec sleep, and between fork and execve,
// the child's /proc/<pid>/cmdline still shows bash's argv (`/bin/bash <dir>/run.sh`),
// so it is mistaken for a second supervisor script. The window is only a few microseconds and rarely hit locally, but on a busy CI machine it bites,
// 'after 10 /start calls there should be only 1 runner process, actually 2: [2727 2728]' is exactly how this came about.
//
// read is a builtin, and the redirection is handled by the current shell itself. Opening a FIFO with no writer blocks in place,
// so nothing forks and the process count is deterministic.
func writeBlockingRunScript(t *testing.T, dir string) {
	t.Helper()
	fifo := filepath.Join(dir, ".test-block")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("failed to create FIFO: %v", err)
	}
	script := "#!/bin/bash\nread -r -t 120 < \"" + fifo + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
