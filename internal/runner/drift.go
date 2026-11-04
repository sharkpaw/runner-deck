// Container config drift detection.
//
// The runner container's image, network, mounts, and Job Docker backend are all fixed at the moment docker create runs,
// so changing the config later has no effect on existing containers. docker start just brings back the same container as it was.
// That makes 'config changed but nothing took effect' the easiest trap to fall into: a runner created with host-socket still
// mounts the host socket after switching to dind, so the isolation we meant to tighten never happened, and the UI looked fine.
//
// containerSpec extracts 'what the container should look like under the current config'. Create arguments and drift comparison are both produced from it,
// so the two never get separate implementations. The comparison uses the actual values returned by docker inspect, so containers built by older versions apply too.
package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sharkpaw/runner-deck/internal/config"
	docker "github.com/soulteary/docker-kit"
)

// runnerMountDest The mount point of the Runner directory inside the container
const runnerMountDest = "/runner"

// labelJobBackend Records the Job Docker backend in effect at creation time as a label on the container.
// It can't be inferred from DOCKER_HOST alone: Config.Env from docker inspect mixes in ENV set by the image itself,
// and a custom Runner image that sets ENV DOCKER_HOST would make backend=none be judged drifted forever,
// so the container would be deleted and rebuilt on every start. Labels are written by us and can't be confused with the image.
const labelJobBackend = "io.runner-deck.job-docker-backend"

// labelNetwork The network name recorded at creation. Same reasoning as the backend label:
// a container can be attached to other networks later with docker network connect, so 'which networks it is connected to'
// can't answer 'which network it was created on'. Only what was recorded at creation counts.
const labelNetwork = "io.runner-deck.network"

// containerSpec The container shape derived from the current config
type containerSpec struct {
	ContainerName string
	Image         string
	Network       string
	MountSrc      string // The runner directory on the host
	JobBackend    string
	DindHost      string
	DockerGID     int // unknownDockerGID Means don't append --group-add
	AgentToken    string
	Limits        config.ResourceLimits
}

// desiredContainerSpec Derives a given Runner's container shape from config.
// Image and Job Docker backend can be overridden per Runner. When unset, they fall back to the global config.
func desiredContainerSpec(cfg *config.Config, runnerName, installDir, agentToken string) containerSpec {
	network := cfg.Runners.ContainerNetwork
	if network == "" {
		network = "runner-net"
	}
	dindHost := cfg.Runners.DindHost
	if dindHost == "" {
		dindHost = "runner-dind"
	}
	return containerSpec{
		ContainerName: ContainerName(runnerName),
		Image:         cfg.ContainerImageFor(runnerName),
		Network:       network,
		MountSrc:      hostMountSource(cfg, installDir),
		JobBackend:    cfg.JobDockerBackendFor(runnerName),
		DindHost:      dindHost,
		DockerGID:     runnerDockerGID(cfg),
		AgentToken:    agentToken,
		Limits:        cfg.ResourcesFor(runnerName),
	}
}

// hostMountSource Returns the host path mounted at /runner in the container.
// When the Manager runs inside a container, installDir is a path inside that container, but docker needs a host path, so convert it using volume_host_path.
func hostMountSource(cfg *config.Config, installDir string) string {
	if cfg.Runners.VolumeHostPath != "" {
		rel, err := filepath.Rel(cfg.Runners.BasePath, installDir)
		if err != nil {
			rel = filepath.Base(installDir)
		}
		return filepath.Join(cfg.Runners.VolumeHostPath, rel)
	}
	// When the Manager runs on the host, pass an absolute path to avoid depending on cwd
	if abs, err := filepath.Abs(installDir); err == nil {
		return abs
	}
	return installDir
}

// dockerHostEnv Returns the DOCKER_HOST injected into the container for this backend. none returns an empty string, meaning no injection
func (s containerSpec) dockerHostEnv() string {
	switch s.JobBackend {
	case "dind":
		return "tcp://" + s.DindHost + ":2375"
	case "host-socket":
		return "unix://" + HostDockerSocket
	default:
		return ""
	}
}

// runnerBind The /runner mount entry, written in the same form as docker inspect's HostConfig.Binds
func (s containerSpec) runnerBind() string {
	return s.MountSrc + ":" + runnerMountDest
}

// createArgs Assembles the docker create arguments.
// When DockerGID is unknownDockerGID, no --group-add is appended and it relies only on the docker group preset in the image.
// Fields unset in Limits produce no arguments, keeping the same 'unlimited' behavior as older versions.
func (s containerSpec) createArgs() ([]string, error) {
	args := []string{
		"create",
		"--name", s.ContainerName,
		"-v", s.runnerBind(),
		"--network", s.Network,
		"--label", labelJobBackend + "=" + s.JobBackend,
		"--label", labelNetwork + "=" + s.Network,
	}
	// Resource limits: without them, a single runaway Job could exhaust the whole machine's resources and drag the Manager down with it
	args = append(args, s.Limits.Args()...)
	// The token is injected via an environment variable, not only through a file in the mounted directory:
	// If the Manager runs as root or as a UID other than 1001, a 0600 token file is unreadable to app (1001) inside the container,
	// so the Agent reads an empty token and silently downgrades to unauthenticated. Environment variables don't depend on UID matching.
	if s.AgentToken != "" {
		args = append(args, "-e", "AGENT_TOKEN="+s.AgentToken)
	}
	switch s.JobBackend {
	case "dind":
		args = append(args, "-e", "DOCKER_HOST="+s.dockerHostEnv())
	case "host-socket":
		args = append(args, "-v", HostDockerSocket+":"+HostDockerSocket, "-e", "DOCKER_HOST="+s.dockerHostEnv())
		// The container runs as app (UID 1001) inside and must join the socket's owning group, otherwise docker in a Job reports permission denied
		if s.DockerGID >= 0 {
			args = append(args, "--group-add", strconv.Itoa(s.DockerGID))
		}
	case "none":
		// Jobs don't get Docker, so no environment or mounts are injected
	default:
		return nil, fmt.Errorf("unsupported runners.job_docker_backend=%q (only dind, host-socket and none are supported)", s.JobBackend)
	}
	return append(args, s.Image), nil
}

// containerFacts The actual values from docker inspect that relate to the create arguments
// containerFacts is the fact as read by docker inspect.
//
// A type alias rather than a new type: it **is** docker.Facts, so existing test cases that construct
// containerFacts{...} directly need no changes, and there won't be two look-alike types that can't be assigned to each other
// type. Fields correspond one-to-one with the originals, including the separate collection of NetworkMode and Networks.
type containerFacts = docker.Facts

// inspectRunnerContainer Gets the container's actual state and creation arguments. Returns (nil, nil) if the container does not exist
func inspectRunnerContainer(ctx context.Context, containerName string) (*containerFacts, error) {
	return docker.Inspect(ctx, containerName)
}

// parseInspect parses the output of docker inspect (an array; takes the first item)
func parseInspect(out []byte) (*containerFacts, error) { return docker.ParseInspect(out) }

// resolveImageID returns the image ID that an image reference currently points to; it returns an empty string when the image is not present locally (in that case only the reference itself is compared, and no pull is attempted)
func resolveImageID(ctx context.Context, imageRef string) string {
	return docker.ImageID(ctx, imageRef)
}

// imageIDCache caches "image reference → image ID".
//
// The list page compares the image once for every runner, and they usually share the same image. Without a cache, every page refresh
// adds N docker image inspect calls, each one a new process. An image ID only changes when the image is rebuilt or pulled,
// so a briefly stale entry only delays the "config changed" badge by a few seconds.
//
// The start/stop path does not use this cache: clicking "start" right after a rebuild is a common action there, and reading a stale ID would stop the rebuild from taking effect.
// docker-kit's ImageIDCache comment states this as a usage restriction, with the same meaning as here.
var imageIDCache = &docker.ImageIDCache{} // TTL uses the default of 10 seconds

// resetImageIDCache drops all cache entries, for isolating test cases.
// docker-kit's ImageIDCache has no per-entry delete, so replacing the whole cache is enough. Its internals are a sync.Map,
// swapping in a new one is simpler than exposing a Delete that only tests would use, and its semantics are stronger (it also clears other keys).
func resetImageIDCache() { imageIDCache = &docker.ImageIDCache{} }

// cachedImageID looks up an image ID with a TTL, for use in status display.
func cachedImageID(ctx context.Context, imageRef string) string {
	return imageIDCache.Get(ctx, imageRef)
}

// envValue gets the value of a variable from the Env field of docker inspect output.
// Keep the "take a slice" signature: this package and its tests both call it with an Env slice, while the kit exposes the Facts method instead.
func envValue(env []string, key string) (string, bool) {
	return (&docker.Facts{Env: env}).EnvValue(key)
}

// bindSource finds the host path mounted at dest in Binds.
func bindSource(binds []string, dest string) (string, bool) {
	return (&docker.Facts{Binds: binds}).BindSource(dest)
}

// driftReason compares an existing container with the expected shape and returns a description of the drift; "" means they match.
// The description is written as "config key: old value → new value": it is independent of the UI language (it goes into logs and tooltips verbatim),
// and it directly tells the reader which config item to look at.
// An empty desiredImageID means the image is not available locally and no ID could be obtained; in that case only the image reference is compared.
func (s containerSpec) driftReason(facts *containerFacts, desiredImageID string) string {
	if facts == nil {
		return ""
	}
	if facts.ImageRef != "" && facts.ImageRef != s.Image {
		return fmt.Sprintf("container_image: %s → %s", facts.ImageRef, s.Image)
	}
	// If an existing tag is rebuilt, the reference stays the same but the ID changes. A "the image content changed" case like this also requires a rebuild
	if desiredImageID != "" && facts.ImageID != "" && facts.ImageID != desiredImageID {
		return fmt.Sprintf("container_image rebuilt: %s", s.Image)
	}
	if reason := s.networkDrift(facts); reason != "" {
		return reason
	}
	if src, ok := bindSource(facts.Binds, runnerMountDest); ok && src != s.MountSrc {
		return fmt.Sprintf("volume_host_path: %s → %s", src, s.MountSrc)
	}
	if reason := s.backendDrift(facts); reason != "" {
		return reason
	}
	// The token check only asks "is there a non-empty value". Containers created before this feature have no AGENT_TOKEN, so the agent falls back to no authentication,
	// and other containers on the same network can control this runner. Rebuilding once fixes it.
	//
	// The value is deliberately not compared. A token mismatch is a runtime fault (the agent returns 401), and it is left for probing to expose,
	// and it should not be a reason to delete the container. But an "empty value" is not a mismatch, it is an absence: on the agent side, if the value is empty after TrimSpace,
	// the agent falls back to reading the mounted .agent_token, and that read fails (the file is root-owned with mode 0600, which is exactly the migration scenario this change fixes),
	// so it runs with no authentication at all, and no 401 would ever reveal it. So emptiness is checked here using the agent's rule,
	// otherwise a single line ENV AGENT_TOKEN= in the image would let this container skip rebuilds forever.
	if s.AgentToken != "" {
		if v, ok := envValue(facts.Env, "AGENT_TOKEN"); !ok || strings.TrimSpace(v) == "" {
			return "agent_token: (none) → set"
		}
	}
	if s.JobBackend == "host-socket" && s.DockerGID >= 0 {
		gid := strconv.Itoa(s.DockerGID)
		if len(facts.GroupAdd) > 0 && !containsString(facts.GroupAdd, gid) {
			return fmt.Sprintf("docker_gid: %s → %s", strings.Join(facts.GroupAdd, ","), gid)
		}
		if len(facts.GroupAdd) == 0 {
			return "docker_gid: (none) → " + gid
		}
	}
	return ""
}

// networkDrift compares the container's network: new containers are checked via their creation label, while old containers can only be inferred from NetworkMode.
//
// It cannot simply ask "is the container connected to the target network?" docker create --network sets only one network,
// but a container can also be attached to other networks with docker network connect. Suppose the config changes from runner-net to net-b,
// and the container happens to be connected to both networks. A "match either" check would pass it, so it keeps being connected to runner-net,
// which is exactly the connection this config change is meant to cut.
//
// NetworkMode is the parameter that create actually received, but it may be normalized (or hold different values on older docker versions),
// so comparing it strictly could mismatch and trigger a rebuild on every start. So, like the backend, the value is recorded in a creation label:
// containers with the label use the label; only containers without it fall back to inferring from NetworkMode. Even if that inference is wrong, it only causes one rebuild,
// since the rebuilt container has the label, it will not repeat.
func (s containerSpec) networkDrift(facts *containerFacts) string {
	if got, ok := facts.Labels[labelNetwork]; ok {
		if got != s.Network {
			return fmt.Sprintf("container_network: %s → %s", got, s.Network)
		}
		return ""
	}
	if facts.NetworkMode != "" {
		if facts.NetworkMode != s.Network {
			return fmt.Sprintf("container_network: %s → %s", facts.NetworkMode, s.Network)
		}
		return ""
	}
	// If even NetworkMode is unavailable (old docker, incomplete info), fall back to "being connected counts as a match,"
	// Like the other comparisons, prefer a missed drift report over deleting and rebuilding the container without cause
	if len(facts.Networks) > 0 && !containsString(facts.Networks, s.Network) {
		return fmt.Sprintf("container_network: %s → %s", strings.Join(facts.Networks, ","), s.Network)
	}
	return ""
}

// backendDrift compares the Job Docker backend: new containers are checked via their label, while old containers can only be inferred from DOCKER_HOST and the socket mount
func (s containerSpec) backendDrift(facts *containerFacts) string {
	if got, ok := facts.Labels[labelJobBackend]; ok {
		if got != s.JobBackend {
			return "job_docker_backend: " + got + " → " + s.JobBackend
		}
		if s.JobBackend == "dind" {
			if env, has := envValue(facts.Env, "DOCKER_HOST"); has && env != s.dockerHostEnv() {
				return fmt.Sprintf("dind_host: %s → %s", env, s.dockerHostEnv())
			}
		}
		return ""
	}
	return s.legacyBackendDrift(facts)
}

// legacyBackendDrift handles legacy containers that have no label. It only recognizes the DOCKER_HOST values we ourselves write,
// and treats same-named variables baked into the image as unrelated. Missing a drift is better than deleting a container by mistake.
// Once a container like this is recreated, it carries a label and no longer takes this path.
func (s containerSpec) legacyBackendDrift(facts *containerFacts) string {
	got, _ := envValue(facts.Env, "DOCKER_HOST")
	_, hasSocket := bindSource(facts.Binds, HostDockerSocket)
	injected := hasSocket || looksInjectedDockerHost(got)

	switch s.JobBackend {
	case "none":
		if injected {
			return "job_docker_backend: → none"
		}
	case "host-socket":
		if got != s.dockerHostEnv() || !hasSocket {
			return "job_docker_backend: → host-socket"
		}
	case "dind":
		if hasSocket {
			return "job_docker_backend: → dind"
		}
		if got == s.dockerHostEnv() {
			return ""
		}
		// The value is still a DinD address, so it was dind before and only the address changed. Report it more specifically.
		if strings.HasPrefix(got, "tcp://") && strings.HasSuffix(got, ":2375") {
			return fmt.Sprintf("dind_host: %s → %s", got, s.dockerHostEnv())
		}
		return "job_docker_backend: → dind"
	}
	return ""
}

// looksInjectedDockerHost reports whether the DOCKER_HOST value looks like one we injected.
//
// It deliberately does not compare against the current dind_host. Containers are created with the config at that time, and when switching to none
// dind_host is often changed or removed too, since keeping it without dind makes no sense. Matching only the current value would treat tcp://old-dind:2375
// as unrelated and miss it, so the container would keep running with a DOCKER_HOST pointing at the old DinD, which is exactly the path none
// is meant to cut off. So we match by shape, consistent with the dind branch in legacyBackendDrift.
//
// Cost: if a custom image's own ENV DOCKER_HOST happens to be tcp://…:2375, such an old container gets recreated one extra time
// (recreation does not remove the image's ENV, so that recreation is wasted). But once recreated it carries the label and uses label
// comparison from then on, so this does not repeat. We accept one unnecessary recreation to avoid missing a real Docker path.
func looksInjectedDockerHost(v string) bool {
	if v == "" {
		return false
	}
	if v == "unix://"+HostDockerSocket {
		return true
	}
	// The DinD address we inject has the form tcp://<dind_host>:2375
	return strings.HasPrefix(v, "tcp://") && strings.HasSuffix(v, ":2375")
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// How imageIDResolver looks up image IDs: status display uses the cached lookup, start/stop uses the live one.
type imageIDResolver func(context.Context, string) string

// Comparison entry point for when driftFromFacts already has the inspect result, saving a repeated docker inspect.
//
// agentToken is obtained by the caller via EnsureAgentToken and passed in. It is not read here:
// Whether the token exists decides whether to report an agent_token drift, and whether it exists depends on the caller having generated it first.
// Previously this called ReadAgentToken directly, so old containers that were **already running** during an upgrade became a blind spot.
// Their directories have no token file yet, and auto-start only handles containers that are not running, so nothing generated it,
// the read always came back empty, the check was skipped without any notice in the UI, and the container never authenticated.
func driftFromFacts(ctx context.Context, cfg *config.Config, runnerName, installDir, agentToken string, facts *containerFacts, imageID imageIDResolver) string {
	if cfg == nil || !cfg.Runners.ContainerMode || facts == nil {
		return ""
	}
	// The comparison only checks whether the container was injected with AGENT_TOKEN, not its value.
	spec := desiredContainerSpec(cfg, runnerName, installDir, agentToken)
	if _, err := spec.createArgs(); err != nil {
		// When the backend config itself is invalid, drift is not discussed; startup reports the explicit error.
		return ""
	}
	return spec.driftReason(facts, imageID(ctx, spec.Image))
}
