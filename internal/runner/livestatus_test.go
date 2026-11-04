package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// fakeAgent starts a local Agent and points agentBaseURL at it.
// The container name cannot be resolved from the test process. Without swapping the base URL, only the 'unreachable' path can be verified.
func fakeAgent(t *testing.T, status string, running bool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(AgentStatus{Status: status, Running: running})
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	orig := agentBaseURL
	agentBaseURL = func(string, int) string { return "http://" + u.Host }
	t.Cleanup(func() { agentBaseURL = orig })
}

// liveStatusConfig builds a container-mode config and sets up the runner directory as 'registered'
func liveStatusConfig(t *testing.T) *config.Config {
	t.Helper()
	base := t.TempDir()
	installDir := filepath.Join(base, "droiddesk-2")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".runner"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Runners: config.RunnersConfig{
		BasePath:         base,
		ContainerMode:    true,
		ContainerImage:   "img:tag",
		ContainerNetwork: "runner-net",
		JobDockerBackend: "host-socket",
		AgentPort:        8081,
		Items:            []config.RunnerItem{{Name: "droiddesk-2", Path: "droiddesk-2"}},
	}}
}

func runningHostSocketInspect(mountSrc string) string {
	return `[{
	  "Image": "sha256:aaa",
	  "State": {"Status": "running", "Running": true},
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

// This is the root-cause regression case for 'restarting every runner every 5 minutes'.
//
// In container mode, Manager and Runner are not in the same PID namespace, so List scans its own /proc
// It inevitably cannot find the Runner process, so Running is always false, and the background check's criterion
// (Status == installed && !Running) therefore holds in every round.
// ListWithLiveStatus must ask the Agent inside the container to get the truth.
func TestListWithLiveStatus_RunningContainerIsNotReportedIdle(t *testing.T) {
	cfg := liveStatusConfig(t)
	installDir := cfg.Runners.Items[0].InstallPath(cfg.Runners.BasePath)
	fakeDocker(t, runningHostSocketInspect(installDir))
	fakeAgent(t, "installed", true)

	// List reflects the disk view: in container mode it cannot see processes in other namespaces
	if plain := List(cfg); len(plain) != 1 || plain[0].Running {
		t.Fatalf("Premise does not hold: in container mode List's Running should be false, got %+v", plain)
	}

	live := ListWithLiveStatus(context.Background(), cfg)
	if len(live) != 1 {
		t.Fatalf("should return 1 item, got %d", len(live))
	}
	if !live[0].Running {
		t.Fatal("Agent reports running=true, but ListWithLiveStatus still reports not running - the scheduled check would restart the running runner again")
	}
	if live[0].Status != StatusInstalled {
		t.Fatalf("status should be %q, got %q", StatusInstalled, live[0].Status)
	}
}

// If the Agent says it is not running, it should be started. The fix must not also block the 'should start' case.
func TestListWithLiveStatus_StoppedRunnerStaysStartable(t *testing.T) {
	cfg := liveStatusConfig(t)
	installDir := cfg.Runners.Items[0].InstallPath(cfg.Runners.BasePath)
	fakeDocker(t, runningHostSocketInspect(installDir))
	fakeAgent(t, "installed", false)

	live := ListWithLiveStatus(context.Background(), cfg)
	if live[0].Running {
		t.Fatal("Agent reports running=false, should not be judged as running")
	}
	if live[0].Status != StatusInstalled {
		t.Fatalf("status should be %q (the check starts it based on this), got %q", StatusInstalled, live[0].Status)
	}
}

// On probe failure it must be set to unknown: starting requires knowing for certain that it is not running.
// When Docker or the Agent is unreachable, that is not known for certain, and repeatedly restarting the container would only make things worse.
func TestListWithLiveStatus_ProbeFailureIsNotTreatedAsIdle(t *testing.T) {
	cfg := liveStatusConfig(t)
	installDir := cfg.Runners.Items[0].InstallPath(cfg.Runners.BasePath)
	fakeDocker(t, runningHostSocketInspect(installDir))
	// Do not start a fake Agent: the container is running but the Agent is unreachable

	live := ListWithLiveStatus(context.Background(), cfg)
	if live[0].Status == StatusInstalled {
		t.Fatalf("status should not be %q on probe failure, otherwise the check would still start it", StatusInstalled)
	}
	if live[0].Status != StatusUnknown {
		t.Fatalf("status on probe failure should be %q, got %q", StatusUnknown, live[0].Status)
	}
}

// The container does not exist at all: it should be started, leaving creation to StartIfInstalled
func TestListWithLiveStatus_MissingContainerStaysStartable(t *testing.T) {
	cfg := liveStatusConfig(t)
	fakeDocker(t, `[]`)

	live := ListWithLiveStatus(context.Background(), cfg)
	if live[0].Running {
		t.Fatal("should not be judged as running when the container does not exist")
	}
	if live[0].Status != StatusInstalled {
		t.Fatalf("container does not exist but directory is registered, status should be %q, got %q", StatusInstalled, live[0].Status)
	}
}

// In default mode, List already reflects the real state of local processes, so it should not go around asking docker
func TestListWithLiveStatus_DefaultModePassesThrough(t *testing.T) {
	cfg := liveStatusConfig(t)
	cfg.Runners.ContainerMode = false
	logPath := fakeDocker(t, `[]`)

	live := ListWithLiveStatus(context.Background(), cfg)
	if len(live) != 1 || live[0].Status != StatusInstalled {
		t.Fatalf("default mode should return List's result unchanged, got %+v", live)
	}
	if calls := dockerCalls(t, logPath); calls != "" {
		t.Fatalf("default mode should not call docker, but it called:\n%s", calls)
	}
}

// liveStatusConfigUnregistered is the same as liveStatusConfig, except it does not write .runner:
// The config contains this runner, but it has never been registered
func liveStatusConfigUnregistered(t *testing.T) *config.Config {
	t.Helper()
	cfg := liveStatusConfig(t)
	installDir := cfg.Runners.Items[0].InstallPath(cfg.Runners.BasePath)
	if err := os.Remove(filepath.Join(installDir, ".runner")); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A never-registered runner must not be described by the probe result as 'registered but not running'.
// When the container does not exist, ContainerRunnerStatus always returns installed. If that overwrites the disk state,
// startIdleRunners would create a container for a runner that was never configured and send /start.
func TestListWithLiveStatus_UnregisteredRunnerStaysNew(t *testing.T) {
	cfg := liveStatusConfigUnregistered(t)
	fakeDocker(t, `[]`) // Container does not exist

	live := ListWithLiveStatus(context.Background(), cfg)
	if live[0].Status != StatusNew {
		t.Fatalf("without .runner and with no container, should be %q, got %q - the check would create a container for it based on this", StatusNew, live[0].Status)
	}
	if live[0].Running {
		t.Fatal("should not be judged as running")
	}
}

// Container exists but is stopped, and the directory is still unregistered: likewise must not be described as installed
func TestListWithLiveStatus_UnregisteredRunnerWithStoppedContainerStaysNew(t *testing.T) {
	cfg := liveStatusConfigUnregistered(t)
	installDir := cfg.Runners.Items[0].InstallPath(cfg.Runners.BasePath)
	fakeDocker(t, stoppedHostSocketInspect(installDir))

	live := ListWithLiveStatus(context.Background(), cfg)
	if live[0].Status != StatusNew {
		t.Fatalf("should be %q when the directory is unregistered, got %q", StatusNew, live[0].Status)
	}
}

// Even less can it be described as installed when the install directory does not exist at all:
// the check would create the container based on this, and Docker would create the bind mount source path owned by root,
// after which Manager (UID 1001) can no longer write to it
func TestListWithLiveStatus_MissingInstallDirStaysMissing(t *testing.T) {
	cfg := liveStatusConfig(t)
	installDir := cfg.Runners.Items[0].InstallPath(cfg.Runners.BasePath)
	if err := os.RemoveAll(installDir); err != nil {
		t.Fatal(err)
	}
	fakeDocker(t, `[]`)

	live := ListWithLiveStatus(context.Background(), cfg)
	if live[0].Status != StatusMissing {
		t.Fatalf("Expected %q when the install directory does not exist, got %q", StatusMissing, live[0].Status)
	}
}

// Pin the ContainerRunnerStatus contract directly: with no container to ask,
// fall back to the on-disk state instead of always reporting installed.
func TestContainerRunnerStatus_FallsBackToDiskStatus(t *testing.T) {
	cfg := liveStatusConfig(t)
	installDir := cfg.Runners.Items[0].InstallPath(cfg.Runners.BasePath)
	fakeDocker(t, `[]`)

	// With .runner: still "registered but not running" when the container is missing
	_, status, _, err := ContainerRunnerStatus(context.Background(), cfg, "droiddesk-2", installDir)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusInstalled {
		t.Fatalf("Expected %q for a registered directory, got %q", StatusInstalled, status)
	}

	// Remove .runner: must no longer report installed
	if err := os.Remove(filepath.Join(installDir, ".runner")); err != nil {
		t.Fatal(err)
	}
	_, status, _, err = ContainerRunnerStatus(context.Background(), cfg, "droiddesk-2", installDir)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusNew {
		t.Fatalf("Expected %q for an unregistered directory, got %q", StatusNew, status)
	}
}
