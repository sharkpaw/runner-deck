package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

// wantStopArgs is the argument prefix that docker stop should get.
//
// Derived from runnerproc.StopGracePeriod instead of writing "stop -t 30": the grace period should only be defined in that one
// constant. If 30 were hardcoded, raising the constant would leave this test green, while the agent already waits for the Runner to exit
// using the new grace period, but docker would still SIGKILL after 30 seconds, so the Job gets cut off mid-run.
func wantStopArgs(runnerName string) string {
	secs := strconv.Itoa(int(runnerproc.StopGracePeriod / time.Second))
	return "stop -t " + secs + " " + ContainerName(runnerName)
}

// fakeDockerProgram puts a fake docker at the front of PATH; its behavior is entirely determined by body.
//
// Unlike fakeDocker in recreate_test.go, this one can fail: its exit code and output are both controllable,
// and nearly every branch on the stop/remove path (container missing, insufficient permissions, daemon unreachable)
// is only reached when docker returns non-zero. body is a sh script, so it can directly use case "$1" in.
func fakeDockerProgram(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + logPath + "\n" +
		body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Prepend only, don't replace: the script itself still needs cat, printf, etc.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readCalls(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---- StopRunnerContainer ----

func TestStopRunnerContainerSucceeds(t *testing.T) {
	logPath := fakeDockerProgram(t, "exit 0")
	if err := StopRunnerContainer(context.Background(), "demo"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	calls := readCalls(t, logPath)
	if !strings.Contains(calls, wantStopArgs("demo")) {
		t.Fatalf("Expected %q to be called, actual calls:\n%s", wantStopArgs("demo"), calls)
	}
}

// Stopping a container that no longer exists is not an error: the delete flow and repeated UI clicks both reach this,
// and reporting an error would only make the caller think they missed something.
func TestStopRunnerContainerTreatsMissingContainerAsDone(t *testing.T) {
	logPath := fakeDockerProgram(t, `
case "$1" in
  stop)    echo "Error response from daemon: No such container: x" >&2; exit 1 ;;
  inspect) echo "Error: No such object: x" >&2; exit 1 ;;
esac
exit 0`)
	if err := StopRunnerContainer(context.Background(), "demo"); err != nil {
		t.Fatalf("Container that doesn't exist should be treated as stopped, but returned: %v", err)
	}
	// Determining whether the container is really gone must use a separate inspect, not just the stop output
	if !strings.Contains(readCalls(t, logPath), "inspect") {
		t.Fatal("After stop fails, inspect again to check whether the container exists")
	}
}

// If the container is still running and cannot be stopped, that is a real failure and must not be swallowed
func TestStopRunnerContainerReportsRealFailure(t *testing.T) {
	fakeDockerProgram(t, `
case "$1" in
  stop)    echo "Error response from daemon: cannot stop container" >&2; exit 1 ;;
  inspect) echo "true" ;;
esac
exit 0`)
	err := StopRunnerContainer(context.Background(), "demo")
	if err == nil {
		t.Fatal("Stop failure should return an error while the container is still running")
	}
	if !strings.Contains(err.Error(), "cannot stop container") {
		t.Fatalf("The error should include docker's raw output, actual: %v", err)
	}
}

// Permission or daemon-unreachable errors are the most common failures in this deployment, so the error must directly state the remedy,
// otherwise the user only sees a bare "exit status 1"
func TestStopRunnerContainerPermissionErrorCarriesHint(t *testing.T) {
	fakeDockerProgram(t, `
case "$1" in
  stop)    echo "permission denied while trying to connect to the Docker daemon socket" >&2; exit 1 ;;
  inspect) echo "true" ;;
esac
exit 0`)
	err := StopRunnerContainer(context.Background(), "demo")
	if err == nil {
		t.Fatal("Expected an error to be returned")
	}
	if !strings.Contains(err.Error(), "DOCKER_GID") {
		t.Fatalf("Permission errors should include a hint about group_add / DOCKER_GID, actual: %v", err)
	}
}

// ---- RemoveRunnerContainer ----

func TestRemoveRunnerContainerStopsThenRemoves(t *testing.T) {
	logPath := fakeDockerProgram(t, "exit 0")
	if err := RemoveRunnerContainer(context.Background(), "demo"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	calls := readCalls(t, logPath)
	cn := ContainerName("demo")
	stopAt := strings.Index(calls, wantStopArgs("demo"))
	rmAt := strings.Index(calls, "rm -f "+cn)
	if stopAt < 0 || rmAt < 0 {
		t.Fatalf("Both stop and rm should be called, actual calls:\n%s", calls)
	}
	// Stop before removing: a direct rm -f would cut off a Job that is still running, whereas stop gives it 30 seconds to wrap up
	if stopAt > rmAt {
		t.Fatalf("stop should be called before rm, actual calls:\n%s", calls)
	}
}

// A stop failure must not block rm: the container may have already exited, or may no longer exist at all
func TestRemoveRunnerContainerIgnoresStopFailure(t *testing.T) {
	logPath := fakeDockerProgram(t, `
case "$1" in
  stop) echo "Error response from daemon: No such container" >&2; exit 1 ;;
  rm)   exit 0 ;;
esac
exit 0`)
	if err := RemoveRunnerContainer(context.Background(), "demo"); err != nil {
		t.Fatalf("A stop failure should not abort removal: %v", err)
	}
	if !strings.Contains(readCalls(t, logPath), "rm -f ") {
		t.Fatal("rm should still run after stop fails")
	}
}

func TestRemoveRunnerContainerTreatsMissingContainerAsDone(t *testing.T) {
	fakeDockerProgram(t, `
case "$1" in
  rm) echo "Error: No such container: x" >&2; exit 1 ;;
esac
exit 0`)
	if err := RemoveRunnerContainer(context.Background(), "demo"); err != nil {
		t.Fatalf("Container that doesn't exist should be treated as removed, but returned: %v", err)
	}
}

func TestRemoveRunnerContainerReportsRealFailure(t *testing.T) {
	fakeDockerProgram(t, `
case "$1" in
  rm) echo "Error response from daemon: device or resource busy" >&2; exit 1 ;;
esac
exit 0`)
	err := RemoveRunnerContainer(context.Background(), "demo")
	if err == nil {
		t.Fatal("rm should return an error on a real failure")
	}
	if !strings.Contains(err.Error(), "device or resource busy") {
		t.Fatalf("The error should include docker's raw output, actual: %v", err)
	}
}

// ---- ContainerState ----

func TestContainerStateExisting(t *testing.T) {
	fakeDockerProgram(t, `
case "$1" in
  inspect)
    cat <<'JSON'
[{"State":{"Status":"running","Running":true},"Config":{"Image":"img:tag"}}]
JSON
    ;;
esac
exit 0`)
	exists, status, err := ContainerState(context.Background(), "github-runner-demo")
	if err != nil {
		t.Fatalf("Should not report an error: %v", err)
	}
	if !exists || status != "running" {
		t.Fatalf("Got exists=%v status=%q, expected true/running", exists, status)
	}
}

// "Is this name already taken" is the main purpose of this function (the conflict precheck before adding a Runner),
// so a missing container is the normal answer, not an error
func TestContainerStateMissingIsNotAnError(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"inspect 报 No such object", `
case "$1" in
  inspect) echo "Error: No such object: x" >&2; exit 1 ;;
esac
exit 0`},
		{"inspect 返回空数组", `
case "$1" in
  inspect) echo "[]" ;;
esac
exit 0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeDockerProgram(t, tc.body)
			exists, status, err := ContainerState(context.Background(), "github-runner-demo")
			if err != nil {
				t.Fatalf("A missing container should not report an error: %v", err)
			}
			if exists || status != "" {
				t.Fatalf("got exists=%v status=%q, expected false/empty", exists, status)
			}
		})
	}
}

// Failing to reach the daemon must be told apart from "container does not exist":
// treating it as "name is free" lets the add flow run all the way to docker create before failing
func TestContainerStateDockerUnreachableIsAnError(t *testing.T) {
	fakeDockerProgram(t, `
case "$1" in
  inspect) echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock" >&2; exit 1 ;;
esac
exit 0`)
	exists, _, err := ContainerState(context.Background(), "github-runner-demo")
	if err == nil {
		t.Fatal("Should return an error when Docker is unavailable, not report \"container does not exist\"")
	}
	if exists {
		t.Fatal("exists should be false on error")
	}
}

// ---- Output classification (pure functions, but they decide which branch each path above takes) ----

func TestContainerNotFound(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Error response from daemon: No such container: abc", true},
		{"Error: No such object: abc", true},
		{"error: NO SUCH CONTAINER", true}, // Classification is case-insensitive
		{"Error response from daemon: 没有此容器", true},
		{"未找到容器 abc", true},
		{"没有找到容器 abc", true},
		{"", false},
		{"permission denied", false},
		// The container exists but cannot be stopped; it must not be silently treated as "does not exist" and succeed
		{"Error response from daemon: cannot stop container", false},
	}
	for _, tc := range cases {
		t.Run(tc.out, func(t *testing.T) {
			if got := containerNotFound([]byte(tc.out)); got != tc.want {
				t.Fatalf("containerNotFound(%q) = %v, expected %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestContainerStartUnrecoverable(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Error response from daemon: network runner-net not found", true},
		{"could not find network runner-net", true},
		{"could not attach to network runner-net", true},
		{"failed to create endpoint x on network y", true},
		{"failed to get network during CreateEndpoint", true},
		{"No such object: whatever", false},
		{"", false},
		// Port conflicts are recoverable; deleting and recreating the container cannot fix them, so don't misclassify
		{"Error starting userland proxy: address already in use", false},
	}
	for _, tc := range cases {
		t.Run(tc.out, func(t *testing.T) {
			if got := containerStartUnrecoverable([]byte(tc.out)); got != tc.want {
				t.Fatalf("containerStartUnrecoverable(%q) = %v, expected %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestManagerDockerHostIsDind(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"", false},
		{"unix:///var/run/docker.sock", false},
		{"tcp://runner-dind:2375", true},
		{"  tcp://runner-dind:2375  ", true}, // Extra spaces commonly found in .env files
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.env)
			if got := ManagerDockerHostIsDind(); got != tc.want {
				t.Fatalf("DOCKER_HOST=%q got %v, expected %v", tc.env, got, tc.want)
			}
		})
	}
}
