// Startup self-check: surface misconfigurations that would otherwise only appear when a Job fails, at Manager startup.
// Typical cases: runner-net not created, mount directory not owned by UID 1001, docker.sock unreachable, Runner image missing.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
	preflight "github.com/soulteary/preflight-kit"
)

// The result model comes from preflight-kit. Here we use a **type alias** rather than a new type:
// CheckResult is preflight.Result, so existing callers and cases need no changes,
// and there is no risk of two identical-looking types that cannot be assigned to each other.
type (
	// CheckLevel is the severity level of a check result
	CheckLevel = preflight.Level
	// CheckResult is the result of a single check
	CheckResult = preflight.Result
)

const (
	CheckOK    = preflight.LevelOK
	CheckWarn  = preflight.LevelWarn  // Does not block startup, but likely to cause problems when running Jobs
	CheckError = preflight.LevelError // Under the current configuration, it almost certainly won't run
)

func ok(name, msg string) CheckResult         { return preflight.OK(name, msg) }
func warn(name, msg, hint string) CheckResult { return preflight.Warn(name, msg, hint) }
func fail(name, msg, hint string) CheckResult { return preflight.Fail(name, msg, hint) }

// runCheck runs one check and also recovers preflight.Run's panics:
// A crashing check must not take Manager startup down with it. The name is only needed in the fallback result,
// On the normal path, the check fills in Name itself.
func runCheck(ctx context.Context, name string, f func(context.Context) CheckResult) CheckResult {
	return preflight.Run(ctx, preflight.Named(name, f))[0]
}

// Preflight runs a set of read-only checks against the current configuration and changes no state.
// Results are returned in check order; the caller decides whether to log them or return them to the UI.
func Preflight(ctx context.Context, cfg *config.Config) []CheckResult {
	if cfg == nil {
		return []CheckResult{fail("config", "the configuration is empty", "")}
	}
	results := []CheckResult{
		runCheck(ctx, "runners directory", func(context.Context) CheckResult { return checkBasePath(cfg) }),
		runCheck(ctx, "runner directory permissions", func(context.Context) CheckResult { return checkRunnerDirPermissions(cfg) }),
	}
	if !cfg.Runners.ContainerMode {
		results = append(results, runCheck(ctx, "runner isolation", func(context.Context) CheckResult {
			return checkDefaultModeIsolation(cfg)
		}))
		return append(results, runCheck(ctx, "Docker in jobs", checkDefaultModeDocker))
	}
	results = append(results,
		runCheck(ctx, "Docker reachability", checkDockerReachable),
		runCheck(ctx, "container network", func(ctx context.Context) CheckResult { return checkNetwork(ctx, cfg) }),
	)
	// Network membership is only checked when a Runner actually uses dind: under other backends the network has no such unauthenticated daemon,
	// and extra containers are just extra containers; reporting them would only be noise
	if anyDindBackend(cfg) {
		results = append(results, runCheck(ctx, "container network members", func(ctx context.Context) CheckResult {
			return checkNetworkMembers(ctx, cfg)
		}))
	}
	results = append(results, checkRunnerImages(ctx, cfg)...)
	return append(results, runCheck(ctx, "Docker in jobs", func(ctx context.Context) CheckResult {
		return checkJobDockerBackend(ctx, cfg)
	}))
}

// checkBasePath checks that the runners root directory exists and is writable by the current process (inside the container it runs as UID 1001,
// and a host directory not chowned to 1001:1001 is the most common deployment mistake)
func checkBasePath(cfg *config.Config) CheckResult {
	const name = "runners directory"
	base := cfg.Runners.BasePath
	info, err := os.Stat(base)
	if err != nil {
		return fail(name, fmt.Sprintf("%s is not reachable: %v", base, err),
			fmt.Sprintf("mkdir -p %s && chown %d:%d %s", base, os.Getuid(), os.Getgid(), base))
	}
	if !info.IsDir() {
		return fail(name, fmt.Sprintf("%s is not a directory", base), "")
	}
	// Use a unique temp file name rather than a fixed one: a fixed name could collide with a user file of the same name in the directory,
	// and the check is meant to be read-only, yet it would open and delete that file
	f, err := os.CreateTemp(base, ".preflight-write-test-*")
	if err != nil {
		return fail(name, fmt.Sprintf("%s is not writable by the current user (UID %d): %v", base, os.Getuid(), err),
			fmt.Sprintf("on the host, run: chown -R %d:%d <the matching directory on the host>", os.Getuid(), os.Getgid()))
	}
	probe := f.Name()
	_ = f.Close()
	// A failed delete means the directory is not really usable (e.g. sticky bit or read-only mount), so it must not be reported as writable
	if err := os.Remove(probe); err != nil {
		return fail(name, fmt.Sprintf("%s allows creating a file but not removing it (the probe file %s was left behind): %v", base, probe, err),
			fmt.Sprintf("check the directory mode and mount options; UID %d needs full read and write access", os.Getuid()))
	}
	return ok(name, fmt.Sprintf("%s is writable (UID %d)", base, os.Getuid()))
}

// checkRunnerDirPermissions flags Runner install directories that others can enter.
//
// The directory holds .credentials_rsaparams, written by config.sh: the RSA private key the Runner uses to identify itself to GitHub.
// actions/runner does not set permissions on these files (on Unix they follow the umask, usually 0644), so the directory's
// permission bits are the last line of defense. Directories created starting with this version are 0700, but MkdirAll does not modify existing directories,
// so older deployments still have 0755.
//
// This only reports and does not change anything: under deployments with mismatched UIDs (Manager runs as root, the container uses app(1001)), tightening permissions on our own initiative
// would break what currently works, so a person should review it first.
func checkRunnerDirPermissions(cfg *config.Config) CheckResult {
	const name = "runner directory permissions"
	base := cfg.Runners.BasePath
	var loose []string
	for _, item := range cfg.Runners.Items {
		dir := item.InstallPath(base)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue // Directories not yet created are out of scope for this check; checkBasePath already covers the root directory
		}
		if info.Mode().Perm()&0o077 != 0 {
			loose = append(loose, dir)
		}
	}
	if len(loose) == 0 {
		return ok(name, "no runner directory is reachable by other local users")
	}
	sort.Strings(loose)
	return warn(name,
		fmt.Sprintf("%d runner directories are reachable by other users on the host: %s. "+
			"Each holds the .credentials_rsaparams that config.sh wrote — the private key the runner "+
			"authenticates to GitHub with. Reading it is enough to impersonate that runner, take its "+
			"jobs, and see the secrets passed to them",
			len(loose), strings.Join(loose, ", ")),
		"chmod 700 "+strings.Join(loose, " "))
}

// checkDefaultModeIsolation: in default mode, multiple Runners share one container and user:
// any Job can read other Runners' .credentials_rsaparams and PATs. It only warns and does not block startup.
//
// The warning is tiered by entry count rather than unconditional: with only one Runner there are no other Runners to worry about,
// and running several Runners for one person's single repository, just for concurrency, is a legitimate setup that needs awareness, not a block.
//
// PATs have already moved out of the Runner directory (internal/secrets), but that only closes the exposure in container mode:
// in default mode config/tokens/ lives in the same container, and the file owner is the Job itself, so 0600 does not help.
func checkDefaultModeIsolation(cfg *config.Config) CheckResult {
	const name = "runner isolation"
	if n := len(cfg.Runners.Items); n >= 2 {
		return warn(name,
			fmt.Sprintf("default mode: %d runners share one container and one user, so a job on "+
				"any of them can read the others' credentials", n),
			"set runners.container_mode: true to give each runner its own container (see SECURITY.md)")
	}
	return ok(name, "default mode with at most one runner")
}

// checkDefaultModeDocker: in default mode Jobs run inside the Manager container; this reports where docker inside a Job will connect
func checkDefaultModeDocker(ctx context.Context) CheckResult {
	const name = "Docker in jobs"
	h := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if h == "" {
		h = "unix://" + HostDockerSocket
	}
	if strings.HasPrefix(h, "tcp://") {
		// Reporting ok based on the prefix alone would show a green result when DinD is not running, while every docker call in a Job would fail
		addr := tcpAddr(strings.TrimPrefix(h, "tcp://"))
		if err := dialTCP(ctx, addr); err != nil {
			return warn(name, fmt.Sprintf("default mode, DOCKER_HOST points at %s, which is not reachable: %v", h, err),
				"docker compose --profile dind up -d, and check that DinD is running on the same network as the Manager")
		}
		return ok(name, fmt.Sprintf("default mode, docker in jobs will reach %s (DinD, reachable)", h))
	}
	sock := strings.TrimPrefix(h, "unix://")
	if _, err := os.Stat(sock); err != nil {
		return warn(name, fmt.Sprintf("default mode, but %s does not exist, so jobs cannot use docker", sock),
			"to give jobs Docker, mount -v /var/run/docker.sock:/var/run/docker.sock, or switch to DinD")
	}
	if gid := socketGID(sock); gid >= 0 && !inGroup(gid) {
		return warn(name, fmt.Sprintf("%s belongs to GID %d and this process is not in that group, so docker in jobs will report permission denied", sock, gid),
			fmt.Sprintf("set group_add: [\"%d\"] in docker-compose, or DOCKER_GID=%d in .env", gid, gid))
	}
	return ok(name, fmt.Sprintf("default mode, docker in jobs will use %s (jobs can control the host Docker daemon)", h))
}

// checkDockerReachable: in container mode, Manager must be able to operate the host Docker
func checkDockerReachable(ctx context.Context) CheckResult {
	const name = "Docker reachability"
	out, err := dockerCmd(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fail(name, "cannot reach the Docker daemon: "+msg, dockerAccessHint)
	}
	return ok(name, "Docker daemon version "+strings.TrimSpace(string(out)))
}

// checkNetwork: in container mode, Runner containers and Manager must be on the same network, otherwise Manager cannot reach the Agent.
// compose down removes networks that are not external; the docs list this pitfall explicitly.
func checkNetwork(ctx context.Context, cfg *config.Config) CheckResult {
	const name = "container network"
	network := cfg.Runners.ContainerNetwork
	if network == "" {
		network = "runner-net"
	}
	if _, err := dockerCmd(ctx, "network", "inspect", network); err != nil {
		return fail(name, fmt.Sprintf("the network %s does not exist, so runner containers cannot be created or started", network),
			"docker network create "+network)
	}
	return ok(name, "the network "+network+" exists")
}

// networkMembersFormat only takes the .Containers section of docker network inspect; its structure is
// map[ContainerID]{Name,...}. The other fields (IPAM, Options, Labels) are irrelevant to this check.
const networkMembersFormat = "{{json .Containers}}"

// maxListedNetworkMembers caps how many containers are named in the message. When dozens of containers are attached to the network, listing each one
// would make that line unreadable, while this check is meant to say "something does not belong here" -
// The first ten are enough for someone to run docker network inspect for the full list.
const maxListedNetworkMembers = 10

// anyDindBackend reports whether any runner actually uses dind. items[].job_docker_backend can override it per item,
// so the global value cannot be taken as a conclusion in either direction: a global none does not mean no runner has dind enabled on its own,
// and a global dind does not mean any runner is actually using it.
func anyDindBackend(cfg *config.Config) bool {
	// With no items configured: runners added later in the UI will inherit the global value, so it is evaluated by the global value
	if len(cfg.Runners.Items) == 0 {
		return cfg.JobDockerBackendFor("") == "dind"
	}
	for _, item := range cfg.Runners.Items {
		if cfg.JobDockerBackendFor(item.Name) == "dind" {
			return true
		}
	}
	return false
}

// checkNetworkMembers lists containers on runner-net that do not belong to this deployment.
//
// The DinD bundled with the repo runs privileged with TLS turned off (--tls=false in compose), so port 2375
// is open without authentication to any container on this network. Connecting gives full control of that daemon, and escaping from
// a privileged container to the host is not a high bar. So this network is a trust boundary, and anything extra on it
// is worth knowing about.
//
// Warn only, do not fail: an extra container is not necessarily a misconfiguration (attaching a monitoring sidecar to the same network is common),
// so what matters is awareness, not blocking startup.
func checkNetworkMembers(ctx context.Context, cfg *config.Config) CheckResult {
	const name = "container network members"
	network := cfg.Runners.ContainerNetwork
	if network == "" {
		network = "runner-net"
	}
	out, err := dockerCmd(ctx, "network", "inspect", network, "--format", networkMembersFormat)
	if err != nil {
		// Cases like "network does not exist" are already reported as a failure by checkNetwork, so reporting it here again
		// would just say the same thing twice, in a more convoluted way.
		return ok(name, fmt.Sprintf("could not read the members of %s (skipped): %s", network, firstLine(out, err)))
	}
	// Only declare the Name that is used; other fields (EndpointID, IPv4Address, ...) are ignored by json
	var raw map[string]struct{ Name string }
	if err := json.Unmarshal(out, &raw); err != nil {
		return ok(name, fmt.Sprintf("could not parse the members of %s (skipped): %v", network, err))
	}
	members := make(map[string]string, len(raw))
	for id, m := range raw {
		members[id] = m.Name
	}
	// When the hostname cannot be obtained, pass an empty string; unexpectedNetworkMembers will then skip the "self" check:
	// It is better to list the Manager itself than to let an empty prefix count the entire network as self.
	selfHostname, _ := os.Hostname()
	unexpected := unexpectedNetworkMembers(members, selfHostname, expectedNetworkMembers(cfg))
	if len(unexpected) == 0 {
		return ok(name, "the network "+network+" has no unexpected members")
	}
	return warn(name,
		fmt.Sprintf("%d containers on %s are not part of this deployment and can reach the unauthenticated "+
			"DinD daemon: %s", len(unexpected), network, listNetworkMembers(unexpected)),
		fmt.Sprintf("disconnect them (docker network disconnect %s <name>) or move this deployment to its own network", network))
}

// expectedNetworkMembers are the container names this deployment itself attaches to this network: the DinD and each runner container.
// The Manager is not in this list, since the config has no container name for it; it is recognized by hostname.
func expectedNetworkMembers(cfg *config.Config) map[string]bool {
	dindHost := cfg.Runners.DindHost
	if dindHost == "" {
		dindHost = "runner-dind"
	}
	expected := map[string]bool{dindHost: true}
	for _, item := range cfg.Runners.Items {
		expected[ContainerName(item.Name)] = true
	}
	return expected
}

// unexpectedNetworkMembers picks out the network members that do not belong to this deployment and returns them sorted by name.
// members maps container ID -> container name, i.e. the .Containers of docker network inspect.
// It is a pure function so test cases can feed it a member table directly, without needing a Docker first.
//
// The Manager identifies itself by hostname: when no hostname is set, Docker sets it to the 12-character short ID
// (moby's daemon/container.go: default hostname is the container's short-ID),
// whereas compose only passes this field when hostname: is written (in pkg/compose/create.go
// Hostname takes service.Hostname directly); container_name does not factor in. This repo's compose
// only sets container_name, so it takes exactly the short-ID path. Deployments that set hostname: fall back to matching by name as well
// so the Manager is not reported as an intruder.
//
// When selfHostname is empty, skip this check: HasPrefix(id, "") is always true, which would count the entire network as self,
// silently turning this check into one that always passes.
func unexpectedNetworkMembers(members map[string]string, selfHostname string, expected map[string]bool) []string {
	var unexpected []string
	for id, member := range members {
		if selfHostname != "" && (strings.HasPrefix(id, selfHostname) || member == selfHostname) {
			continue
		}
		if expected[member] {
			continue
		}
		if member == "" {
			// Endpoints without a name are identified by short ID, otherwise the message would have an empty slot that leaves the reader with nothing to go on
			member = shortContainerID(id)
		}
		unexpected = append(unexpected, member)
	}
	sort.Strings(unexpected)
	return unexpected
}

// shortContainerID takes the first 12 characters of the container ID, matching what docker ps shows.
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// listNetworkMembers builds the names segment of the message, and anything beyond maxListedNetworkMembers is collapsed into a count.
func listNetworkMembers(names []string) string {
	if len(names) <= maxListedNetworkMembers {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, …and %d more",
		strings.Join(names[:maxListedNetworkMembers], ", "), len(names)-maxListedNetworkMembers)
}

// requiredRunnerTools are commands that jobs commonly depend on. Missing any one of them fails in ways that are hard to trace:
// without git, actions/checkout silently falls back to downloading a tar archive (the working directory has no .git),
// without unzip, actions like setup-gradle only fail after the distribution package has been downloaded.
// No sudo: command -v sudo only proves the command exists, not that the Job user is in sudoers,
// which would give a false guarantee that the self-check passes but sudo apt-get still fails. Passwordless sudo is guaranteed by the image build
// (ALLOW_SUDO in both Dockerfiles), not by a runtime probe.
var requiredRunnerTools = []string{"git", "unzip", "tar", "curl", "jq"}

// RunnerImages returns all Runner images used in the config (deduplicated, stable order).
// Since items[].container_image supports per-Runner overrides, there may be more than one image.
func RunnerImages(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	var images []string
	add := func(img string) {
		img = strings.TrimSpace(img)
		if img == "" || seen[img] {
			return
		}
		seen[img] = true
		images = append(images, img)
	}
	// Runners without items configured (including ones added later in the UI) use the global image, so it is always in the list
	add(cfg.Runners.ContainerImage)
	if len(images) == 0 {
		add(config.DefaultRunnerContainerImage())
	}
	for _, item := range cfg.Runners.Items {
		add(cfg.ContainerImageFor(item.Name))
	}
	return images
}

// checkRunnerImages checks each image used by the config: whether it exists locally, and whether it has
// the base commands a Job needs. An image not being local is not fatal (docker create pulls it automatically).
func checkRunnerImages(ctx context.Context, cfg *config.Config) []CheckResult {
	var results []CheckResult
	for _, img := range RunnerImages(cfg) {
		present := runCheck(ctx, "runner image", func(ctx context.Context) CheckResult {
			if _, err := dockerCmd(ctx, "image", "inspect", img); err != nil {
				return warn("runner image",
					fmt.Sprintf("%s is not present locally; it is pulled the first time a runner starts (a private registry needs docker login first)", img),
					"docker pull "+img)
			}
			return ok("runner image", img+" is ready")
		})
		results = append(results, present)
		// Toolchain checks only run when the image is already local, to avoid triggering an image pull during the self-check
		if present.Level != CheckOK {
			continue
		}
		results = append(results, runCheck(ctx, "runner image toolchain", func(ctx context.Context) CheckResult {
			return checkRunnerImageTools(ctx, img)
		}))
	}
	return results
}

// missingToolMarker is the line prefix the probe script prints for each missing command.
// dockerCmd uses CombinedOutput, so non-fatal docker warnings (e.g. platform mismatch when running an amd64
// image on an arm64 host) get mixed in with exit code 0; splitting on whitespace would treat every word
// of the warning as a missing command name. With a marker, only lines we printed are recognized.
const missingToolMarker = "RUNNER_DECK_MISSING:"

// parseMissingTools extracts missing command names from the probe output.
// Only marked lines are accepted, and the name must be in requiredRunnerTools, so even if the warning text
// happens to contain the marker, it cannot forge a command name.
func parseMissingTools(out []byte) []string {
	known := make(map[string]bool, len(requiredRunnerTools))
	for _, t := range requiredRunnerTools {
		known[t] = true
	}
	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), missingToolMarker)
		if !ok {
			continue
		}
		if name = strings.TrimSpace(name); known[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// checkRunnerImageTools starts a one-off container to confirm the image has requiredRunnerTools.
// Custom Runner images often miss these, and the gap would only surface halfway through a Job.
func checkRunnerImageTools(ctx context.Context, img string) CheckResult {
	const name = "runner image toolchain"
	// The image's ENTRYPOINT is Agent; override it with a shell here. --network none avoids network setup overhead
	script := "for t in " + strings.Join(requiredRunnerTools, " ") +
		"; do command -v \"$t\" >/dev/null 2>&1 || echo \"" + missingToolMarker + "$t\"; done"
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := dockerCmd(runCtx, "run", "--rm", "--network", "none", "--entrypoint", "sh", img, "-c", script)
	if err != nil {
		return warn(name, fmt.Sprintf("could not inspect the commands inside %s (skipped): %s", img, firstLine(out, err)),
			"check it by hand: docker run --rm --entrypoint sh "+img+" -c \"command -v git unzip\"")
	}
	missing := parseMissingTools(out)
	if len(missing) > 0 {
		return warn(name, fmt.Sprintf("%s is missing %s; a job only fails when it reaches them (without git, "+
			"actions/checkout silently degrades to a tarball with no .git)",
			img, strings.Join(missing, ", ")),
			"install those packages in a custom image; see examples/runner-images/")
	}
	return ok(name, img+" has "+strings.Join(requiredRunnerTools, ", "))
}

// firstLine takes the first line of command output or error, so the full docker output is not stuffed into the self-check result
func firstLine(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// checkJobDockerBackend checks the prerequisites for Docker inside a Job according to job_docker_backend
func checkJobDockerBackend(ctx context.Context, cfg *config.Config) CheckResult {
	const name = "Docker in jobs"
	switch strings.ToLower(strings.TrimSpace(cfg.Runners.JobDockerBackend)) {
	case "host-socket":
		if _, err := os.Stat(HostDockerSocket); err != nil {
			return fail(name, HostDockerSocket+" does not exist, so it cannot be mounted into runner containers",
				"mount -v /var/run/docker.sock:/var/run/docker.sock onto runner-manager in docker-compose")
		}
		gid := runnerDockerGID(cfg)
		if gid < 0 {
			return warn(name, "could not determine the group owning docker.sock, so --group-add is not passed when creating runner containers and docker in jobs may report permission denied",
				"set runners.docker_gid in the config, or DOCKER_GID in .env")
		}
		return ok(name, fmt.Sprintf("host-socket; runner containers get --group-add %d", gid))
	case "none":
		return ok(name, "none; jobs get no Docker")
	default: // dind
		host := cfg.Runners.DindHost
		if host == "" {
			host = "runner-dind"
		}
		addr := net.JoinHostPort(host, "2375")
		if err := dialTCP(ctx, addr); err != nil {
			return warn(name, fmt.Sprintf("dind, but %s is not reachable: %v", addr, err),
				"docker compose --profile dind up -d, or set job_docker_backend to none if jobs do not need Docker")
		}
		return ok(name, "dind; "+addr+" is reachable")
	}
}

// tcpAddr fills in the default port, since DOCKER_HOST may be written as tcp://host without a port
func tcpAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, "2375")
	}
	return addr
}

// dialTCP checks TCP endpoint reachability, reused by the DinD checks in default and container modes
func dialTCP(ctx context.Context, addr string) error {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// inGroup reports whether the current process belongs to the GID (primary or supplementary group)
func inGroup(gid int) bool {
	if os.Getgid() == gid {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return false
}

// preflightLogf is replaceable for tests; defaults to the standard library log
var preflightLogf = log.Printf

// PreflightLogMarker is the line-start marker for every self-check log line.
//
// Docs in all six languages make "check the startup self-check first" the first troubleshooting step, with the command
// `docker compose logs runner-manager | grep '\[preflight'`. This marker was originally
// "[Self-check …]" — so the five documents in English, French, German, Japanese and Korean each contain a string that a non-Chinese user cannot type,
// nor understand. The log body is still in Chinese for now (that is a bigger task),
// but at least how to pull these lines out no longer depends on the reader knowing Chinese characters.
//
// Extracted as a constant so preflight_test.go can enforce it: the commands in the docs depend on this literal,
// and changing it without updating all six docs would silently make the first troubleshooting step grep for nothing.
const PreflightLogMarker = "[preflight"

// LogPreflight logs self-check results by level, with error items also getting a fix suggestion
func LogPreflight(results []CheckResult) {
	for _, r := range results {
		prefix := PreflightLogMarker + " ✓]"
		switch r.Level {
		case CheckWarn:
			prefix = PreflightLogMarker + " !]"
		case CheckError:
			prefix = PreflightLogMarker + " ✗]"
		}
		line := fmt.Sprintf("%s %s: %s", prefix, r.Name, r.Message)
		if r.Hint != "" && r.Level != CheckOK {
			line += "  → hint: " + r.Hint
		}
		preflightLogf("%s", line)
	}
}
