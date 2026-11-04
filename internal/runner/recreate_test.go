package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// fakeDocker puts a fake docker in front of PATH: it records every call and returns the given JSON for inspect.
// Returns the log file path, for asserting which docker commands were actually run.
func fakeDocker(t *testing.T, inspectJSON string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + logPath + "\n" +
		"case \"$1\" in\n" +
		"  inspect)\n" +
		"    cat <<'JSON'\n" + inspectJSON + "\nJSON\n" +
		"    ;;\n" +
		"  image)\n" +
		"    echo 'sha256:aaa'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	// Only put the fake docker first. Don't replace PATH entirely, because external commands like cat used by the script must still be found.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The sleep that waits for the Agent to be ready is pointless here. Set it to 0 so each case doesn't wait 5 seconds for nothing.
	origCreate, origStart := agentReadyDelayAfterCreate, agentReadyDelayAfterStart
	agentReadyDelayAfterCreate, agentReadyDelayAfterStart = 0, 0
	t.Cleanup(func() {
		agentReadyDelayAfterCreate, agentReadyDelayAfterStart = origCreate, origStart
	})
	return logPath
}

func dockerCalls(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

func driftTestConfig(t *testing.T, backend string) (*config.Config, string) {
	t.Helper()
	base := t.TempDir()
	installDir := filepath.Join(base, "a")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Drift only matters for a registered Runner. A directory without .runner has state new,
	// ContainerRunnerStatus honestly falls back to the on-disk state instead of installed
	if err := os.WriteFile(filepath.Join(installDir, ".runner"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Runners: config.RunnersConfig{
		BasePath:         base,
		ContainerMode:    true,
		ContainerImage:   "img:tag",
		ContainerNetwork: "runner-net",
		JobDockerBackend: backend,
		AgentPort:        8081,
	}}, installDir
}

// inspectJSON builds a container that is stopped and was created with host-socket
func stoppedHostSocketInspect(mountSrc string) string {
	return `[{
	  "Image": "sha256:aaa",
	  "State": {"Status": "exited", "Running": false},
	  "Config": {
	    "Image": "img:tag",
	    "Env": ["DOCKER_HOST=unix:///var/run/docker.sock", "AGENT_TOKEN=already-injected"],
	    "Labels": {
      "io.runner-deck.job-docker-backend": "host-socket",
      "io.runner-deck.network": "runner-net"
    }
	  },
	  "HostConfig": {
	    "Binds": ["` + mountSrc + `:/runner", "/var/run/docker.sock:/var/run/docker.sock"],
	    "GroupAdd": ["999"],
	    "NetworkMode": "runner-net"
	  },
	  "NetworkSettings": {"Networks": {"runner-net": {}}}
	}]`
}

// TestStartRunnerContainer_RecreatesStoppedContainerOnDrift
// The container was created with host-socket, but the config has changed to dind. On start it must be removed and recreated,
// otherwise it would come back still holding the host socket, as if the switch never happened.
func TestStartRunnerContainer_RecreatesStoppedContainerOnDrift(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "dind")
	logPath := fakeDocker(t, stoppedHostSocketInspect(installDir))

	// The Agent call is bound to fail (the container name can't be resolved). Here we only care which docker commands were run.
	_ = StartRunnerContainer(context.Background(), cfg, "a", installDir)

	calls := dockerCalls(t, logPath)
	if !strings.Contains(calls, "rm -f github-runner-a") {
		t.Fatalf("On drift the old container should be removed first; actual calls:\n%s", calls)
	}
	if !strings.Contains(calls, "create --name github-runner-a") {
		t.Fatalf("On drift the container should be recreated; actual calls:\n%s", calls)
	}
	if !strings.Contains(calls, "DOCKER_HOST=tcp://runner-dind:2375") {
		t.Fatalf("Rebuild should inject DOCKER_HOST for the new backend; actual calls:\n%s", calls)
	}
}

// TestStartRunnerContainer_StartsStoppedContainerWhenInSync If the config is unchanged, just start it. Don't rebuild for nothing.
func TestStartRunnerContainer_StartsStoppedContainerWhenInSync(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "host-socket")
	cfg.Runners.DockerGID = 999
	logPath := fakeDocker(t, stoppedHostSocketInspect(installDir))

	_ = StartRunnerContainer(context.Background(), cfg, "a", installDir)

	calls := dockerCalls(t, logPath)
	if !strings.Contains(calls, "start github-runner-a") {
		t.Fatalf("When the config matches, it should start directly; actual calls:\n%s", calls)
	}
	if strings.Contains(calls, "rm -f") || strings.Contains(calls, "create --name") {
		t.Fatalf("When the config matches, the container should not be recreated; actual calls:\n%s", calls)
	}
}

// TestStartRunnerContainer_RunningContainerIsNotRecreated
// A running container must not be touched even if it has drifted. A Job is likely running in it above, and deleting it would cut that job off midway.
func TestStartRunnerContainer_RunningContainerIsNotRecreated(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "dind")
	running := strings.Replace(stoppedHostSocketInspect(installDir),
		`"State": {"Status": "exited", "Running": false}`,
		`"State": {"Status": "running", "Running": true}`, 1)
	logPath := fakeDocker(t, running)

	_ = StartRunnerContainer(context.Background(), cfg, "a", installDir)

	calls := dockerCalls(t, logPath)
	if strings.Contains(calls, "rm -f") || strings.Contains(calls, "create --name") {
		t.Fatalf("A running container should not be rebuilt automatically; actual calls:\n%s", calls)
	}
}

// TestRecreateRunnerContainer_ForcesRebuildOfRunningContainer
// Interrupting is only allowed when "rebuild" is explicitly clicked. Even while running, remove and recreate.
func TestRecreateRunnerContainer_ForcesRebuildOfRunningContainer(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "host-socket")
	cfg.Runners.DockerGID = 999
	running := strings.Replace(stoppedHostSocketInspect(installDir),
		`"State": {"Status": "exited", "Running": false}`,
		`"State": {"Status": "running", "Running": true}`, 1)
	logPath := fakeDocker(t, running)

	// Config matches the container exactly; a forced rebuild should still run
	_ = RecreateRunnerContainer(context.Background(), cfg, "a", installDir)

	calls := dockerCalls(t, logPath)
	if !strings.Contains(calls, "rm -f github-runner-a") {
		t.Fatalf("A forced rebuild should remove the old container; actual calls:\n%s", calls)
	}
	if !strings.Contains(calls, "create --name github-runner-a") {
		t.Fatalf("A forced rebuild should create a new container; actual calls:\n%s", calls)
	}
}

// TestContainerRunnerStatus_ReportsDrift The status query also returns a drift explanation, which the UI uses for its hint
func TestContainerRunnerStatus_ReportsDrift(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "dind")
	fakeDocker(t, stoppedHostSocketInspect(installDir))

	running, status, drift, err := ContainerRunnerStatus(context.Background(), cfg, "a", installDir)
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("running should be false when the container is not running")
	}
	if status != StatusInstalled {
		t.Fatalf("status = %v, want installed", status)
	}
	if !strings.Contains(drift, "dind") {
		t.Fatalf("drift = %q, should point out that the backend has changed to dind", drift)
	}
}

// TestLockRunnerOps_SerializesSameRunner Start and stop operations on the same Runner must be serialized:
// A rebuild runs docker rm and then create. If two paths interleave, one may delete the container the other just created.
func TestLockRunnerOps_SerializesSameRunner(t *testing.T) {
	unlock := lockRunnerOps("github-runner-a")

	acquired := make(chan struct{})
	go func() {
		defer lockRunnerOps("github-runner-a")()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("The second operation on the same-named Runner should not acquire the lock while it is held")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("The second operation still did not acquire the lock after release")
	}
}

// TestLockRunnerOps_DifferentRunnersRunInParallel Different Runners should not queue behind each other
func TestLockRunnerOps_DifferentRunnersRunInParallel(t *testing.T) {
	defer lockRunnerOps("github-runner-a")()

	done := make(chan struct{})
	go func() {
		defer lockRunnerOps("github-runner-b")()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Different Runners were needlessly serialized")
	}
}

// TestImageIDCache_StatusPathCachesStartPathDoesNot
// The list page compares the image once per Runner, so a cache avoids repeated docker image inspect calls;
// but the start/stop path must read live values. Right after a build, clicking "start" with a stale ID would skip the rebuild.
func TestImageIDCache_StatusPathCachesStartPathDoesNot(t *testing.T) {
	logPath := fakeDocker(t, `[]`)
	resetImageIDCache()
	t.Cleanup(resetImageIDCache)

	countInspects := func() int {
		return strings.Count(dockerCalls(t, logPath), "image inspect -f {{.Id}} img:cache-test")
	}

	cachedImageID(context.Background(), "img:cache-test")
	cachedImageID(context.Background(), "img:cache-test")
	if got := countInspects(); got != 1 {
		t.Fatalf("The cached query ran docker image inspect %d times; it should be 1", got)
	}

	resolveImageID(context.Background(), "img:cache-test")
	resolveImageID(context.Background(), "img:cache-test")
	if got := countInspects(); got != 3 {
		t.Fatalf("Live queries should run every time, totaling 3; actual %d", got)
	}
}

// TestStartRunnerContainer_RecreatesContainerWithoutAgentToken
// Containers created before this feature have no AGENT_TOKEN, and the Agent does not authenticate. Any other container on the same network can control this Runner.
// On startup it should be rebuilt automatically to fix this, rather than waiting for someone to run docker rm by hand.
func TestStartRunnerContainer_RecreatesContainerWithoutAgentToken(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "host-socket")
	cfg.Runners.DockerGID = 999
	legacy := strings.Replace(stoppedHostSocketInspect(installDir),
		`"AGENT_TOKEN=already-injected"`, `"LANG=C.UTF-8"`, 1)
	logPath := fakeDocker(t, legacy)

	_ = StartRunnerContainer(context.Background(), cfg, "a", installDir)

	calls := dockerCalls(t, logPath)
	if !strings.Contains(calls, "rm -f github-runner-a") || !strings.Contains(calls, "create --name github-runner-a") {
		t.Fatalf("An old container without AGENT_TOKEN should be rebuilt; actual calls:\n%s", calls)
	}
	if !strings.Contains(calls, "-e AGENT_TOKEN=") {
		t.Fatalf("Rebuild should inject AGENT_TOKEN; actual calls:\n%s", calls)
	}
}

// TestContainerRunnerStatus_RunningPreTokenContainerReportsDrift
// The most dangerous kind of container during an upgrade: created before this feature, and **currently running**.
// Its directory has no .agent_token, and both auto-start paths only handle containers that aren't running (they start only when info.Running is false),
// so nobody ever generates the token. The old status path called ReadAgentToken directly, and when it read an empty value it skipped the whole check.
// The UI never shows "config changed", so the user has no reason to click "rebuild container", and the container keeps running without auth.
// The status path must Ensure the token itself before it can detect this.
func TestContainerRunnerStatus_RunningPreTokenContainerReportsDrift(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "host-socket")
	cfg.Runners.DockerGID = 999

	// Precondition: the directory has no token file, as it did before the upgrade
	tokenPath := filepath.Join(installDir, AgentTokenFile)
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatal("test precondition violated: no token file should exist yet")
	}

	running := strings.Replace(stoppedHostSocketInspect(installDir),
		`"State": {"Status": "exited", "Running": false}`,
		`"State": {"Status": "running", "Running": true}`, 1)
	// The old container was never injected with AGENT_TOKEN
	running = strings.Replace(running, `, "AGENT_TOKEN=already-injected"`, "", 1)
	fakeDocker(t, running)

	// The agent probe always fails (the container name doesn't resolve); here we only care about drift and the token file
	_, _, drift, _ := ContainerRunnerStatus(context.Background(), cfg, "a", installDir)

	if !strings.Contains(drift, "agent_token") {
		t.Fatalf("running old container missing AGENT_TOKEN was not detected, drift=%q", drift)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("status query should also generate the token, otherwise the next read is still empty: %v", err)
	}
}

// TestContainerRunnerStatus_TokenlessDirStaysQuiet
// Negative case: when the token truly can't be generated (simulated here with a nonexistent directory, which can't be written to), stay silent.
// Otherwise "report drift -> rebuild -> still no token to inject -> report drift again" becomes a container delete-and-rebuild on every startup.
func TestContainerRunnerStatus_TokenlessDirStaysQuiet(t *testing.T) {
	cfg, installDir := driftTestConfig(t, "host-socket")
	cfg.Runners.DockerGID = 999
	missingDir := filepath.Join(installDir, "does-not-exist")

	running := strings.Replace(stoppedHostSocketInspect(missingDir),
		`"State": {"Status": "exited", "Running": false}`,
		`"State": {"Status": "running", "Running": true}`, 1)
	running = strings.Replace(running, `, "AGENT_TOKEN=already-injected"`, "", 1)
	fakeDocker(t, running)

	_, _, drift, _ := ContainerRunnerStatus(context.Background(), cfg, "a", missingDir)

	if strings.Contains(drift, "agent_token") {
		t.Fatalf("agent_token drift must not be reported when the token can't be generated (it would cause repeated rebuilds), drift=%q", drift)
	}
}
