package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/runner"
)

// fakeConfigScript writes a fake config script under installDir that logs each received argument on its own line to args.txt
func fakeConfigScript(t *testing.T) (installDir, argsFile string) {
	t.Helper()
	installDir = t.TempDir()
	argsFile = filepath.Join(installDir, "args.txt")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + argsFile + "; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(installDir, runner.ConfigScriptName()), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return installDir, argsFile
}

func recordedArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake config script did not record the arguments: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func hasFlagValue(args []string, flag, want string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == want {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestRunConfigScript_PassesRunnerName
// Without --name, config.sh uses the host's hostname as the runner name, and it runs inside the manager container:
// every runner in a deployment registers with the same name (the manager container's hostname),
// GitHub ends up showing only one runner, named with a container ID that does not match the name shown in the UI.
func TestRunConfigScript_PassesRunnerName(t *testing.T) {
	installDir, argsFile := fakeConfigScript(t)

	if _, err := runConfigScript(installDir, "https://github.com/o/r", "tok", "droiddesk-2", nil, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	args := recordedArgs(t, argsFile)
	if !hasFlagValue(args, "--name", "droiddesk-2") {
		t.Fatalf("registration args have no --name droiddesk-2, got: %v", args)
	}
}

// TestRunConfigScript_PassesUnattended
// On a name clash, --unattended makes it fail and exit immediately; without it, it enters an interactive retry loop
// ("Failed to replace the runner. Try again or ctrl-c to quit"). There is no TTY here,
// that loop keeps running until the 2-minute timeout, and since registration runs sequentially on a single worker, every runner queued behind it is blocked.
func TestRunConfigScript_PassesUnattended(t *testing.T) {
	installDir, argsFile := fakeConfigScript(t)

	if _, err := runConfigScript(installDir, "https://github.com/o/r", "tok", "a", nil, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	if args := recordedArgs(t, argsFile); !hasFlag(args, "--unattended") {
		t.Fatalf("registration args have no --unattended, so a name clash would hang on an interactive prompt, got: %v", args)
	}
}

// TestRunConfigScript_KeepsUrlTokenAndLabels ensures existing args are not dropped when new ones are added
func TestRunConfigScript_KeepsUrlTokenAndLabels(t *testing.T) {
	installDir, argsFile := fakeConfigScript(t)

	if _, err := runConfigScript(installDir, "https://github.com/o/r", "tok", "a", []string{"x", "y"}, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	args := recordedArgs(t, argsFile)
	for _, c := range [][2]string{{"--url", "https://github.com/o/r"}, {"--token", "tok"}, {"--labels", "x,y"}} {
		if !hasFlagValue(args, c[0], c[1]) {
			t.Fatalf("registration args are missing %s %s, got: %v", c[0], c[1], args)
		}
	}
}

// TestRunConfigScript_OmitsEmptyName ensures an empty name does not pass an empty --name (config.sh would treat it as a missing argument and fail)
func TestRunConfigScript_OmitsEmptyName(t *testing.T) {
	installDir, argsFile := fakeConfigScript(t)

	if _, err := runConfigScript(installDir, "https://github.com/o/r", "tok", "", nil, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	if args := recordedArgs(t, argsFile); hasFlag(args, "--name") {
		t.Fatalf("--name should not be passed when the name is empty, got: %v", args)
	}
}
