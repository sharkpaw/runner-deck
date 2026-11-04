package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

// In container mode, the Agent's AGENT_TOKEN is injected by the Manager via -e (see containerSpec.createArgs),
// and since the Agent passes its entire environment through unchanged when it starts run.sh, the token ends up in the Job's environment.
// It can only control this Runner itself, so its impact is smaller than the Basic Auth password, but an env step can still print it to the log.
//
// Note that this filters only the copy passed to the child process: the Agent itself calls os.Getenv("AGENT_TOKEN") on every request
// to authenticate (expectedToken), so Unsetenv in this process would turn auth off entirely.
func TestStartRunner_DoesNotLeakAgentToken(t *testing.T) {
	requireEnvDumpOS(t)
	t.Setenv("AGENT_TOKEN", "super-secret")
	t.Setenv("DOCKER_HOST", "tcp://d:2375")

	dir := t.TempDir()
	writeEnvDumpingRunScript(t, dir)
	killRunnerProcsOnCleanup(t, dir)

	if err := startRunner(dir); err != nil {
		t.Fatalf("startRunner failed: %v", err)
	}
	dump := waitForEnvDump(t, dir)

	if v, ok := envDumpLookup(dump, "AGENT_TOKEN"); ok {
		t.Errorf("run.sh environment has AGENT_TOKEN=%q; a Job env step can print it to the log", v)
	}
	// The Agent must still be able to read it, or /start and /stop would no longer authenticate
	if got := expectedToken(); got != "super-secret" {
		t.Errorf("expectedToken() = %q; the Agent process's own token should not have been modified", got)
	}
	if v, ok := envDumpLookup(dump, "DOCKER_HOST"); !ok || v != "tcp://d:2375" {
		t.Errorf("DOCKER_HOST = %q (present: %v), expected to pass through unchanged as tcp://d:2375", v, ok)
	}
}

// ---- Same approach as the identically named test case in internal/runner; each place keeps its own copy ----

func requireEnvDumpOS(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test case depends on bash and FIFO")
	}
}

// writeEnvDumpingRunScript writes a run.sh that first dumps its own environment to disk, then blocks indefinitely.
// The blocking approach matches writeBlockingRunScript; see the comments there for the reasoning.
func writeEnvDumpingRunScript(t *testing.T, dir string) {
	t.Helper()
	fifo := filepath.Join(dir, ".test-block")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("failed to create FIFO: %v", err)
	}
	script := "#!/bin/bash\nenv > \"$PWD/" + envDumpFile + ".tmp\"\n" +
		"mv \"$PWD/" + envDumpFile + ".tmp\" \"$PWD/" + envDumpFile + "\"\n" +
		"read -r -t 120 < \"" + fifo + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

const envDumpFile = ".env.out"

func killRunnerProcsOnCleanup(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		for _, pid := range runnerproc.Find(dir) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func waitForEnvDump(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, envDumpFile)
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not get %s; run.sh may not have been started: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// envDumpLookup looks up a value by variable name in env(1) output, matching KEY= line by line.
// Don't use strings.Contains: that would also match XAGENT_TOKEN.
func envDumpLookup(dump, name string) (string, bool) {
	for _, line := range strings.Split(dump, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && k == name {
			return v, true
		}
	}
	return "", false
}
