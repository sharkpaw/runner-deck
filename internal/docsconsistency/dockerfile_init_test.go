package docsconsistency

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initBinary is the program PID 1 should be in both images.
// Path comes from the tini package in noble/universe (tini_0.19.0-1, installs /usr/bin/tini and /usr/bin/tini-static).
const initBinary = "/usr/bin/tini"

// TestDockerfiles_EntrypointRunsUnderInit requires both images' ENTRYPOINT to start with init.
//
// Why this deserves a test: this is a change that still builds and still runs if you delete it.
// Without tini in the image, Agent and Manager go back to being PID 1 in the container, so
//   - orphaned processes left by Jobs are never reaped, zombies pile up across Jobs and take up
//     slots in runners.resources.pids_limit, and once that limit is hit fork fails,
//     so Jobs randomly fail with Resource temporarily unavailable;
//   - SIGTERM is no longer forwarded. That half is now covered by Agent / Manager subscribing to signals themselves,
//     so removing tini won't turn any existing test red. Reaping orphans has no other safety net.
//
// Any change that puts ENTRYPOINT back by hand must first turn this test red.
func TestDockerfiles_EntrypointRunsUnderInit(t *testing.T) {
	repo := repoRoot(t)

	for _, name := range []string{"Dockerfile", "Dockerfile.runner"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(repo, name)
			argv := entrypointArgv(t, path)
			if len(argv) == 0 {
				t.Fatalf("no exec-form (JSON array) ENTRYPOINT found in %s", name)
			}
			if argv[0] != initBinary {
				t.Fatalf("first item of ENTRYPOINT in %s should be %s, got %q (full argv: %v)",
					name, initBinary, argv[0], argv)
			}
			// tini and the program it launches need -- between them, or tini will consume the program's arguments itself
			if len(argv) < 3 || argv[1] != "--" {
				t.Fatalf("ENTRYPOINT in %s should be [%q, \"--\", <program>], got %v", name, initBinary, argv)
			}
		})
	}
}

// entrypointArgv returns the argv of the last exec-form ENTRYPOINT in the Dockerfile.
//
// Only the JSON array form is accepted: shell form (ENTRYPOINT foo bar) gets wrapped in /bin/sh -c,
// and sh becomes PID 1, which neither forwards signals nor reaps orphans. That is exactly the case this test guards against,
// so when no array can be parsed, make the caller fail rather than let it pass here.
func entrypointArgv(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	// Join continuation lines (ending in \) first, so an ENTRYPOINT split across lines is still seen
	lines := strings.Split(strings.ReplaceAll(string(b), "\\\n", " "), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "ENTRYPOINT") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "ENTRYPOINT"))
		var parsed []string
		if err := json.Unmarshal([]byte(rest), &parsed); err != nil {
			t.Fatalf("ENTRYPOINT in %s is not exec form (JSON array): %s", filepath.Base(path), rest)
		}
		argv = parsed
	}
	return argv
}
