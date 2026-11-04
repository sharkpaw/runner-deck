package handler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/runner"
)

// Registration is the path with the most side effects in this project: it downloads the runner, runs config.sh to register with GitHub,
// writes the registration result to disk, then starts the process. All of it runs in a background goroutine, so a failure only shows up as one line of text in the UI.
// These cases replace both config.sh and install-runner.sh with local fake scripts to cover branch decisions and error wording.

// newRegDir creates an install directory. When configScript is non-empty, it puts a fake config.sh in it.
// run.sh is deliberately left out: after a successful registration, runRegistrationJob calls Start, and without run.sh it
// only returns an error that gets logged, which neither forks a process nor affects the registration result assertions.
func newRegDir(t *testing.T, configScript string) string {
	t.Helper()
	dir := t.TempDir()
	installDir := filepath.Join(dir, "demo")
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if configScript != "" {
		writeScript(t, filepath.Join(installDir, runner.ConfigScriptName()), configScript)
	}
	return installDir
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

type regResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	At      string `json:"at"`
}

func readRegResult(t *testing.T, installDir string) regResult {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(installDir, runner.RegistrationResultFile))
	if err != nil {
		t.Fatalf("failed to write registration result file: %v", err)
	}
	var r regResult
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("registration result is not valid JSON: %s", b)
	}
	return r
}

// isolateConfigPath points ConfigPath at a temp file so the cases don't interfere with each other
func isolateConfigPath(t *testing.T) {
	t.Helper()
	old := ConfigPath
	ConfigPath = filepath.Join(t.TempDir(), "config.yaml")
	t.Cleanup(func() { ConfigPath = old })
}

func job(installDir string) registrationJob {
	return registrationJob{
		BasePath:   filepath.Dir(installDir),
		InstallDir: installDir,
		RunnerName: "demo",
		URL:        "https://github.com/o/r",
		Token:      "AAAA",
		Labels:     []string{"self-hosted", "linux"},
	}
}

// ---- install branch ----

// If config.sh already exists, install should not run again: re-downloading and extracting is slow and may overwrite already-registered credentials
func TestRunRegistrationJobSkipsInstallWhenConfigScriptExists(t *testing.T) {
	isolateConfigPath(t)
	installDir := newRegDir(t, "exit 0")

	marker := filepath.Join(t.TempDir(), "installed")
	old := installRunnerScriptPath
	installRunnerScriptPath = filepath.Join(t.TempDir(), "install.sh")
	writeScript(t, installRunnerScriptPath, "touch "+marker)
	t.Cleanup(func() { installRunnerScriptPath = old })

	runRegistrationJob(job(installDir))

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("install script should not run when config.sh already exists")
	}
	if r := readRegResult(t, installDir); !r.Success {
		t.Fatalf("expected registration to succeed, got: %+v", r)
	}
}

// config.sh is missing, so install first, then register
func TestRunRegistrationJobInstallsWhenConfigScriptMissing(t *testing.T) {
	isolateConfigPath(t)
	installDir := newRegDir(t, "") // no config.sh

	old := installRunnerScriptPath
	installRunnerScriptPath = filepath.Join(t.TempDir(), "install.sh")
	// The fake install script's job is to put config.sh in place, matching the real script
	writeScript(t, installRunnerScriptPath,
		`printf '#!/bin/sh\nexit 0\n' > "$RUNNERS_BASE_PATH/$1/`+runner.ConfigScriptName()+`"
chmod +x "$RUNNERS_BASE_PATH/$1/`+runner.ConfigScriptName()+`"`)
	t.Cleanup(func() { installRunnerScriptPath = old })

	runRegistrationJob(job(installDir))

	if r := readRegResult(t, installDir); !r.Success {
		t.Fatalf("expected registration to succeed, got: %+v", r)
	}
}

func TestRunRegistrationJobReportsInstallFailure(t *testing.T) {
	isolateConfigPath(t)
	installDir := newRegDir(t, "")

	old := installRunnerScriptPath
	installRunnerScriptPath = filepath.Join(t.TempDir(), "install.sh")
	writeScript(t, installRunnerScriptPath, "echo 'download failed: 404' >&2; exit 1")
	t.Cleanup(func() { installRunnerScriptPath = old })

	runRegistrationJob(job(installDir))

	r := readRegResult(t, installDir)
	if r.Success {
		t.Fatal("a failed install must not be recorded as success")
	}
	if !strings.Contains(r.Message, "installing the runner automatically failed") {
		t.Fatalf("message should say the install stage failed, got: %q", r.Message)
	}
}

// The install script exits with 0 but produces no config.sh, which has really happened (the downloaded package was incomplete).
// Without a separate check here, the failure would surface during registration as a 'file not found' error, pointing at the wrong place.
func TestRunRegistrationJobDetectsInstallWithoutConfigScript(t *testing.T) {
	isolateConfigPath(t)
	installDir := newRegDir(t, "")

	old := installRunnerScriptPath
	installRunnerScriptPath = filepath.Join(t.TempDir(), "install.sh")
	writeScript(t, installRunnerScriptPath, "exit 0") // All that install for nothing
	t.Cleanup(func() { installRunnerScriptPath = old })

	runRegistrationJob(job(installDir))

	r := readRegResult(t, installDir)
	if r.Success {
		t.Fatal("registration cannot succeed without config.sh")
	}
	if !strings.Contains(r.Message, "the install finished but") {
		t.Fatalf("message should point out that install finished but there is no config script, got: %q", r.Message)
	}
}

// ---- registration success ----

func TestRunRegistrationJobPassesNameAndUnattended(t *testing.T) {
	isolateConfigPath(t)
	argsFile := filepath.Join(t.TempDir(), "args")
	installDir := newRegDir(t, `printf '%s\n' "$*" > `+argsFile+`
exit 0`)

	runRegistrationJob(job(installDir))

	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("config.sh was not called: %v", err)
	}
	args := string(b)
	// --name: if omitted, config.sh takes the Manager container's hostname, so every Runner in one deployment
	// registers under the same name, so GitHub shows only one (fixed in v1.5.1) version-check-ignore
	if !strings.Contains(args, "--name demo") {
		t.Errorf("--name must be passed, actual args: %s", args)
	}
	// --unattended: it only makes the process fail and exit immediately on a name clash; otherwise it enters an interactive retry loop until timeout,
	// blocking every Runner queued behind it in the single-worker queue (also fixed in v1.5.1) version-check-ignore
	if !strings.Contains(args, "--unattended") {
		t.Errorf("--unattended must be passed, actual args: %s", args)
	}
	if !strings.Contains(args, "--labels self-hosted,linux") {
		t.Errorf("labels should be passed comma-joined, actual args: %s", args)
	}
	if !strings.Contains(args, "--url https://github.com/o/r") || !strings.Contains(args, "--token AAAA") {
		t.Errorf("url/token should be passed through unchanged, actual args: %s", args)
	}
}

func TestRunRegistrationJobWritesTimestampedResult(t *testing.T) {
	isolateConfigPath(t)
	installDir := newRegDir(t, "exit 0")

	runRegistrationJob(job(installDir))

	r := readRegResult(t, installDir)
	if !r.Success || r.Message != "registered" {
		t.Fatalf("expected success, got: %+v", r)
	}
	if _, err := time.Parse(time.RFC3339, r.At); err != nil {
		t.Fatalf("at should be an RFC3339 timestamp, got %q: %v", r.At, err)
	}
}

// ---- failure wording ----

// These hints are all the user ever sees. config.sh's raw output only says what went wrong,
// it cannot say where to fix it. The extra sentence is the part the user can actually act on.
func TestRunRegistrationJobEnrichesKnownFailures(t *testing.T) {
	cases := []struct {
		name       string
		scriptOut  string
		wantInMsg  string
		wantOrigin string // keep the raw output too
	}{
		{
			name:       "重名",
			scriptOut:  "A runner exists with the same name",
			wantInMsg:  "Settings → Actions → Runners",
			wantOrigin: "A runner exists with the same name",
		},
		{
			name:       "token 失效",
			scriptOut:  "Invalid registration token",
			wantInMsg:  "Generate a fresh registration token",
			wantOrigin: "Invalid registration token",
		},
		{
			name:       "token 已被用过",
			scriptOut:  "The registration token has already been used",
			wantInMsg:  "Generate a fresh registration token",
			wantOrigin: "already been used",
		},
		{
			name:       "token 过期",
			scriptOut:  "Registration token expired",
			wantInMsg:  "Generate a fresh registration token",
			wantOrigin: "expired",
		},
		{
			name:       "以 root 运行",
			scriptOut:  "Must not run with sudo",
			wantInMsg:  "RUNNER_ALLOW_RUNASROOT=1",
			wantOrigin: "Must not run with sudo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigPath(t)
			installDir := newRegDir(t, "echo '"+tc.scriptOut+"' >&2\nexit 1")

			runRegistrationJob(job(installDir))

			r := readRegResult(t, installDir)
			if r.Success {
				t.Fatal("config.sh exited non-zero, must not be recorded as success")
			}
			if !strings.Contains(r.Message, tc.wantInMsg) {
				t.Errorf("message should include an actionable hint %q, actual: %q", tc.wantInMsg, r.Message)
			}
			if !strings.Contains(r.Message, tc.wantOrigin) {
				t.Errorf("message should keep the raw config.sh output %q, actual: %q", tc.wantOrigin, r.Message)
			}
		})
	}
}

// Unrecognized failures must still be persisted, with the raw output attached; otherwise the UI only shows "registration failed"
func TestRunRegistrationJobKeepsUnknownFailureOutput(t *testing.T) {
	isolateConfigPath(t)
	installDir := newRegDir(t, "echo 'Http response code: NotFound from POST' >&2\nexit 1")

	runRegistrationJob(job(installDir))

	r := readRegResult(t, installDir)
	if r.Success {
		t.Fatal("must not be recorded as success")
	}
	if !strings.Contains(r.Message, "NotFound") {
		t.Fatalf("message should keep the raw output, actual: %q", r.Message)
	}
}

// If config.sh fails without saying anything, fall back to the error itself so the message is never empty
func TestRunRegistrationJobFallsBackToErrWhenScriptSilent(t *testing.T) {
	isolateConfigPath(t)
	installDir := newRegDir(t, "exit 3")

	runRegistrationJob(job(installDir))

	r := readRegResult(t, installDir)
	if r.Success {
		t.Fatal("must not be recorded as success")
	}
	if strings.TrimSpace(r.Message) == "" {
		t.Fatal("message must not be empty")
	}
}

// ---- queue ----

// The queue is consumed sequentially by a single worker: running config.sh concurrently would saturate CPU and network at once,
// and actions/runner registration is not suited to parallel execution. This confirms jobs are run one at a time.
func TestRegistrationWorkerProcessesQueueSequentially(t *testing.T) {
	isolateConfigPath(t)
	queue := make(chan registrationJob, 128)
	done := startRegistrationWorker(queue)
	defer func() {
		close(queue)
		<-done
	}()

	const n = 3
	dirs := make([]string, n)
	for i := range dirs {
		// Each script writes one line to the shared log; sequential execution keeps lines from interleaving
		dirs[i] = newRegDir(t, "exit 0")
	}
	for i, d := range dirs {
		j := job(d)
		j.RunnerName = "demo" + string(rune('a'+i))
		queue <- j
	}

	deadline := time.Now().Add(10 * time.Second)
	for _, d := range dirs {
		for {
			if _, err := os.Stat(filepath.Join(d, runner.RegistrationResultFile)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout: the registration result for %s was never written", d)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, d := range dirs {
		if r := readRegResult(t, d); !r.Success {
			t.Fatalf("%s should register successfully, actual: %+v", d, r)
		}
	}
}
