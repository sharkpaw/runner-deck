package runner

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

// Start launches run.sh, and run.sh launches the Job. In default mode the Runner is a child process of the Manager,
// so BASIC_AUTH_PASSWORD, which docker-compose.yml injects into the Manager via environment:, passes all the way
// down into the Job's environment, where any env / printenv step in the workflow or any
// third-party Action that prints environment variables will write the admin password into the Job log. It is not an Actions secret, so GitHub does not mask it.
//
// This test case targets exactly that path: it really launches run.sh, has it write the environment it receives to disk, and then checks what is in it.
func TestStart_DoesNotLeakCredentials(t *testing.T) {
	requireEnvDumpOS(t)
	t.Setenv("BASIC_AUTH_PASSWORD", "x")
	t.Setenv("DOCKER_HOST", "tcp://d:2375")

	dir := t.TempDir()
	writeEnvDumpingRunScript(t, dir)
	killRunnerProcsOnCleanup(t, dir)

	if err := Start(dir); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	dump := waitForEnvDump(t, dir)

	// Credentials should not be in there
	if v, ok := envDumpLookup(dump, "BASIC_AUTH_PASSWORD"); ok {
		t.Errorf("the run.sh environment contains BASIC_AUTH_PASSWORD=%q; an env step is enough to leak it into the Job log", v)
	}
	// All other variables are passed through as usual: DinD relies on the inherited DOCKER_HOST to find the daemon,
	// filtering it out too would silently stop Jobs in container mode from connecting to Docker.
	if v, ok := envDumpLookup(dump, "DOCKER_HOST"); !ok || v != "tcp://d:2375" {
		t.Errorf("DOCKER_HOST = %q (present: %v), expected to be passed through unchanged as tcp://d:2375", v, ok)
	}
}

// ---- The helpers below are shared with the same-named test case on the agent side; each side keeps its own copy ----

func requireEnvDumpOS(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test case depends on bash and a FIFO")
	}
}

// writeEnvDumpingRunScript writes a run.sh that first dumps its own environment to disk, then blocks indefinitely.
//
// The blocking part matches writeBlockingRunScript; see the comment there for why: read is a builtin,
// opening a FIFO with no writer blocks in place without forking, so the process count is deterministic.
// The only addition is the first line; env is the only thing this test case checks.
func writeEnvDumpingRunScript(t *testing.T, dir string) {
	t.Helper()
	fifo := filepath.Join(dir, ".test-block")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("failed to create FIFO: %v", err)
	}
	// cmd.Dir is the install directory, so $PWD equals dir. Write a temp file first and then mv it,
	// so that a poll cannot read a half-written file.
	script := "#!/bin/bash\nenv > \"$PWD/" + envDumpFile + ".tmp\"\n" +
		"mv \"$PWD/" + envDumpFile + ".tmp\" \"$PWD/" + envDumpFile + "\"\n" +
		"read -r -t 120 < \"" + fifo + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

const envDumpFile = ".env.out"

// killRunnerProcsOnCleanup kills run.sh when the test case ends.
// The script has a read -t 120 fallback, but the process stays alive for those 120 seconds, so do not rely on it.
func killRunnerProcsOnCleanup(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		for _, pid := range runnerproc.Find(dir) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// waitForEnvDump waits for run.sh to dump the environment. Start only forks; writing the file is the child's job,
// so the only option here is polling.
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
			t.Fatalf("gave up waiting for %s, run.sh may not have been launched: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// envDumpLookup looks up a variable's value by name in the output of env(1).
//
// Do not use strings.Contains(dump, "BASIC_AUTH_PASSWORD"): that would also match BASIC_AUTH_PASSWORD_FILE,
// so a variable that should have been kept would produce a false report; conversely, a variable whose value contains a newline would also
// scramble the whole output. Reading by line with the KEY= prefix is the only reliable approach here.
func envDumpLookup(dump, name string) (string, bool) {
	for _, line := range strings.Split(dump, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && k == name {
			return v, true
		}
	}
	return "", false
}
