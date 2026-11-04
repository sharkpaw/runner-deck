// Container mode: C/S control and status queries via the Docker CLI and the Agent inside the Runner container.
// This package handles creating, starting, stopping, and deleting Runner containers, plus HTTP communication with the Agent. Manager only orchestrates and does not host Runner processes.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
	docker "github.com/soulteary/docker-kit"
)

// stopTimeoutArg is the -t value (in seconds) for docker stop.
//
// Taken from runnerproc.StopGracePeriod instead of a hardcoded 30: after the Agent in the container receives SIGTERM, it waits
// for the Runner to exit, and the same constant determines how long. If the two disagree, the shorter one wins: lower -t,
// and the Agent is still waiting on the Runner when the container gets SIGKILL, so the Job is cut off mid-run anyway.
func stopTimeoutArg() string {
	return strconv.Itoa(int(runnerproc.StopGracePeriod / time.Second))
}

// ContainerName converts the runner name into a valid container name, consistent with the config package rules
func ContainerName(name string) string {
	return config.NormalizedContainerName(name)
}

// AgentStatus is the structure returned by the Agent's /status endpoint inside the container
type AgentStatus struct {
	Status  string `json:"status"`
	Running bool   `json:"running"`
}

// agentBaseURL builds the base URL of the Agent inside the container. It is a variable so tests can point it at a local
// httptest server. The container name can't be resolved in the test process, so otherwise only the "connection refused" path could be tested.
var agentBaseURL = func(containerName string, port int) string {
	return fmt.Sprintf("http://%s:%d", containerName, port)
}

// GetAgentStatus requests /status from the Agent inside the Runner container, with a 5 second timeout.
// Callers must first obtain the persistent token; the new Agent rejects anonymous requests.
func GetAgentStatus(ctx context.Context, containerName string, port int, token string) (*AgentStatus, error) {
	if port <= 0 {
		port = 8081
	}
	url := agentBaseURL(containerName, port) + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setAgentAuth(req, token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return nil, fmt.Errorf("agent returned %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("agent returned %d: %s", resp.StatusCode, msg)
	}
	var out AgentStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CallAgentStart sends POST /start to the Agent inside the Runner container.
// Callers must first obtain the persistent token; the new Agent rejects anonymous requests.
func CallAgentStart(ctx context.Context, containerName string, port int, token string) error {
	if port <= 0 {
		port = 8081
	}
	url := agentBaseURL(containerName, port) + "/start"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	setAgentAuth(req, token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return fmt.Errorf("agent /start returned %d", resp.StatusCode)
		}
		return fmt.Errorf("agent /start returned %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// setAgentAuth attaches the token to Agent requests; if the token is empty, no header is added
func setAgentAuth(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// The four functions below forward directly to docker-kit. We keep this package's names because they are referenced nearly thirty times
// across this package and its use cases, and renaming them would not make any of those call sites clearer.
//
// docker-kit's checks match the original implementation here one for one, including Chinese-locale messages such as "no such container"
// Those messages were originally extracted from here, so this is the same logic, not roughly equivalent logic.

func dockerCmd(ctx context.Context, args ...string) ([]byte, error) {
	return docker.Run(ctx, args...)
}

// containerNotFound reports whether docker output means the container does not exist (English, Chinese, etc.)
func containerNotFound(out []byte) bool { return docker.NotFound(out) }

// containerStartUnrecoverable reports whether docker start failed in a way that can't be recovered, such as the network having been removed; the container must be deleted and recreated
func containerStartUnrecoverable(out []byte) bool { return docker.UnrecoverableStart(out) }

// dockerPermissionDenied reports whether an error is a Docker permission or connection error (the host socket must be accessible to the Manager container)
func dockerPermissionDenied(out []byte) bool { return docker.PermissionDenied(out) }

func dockerCmdError(op string, out []byte, err error) error {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		trimmed = "(no output)"
	}
	if dockerPermissionDenied(out) {
		return fmt.Errorf("%s failed (permission denied, or the daemon is unreachable). %s. Output: %s: %w", op, dockerAccessHint, trimmed, err)
	}
	return fmt.Errorf("%s failed. Output: %s: %w", op, trimmed, err)
}

const dockerAccessHint = "If the Manager runs in a container, give runner-manager a group_add with the host docker group GID (DOCKER_GID=$(getent group docker | cut -d: -f3) in .env), or use user: \"0:0\" to reach the socket as root"

// managerMustUseHostDocker is a reminder: in container mode, Manager must use the host Docker to create Runner containers, and DOCKER_HOST must not be set to DinD
const errContainerModeNeedHostDocker = "in container mode the Manager must use the host Docker (unix socket) to create, start and stop runner containers; DinD cannot do it. Remove or comment out DOCKER_HOST=tcp://runner-dind:2375 in .env so the Manager falls back to unix:///var/run/docker.sock. DinD is only for what jobs do inside the runner container, such as docker build"

func managerDockerHostIsDind() bool {
	h := os.Getenv("DOCKER_HOST")
	return strings.HasPrefix(strings.TrimSpace(h), "tcp://")
}

// ManagerDockerHostIsDind is checked at startup: if true and container mode is enabled, Manager cannot create Runner containers
func ManagerDockerHostIsDind() bool {
	return managerDockerHostIsDind()
}

// HostDockerSocket is the host Docker socket mounted into the Runner container when job_docker_backend=host-socket
const HostDockerSocket = "/var/run/docker.sock"

// runnerOps serializes starts, stops, and rebuilds per Runner.
//
// The same Runner can be hit by several paths at once: the auto-start 15 seconds after Manager boots, the periodic start every 5 minutes,
// the start right after registration completes, and clicks in the UI. Previously these collisions left at most one "container name is already in use"
// log line. Now a rebuild runs docker rm first, and if two calls interleave they could delete the container the other one just created.
var runnerOps docker.Locks

// lockRunnerOps acquires the operation lock for a container and returns the unlock function
func lockRunnerOps(containerName string) func() { return runnerOps.Lock(containerName) }

// Readiness wait for the Agent after starting a container: a newly created container must wait for the image entrypoint to come up; an existing one is faster.
// It is a variable only so tests can shorten it; the production path uses the same value as before.
var (
	agentReadyDelayAfterCreate = 3 * time.Second
	agentReadyDelayAfterStart  = 2 * time.Second
)

// unknownDockerGID means the group owning docker.sock cannot be determined, so --group-add is not appended
const unknownDockerGID = -1

// socketGID returns the GID of the group owning path; on read failure or unsupported platforms it returns unknownDockerGID.
// When Manager runs inside a container, docker.sock is a host mount, so the stat result is the host docker group GID.
//
// docker-kit's SocketGID also returns -1 when it can't be determined, which matches unknownDockerGID.
func socketGID(path string) int { return docker.SocketGID(path) }

// runnerDockerGID determines the docker group GID to append to the Runner container:
// prefer runners.docker_gid (which the DOCKER_GID environment variable can override), otherwise auto-detect the group owning docker.sock.
func runnerDockerGID(cfg *config.Config) int {
	if cfg != nil && cfg.Runners.DockerGID > 0 {
		return cfg.Runners.DockerGID
	}
	return socketGID(HostDockerSocket)
}

// runnerCreateArgs builds the docker create arguments for the Runner container.
// The actual shape is defined by containerSpec (see drift.go): creation arguments and drift comparison share the same definition,
// Keep it from leaving the create logic and drift comparison on the old approach.
func runnerCreateArgs(containerName, mountSrc, network, img, jobBackend, dindHost string, dockerGID int, limits config.ResourceLimits, agentToken string) ([]string, error) {
	spec := containerSpec{
		ContainerName: containerName,
		Image:         img,
		Network:       network,
		MountSrc:      mountSrc,
		JobBackend:    jobBackend,
		DindHost:      dindHost,
		DockerGID:     dockerGID,
		AgentToken:    agentToken,
		Limits:        limits,
	}
	return spec.createArgs()
}

// applyResourceLimitsToExisting applies resource limits to existing containers.
//
// docker create resource flags only take effect at creation: after upgrading to a version that supports runners.resources,
// existing containers stay unlimited, and stop/start alone won't change that. Here docker update fills the gap,
// so the config also applies to existing containers without deleting and recreating them.
// If no limits are configured, nothing is done, keeping behavior the same as the old version.
func applyResourceLimitsToExisting(ctx context.Context, containerName string, limits config.ResourceLimits) {
	updateArgs := resourceUpdateArgs(containerName, limits)
	if updateArgs == nil {
		return
	}
	if out, err := dockerCmd(ctx, updateArgs...); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		log.Printf("warning: cannot apply resource limits to the existing container %s, which keeps the limits it was created with: %s (run docker rm -f %s and start it again from the UI to rebuild it)",
			containerName, msg, containerName)
	}
}

// resourceUpdateArgs builds the docker update arguments; returns nil when no limits are configured, meaning nothing needs to run
func resourceUpdateArgs(containerName string, limits config.ResourceLimits) []string {
	args := limits.Args()
	if len(args) == 0 {
		return nil
	}
	return append(append([]string{"update"}, args...), containerName)
}

// StartRunnerContainer creates and starts the container if it doesn't exist, or starts it if it does; on create, mounts installDir to /runner.
// If an existing container's creation parameters differ from the current config (different image, network, mount directory, or Job Docker backend),
// it is deleted and rebuilt with the new config, but only if it is stopped. Running containers are left alone; see startRunnerContainer.
func StartRunnerContainer(ctx context.Context, cfg *config.Config, runnerName, installDir string) error {
	return startRunnerContainer(ctx, cfg, runnerName, installDir, false)
}

// RecreateRunnerContainer unconditionally rebuilds the container from the current config, deleting it even if it is running.
// Used for the case where the container is running but its config has changed, a decision only a person can make about when to interrupt.
func RecreateRunnerContainer(ctx context.Context, cfg *config.Config, runnerName, installDir string) error {
	return startRunnerContainer(ctx, cfg, runnerName, installDir, true)
}

func startRunnerContainer(ctx context.Context, cfg *config.Config, runnerName, installDir string, forceRecreate bool) error {
	if cfg.Runners.ContainerMode && managerDockerHostIsDind() {
		return fmt.Errorf("%s", errContainerModeNeedHostDocker)
	}
	cn := ContainerName(runnerName)
	defer lockRunnerOps(cn)()
	// Write the token before creating or starting the container, so the in-container Agent can read it from the mounted directory on startup.
	token, tokenErr := EnsureAgentToken(installDir)
	if tokenErr != nil {
		return fmt.Errorf("cannot authenticate runner Agent: %w", tokenErr)
	}
	facts, err := inspectRunnerContainer(ctx, cn)
	if err != nil {
		return err
	}
	if facts != nil {
		// The start/stop path doesn't use a cache: clicking Start right after a build is common, and reading a stale image ID would prevent the rebuild from happening
		drift := driftFromFacts(ctx, cfg, runnerName, installDir, token, facts, resolveImageID)
		switch {
		case forceRecreate:
			log.Printf("recreating container %s as requested%s", cn, driftSuffix(drift))
			if out, rmErr := dockerCmd(ctx, "rm", "-f", cn); rmErr != nil {
				return dockerCmdError("docker rm", out, rmErr)
			}
			// falls through to "Create new container" below
		case facts.Running:
			// Running containers are not rebuilt automatically: one is likely running a Job right now, and deleting it would cut that Job off mid-run.
			// Only log once; the UI prompts the user to stop it and start again, or to explicitly click "Rebuild container".
			if drift != "" {
				log.Printf("note: container %s was created with settings that no longer match the config (%s). "+
					"A running container is never rebuilt on its own: stop it and start it again, or use "+
					"\"Recreate container\" in the UI to rebuild it against the current config", cn, drift)
			}
			// Existing containers may have been created before resource limits were configured, so apply them once here
			applyResourceLimitsToExisting(ctx, cn, cfg.Runners.Resources)
			// Container is already running; optionally call Agent /start to make sure the listener is up (the agent may not have launched run.sh yet if the container just started)
			_ = CallAgentStart(ctx, cn, cfg.Runners.AgentPort, token)
			return nil
		case drift != "":
			log.Printf("container %s was created with settings that no longer match the config (%s); removing it and rebuilding against the current config", cn, drift)
			if out, rmErr := dockerCmd(ctx, "rm", "-f", cn); rmErr != nil {
				return dockerCmdError("docker rm", out, rmErr)
			}
			// falls through to "Create new container" below
		default:
			startOut, startErr := dockerCmd(ctx, "start", cn)
			if startErr == nil {
				applyResourceLimitsToExisting(ctx, cn, cfg.Runners.Resources)
				time.Sleep(agentReadyDelayAfterStart)
				return CallAgentStart(ctx, cn, cfg.Runners.AgentPort, token)
			}
			// If start fails for an unrecoverable reason such as a deleted network, delete the old container and rebuild it (e.g., the network was removed after compose down)
			if !containerStartUnrecoverable(startOut) {
				return dockerCmdError("docker start", startOut, startErr)
			}
			_, _ = dockerCmd(ctx, "rm", "-f", cn)
		}
	}
	// Create new container
	// In container mode, if the Manager runs inside a container (base_path is usually /app/runners) and volume_host_path is not set, docker create -v uses the in-container path, which is invalid on the host
	if cfg.Runners.ContainerMode && strings.TrimSpace(cfg.Runners.VolumeHostPath) == "" {
		baseClean := filepath.Clean(cfg.Runners.BasePath)
		if strings.HasPrefix(baseClean, "/app") || strings.HasPrefix(filepath.Clean(installDir), "/app") {
			return fmt.Errorf("in container mode, a Manager that itself runs in a container needs runners.volume_host_path in config/config.yaml set to the absolute path of the runners base directory on the host (base_path is currently %s)", cfg.Runners.BasePath)
		}
	}
	spec := desiredContainerSpec(cfg, runnerName, installDir, token)
	createArgs, err := spec.createArgs()
	if err != nil {
		return err
	}
	out, err := dockerCmd(ctx, createArgs...)
	if err != nil {
		return dockerCmdError("docker create", out, err)
	}
	out, err = dockerCmd(ctx, "start", cn)
	if err != nil {
		return dockerCmdError("docker start", out, err)
	}
	// Wait for the agent to be ready, then call /start
	time.Sleep(agentReadyDelayAfterCreate)
	return CallAgentStart(ctx, cn, cfg.Runners.AgentPort, token)
}

// driftSuffix appends the drift reasons to the log as a suffix, staying quiet when there are no differences
func driftSuffix(drift string) string {
	if drift == "" {
		return ""
	}
	return " (" + drift + ")"
}

// StopRunnerContainer stops the container (does not delete it, so it can be started again later)
func StopRunnerContainer(ctx context.Context, runnerName string) error {
	cn := ContainerName(runnerName)
	defer lockRunnerOps(cn)()
	out, err := dockerCmd(ctx, "stop", "-t", stopTimeoutArg(), cn)
	if err != nil {
		inspectOut, _ := dockerCmd(ctx, "inspect", "-f", "{{.State.Running}}", cn)
		if containerNotFound(inspectOut) {
			return nil
		}
		return dockerCmdError("docker stop", out, err)
	}
	return nil
}

// RemoveRunnerContainer stops and deletes the runner container (called when removing a runner)
func RemoveRunnerContainer(ctx context.Context, runnerName string) error {
	cn := ContainerName(runnerName)
	defer lockRunnerOps(cn)()
	_, _ = dockerCmd(ctx, "stop", "-t", stopTimeoutArg(), cn)
	out, err := dockerCmd(ctx, "rm", "-f", cn)
	if err != nil {
		if containerNotFound(out) {
			return nil
		}
		return dockerCmdError("docker rm", out, err)
	}
	return nil
}

// ContainerRunnerStatus gets a runner's status in container mode: first checks whether the container is running, then asks the Agent.
// The same inspect also compares creation parameters; non-empty drift means the container was built from an old config.
//
// When the container doesn't exist or isn't running there's no one to ask, so it honestly falls back to disk state (a registered runner stays StatusInstalled,
// which the UI shows as "registered, not running"). This used to ignore disk and always return StatusInstalled, so a
// runner in the config that had never been registered (new/missing on disk) was reported as "registered, not running". The UI showed the wrong state,
// and the background "launch if registered but not running" step then built a container for it and sent /start. When the install directory didn't exist at all,
// Docker would also create the bind mount's source path owned by root, and then Manager (UID 1001) couldn't write to it.
func ContainerRunnerStatus(ctx context.Context, cfg *config.Config, runnerName, installDir string) (running bool, status Status, drift string, err error) {
	cn := ContainerName(runnerName)
	facts, err := inspectRunnerContainer(ctx, cn)
	if err != nil {
		return false, StatusUnknown, "", newProbeError(ProbeErrorTypeDockerAccess, err)
	}
	if facts == nil {
		return false, diskStatus(installDir), "", nil
	}
	// Generate a persistent token for an old container so drift can be detected; if that fails, don't send anonymous Agent requests.
	token, tokenErr := EnsureAgentToken(installDir)
	if tokenErr != nil {
		WarnAgentTokenUnavailable(installDir, tokenErr)
		return facts.Running, StatusUnknown, "", newProbeError(ProbeErrorTypeAgentHTTP, tokenErr)
	}
	drift = driftFromFacts(ctx, cfg, runnerName, installDir, token, facts, cachedImageID)
	if !facts.Running {
		// When the container isn't running, keep the disk state (registered means "registered, not running"), don't overwrite it with unknown
		return false, diskStatus(installDir), drift, nil
	}
	agent, err := GetAgentStatus(ctx, cn, cfg.Runners.AgentPort, token)
	if err != nil {
		agentErrType := ProbeErrorTypeAgentConnect
		if strings.Contains(err.Error(), "agent returned") {
			agentErrType = ProbeErrorTypeAgentHTTP
		}
		return true, StatusUnknown, drift, newProbeError(agentErrType, err)
	}
	switch agent.Status {
	case "installed":
		return agent.Running, StatusInstalled, drift, nil
	case "new":
		return false, StatusNew, drift, nil
	default:
		return false, StatusMissing, drift, nil
	}
}

// ContainerState returns whether the container exists and its state (running / exited / created, etc.).
// When the container doesn't exist, return exists=false without an error; most callers only care whether the name is already taken.
func ContainerState(ctx context.Context, containerName string) (exists bool, status string, err error) {
	facts, err := inspectRunnerContainer(ctx, containerName)
	if err != nil || facts == nil {
		return false, "", err
	}
	return true, facts.Status, nil
}
