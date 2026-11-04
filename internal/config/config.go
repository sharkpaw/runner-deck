package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/sharkpaw/runner-deck/internal/atomicfile"
	"github.com/soulteary/cli-kit/env"
	"github.com/soulteary/cli-kit/validator"
	// go.yaml.in/yaml/v3 is the continuation of gopkg.in/yaml.v3: same code, same API, same
	// yaml package name and struct tags, while the latter stopped at its last release in 2022 and is no longer published. After switching
	// Marshal output is unchanged byte for byte (full config, empty config, and config.yaml.example round-trip all pass),
	// so the existing config.yaml in production needs no byte changed. i18n-kit's yamlloader did the same
	// the same thing.
	"go.yaml.in/yaml/v3"
)

// mu guards reads and writes of the config file, preventing overwrites from concurrent writes.
//
// Only writers are serialized; Load deliberately takes no lock: Save publishes via atomicfile using rename, so the target name
// is either the old or the new content, readers never see a partial file, and it also covers crash scenarios that a read lock could not block.
var mu sync.Mutex
var runnerContainerNameSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// DefaultRunnerImageRepo is the default Runner image repository name, same repository as the Manager
const DefaultRunnerImageRepo = "ghcr.io/sharkpaw/runner-deck"

// DefaultJobDockerBackend is the default backend when job_docker_backend is not configured
const DefaultJobDockerBackend = "dind"

// JobDockerBackends is the full set of valid values for job_docker_backend.
// Kept as a package-level variable rather than a local map scattered in Validate: both the global config and items[] must be validated,
// the error message is also built from it, so a change in one place suffices and you never get 'a value was added but the error still lists the old set'.
var JobDockerBackends = []string{"dind", "host-socket", "none"}

// DefaultRunnerContainerImage returns the default Runner container image (used when container_image is not configured).
// Tag is taken from the FLEET_IMAGE_TAG environment variable, defaulting to v2.0.0 when unset; the image name is {repo}:{tag}-runner.
func DefaultRunnerContainerImage() string {
	// Deliberately not rewritten as env.GetTrimmed("FLEET_IMAGE_TAG", "v2.0.0"):
	// The version consistency check (ci-recipes runner-fleet check-version-consistency, regex written in
	// version_baseline_regex in scripts/ci-recipes.conf) uses `tag = "vX.Y.Z"` to read the baseline version from this file
	// across the repo; after switching to a function call that regex no longer matches, and the check would fail directly with
	// 'cannot parse default image tag'. Keep this literal shape.
	tag := strings.TrimSpace(os.Getenv("FLEET_IMAGE_TAG"))
	if tag == "" {
		tag = "v2.0.0"
	}
	return DefaultRunnerImageRepo + ":" + tag + "-runner"
}

// containerImageFromManagerImage derives the Runner image from MANAGER_IMAGE (image:tag -> image:tag-runner).
// Without a tag it returns image:latest-runner; when empty or parsing fails it returns an empty string.
func containerImageFromManagerImage() string {
	raw := env.GetTrimmed("MANAGER_IMAGE", "")
	if raw == "" {
		return ""
	}
	lastColon := strings.LastIndex(raw, ":")
	if lastColon == -1 {
		return raw + ":latest-runner"
	}
	image, tag := raw[:lastColon], raw[lastColon+1:]
	if tag == "" {
		return image + ":latest-runner"
	}
	return image + ":" + tag + "-runner"
}

// applyEnvOverrides overrides config fields with environment variables (only when env is non-empty).
// It should be called in Load after default handling and before Validate.
func applyEnvOverrides(c *Config) {
	// The semantics of env.GetTrimmed match what is needed here exactly: it returns the passed-in original value when the variable is unset or empty after trimming.
	// So 'override only when the env var is non-empty' need not be written as separate if blocks; it reads as an override table.
	//
	// When one field has two variable names, order is priority: the later line overrides the earlier one.
	c.Server.Addr = env.GetTrimmed("SERVER_ADDR", c.Server.Addr)
	c.Runners.BasePath = env.GetTrimmed("RUNNERS_BASE_PATH", c.Runners.BasePath)
	c.Runners.ContainerImage = env.GetTrimmed("RUNNER_IMAGE", c.Runners.ContainerImage)
	c.Runners.ContainerImage = env.GetTrimmed("CONTAINER_IMAGE", c.Runners.ContainerImage)
	c.Runners.ContainerNetwork = env.GetTrimmed("CONTAINER_NETWORK", c.Runners.ContainerNetwork)
	c.Runners.VolumeHostPath = env.GetTrimmed("VOLUME_HOST_PATH", c.Runners.VolumeHostPath)
	c.Runners.VolumeHostPath = env.GetTrimmed("RUNNERS_VOLUME_HOST_PATH", c.Runners.VolumeHostPath)

	// The port deliberately does not use env.GetInt: it does not trim, and in .env and compose environment
	// a trailing space is common, in which case MANAGER_PORT=9090 would be silently dropped and it would keep listening on 8080.
	// Here it still does 'trim then parse' as-is, and range checking is left to Validate; silently ignoring an
	// out-of-range port here is harder to debug than letting it reach Validate and report a clear error.
	for _, key := range []string{"MANAGER_PORT", "SERVER_PORT"} {
		if v := env.GetTrimmed(key, ""); v != "" {
			if port, err := strconv.Atoi(v); err == nil && port > 0 {
				c.Server.Port = port
			}
		}
	}
	if v := env.GetTrimmed("DOCKER_GID", ""); v != "" {
		if gid, err := strconv.Atoi(v); err == nil && gid >= 0 {
			c.Runners.DockerGID = gid
		}
	}

	// Likewise, not env.GetBool: it goes through strconv.ParseBool, which does not trim, and also lets
	// CONTAINER_MODE=false turn off container mode that is already enabled in the config file.
	// This variable has only ever been able to enable, never disable; keep it as is, so one deployment change does not silently alter the run mode.
	if v := strings.ToLower(env.GetTrimmed("CONTAINER_MODE", "")); v == "true" || v == "1" {
		c.Runners.ContainerMode = true
	}
	if v := normalizeJobDockerBackend(env.GetTrimmed("JOB_DOCKER_BACKEND", "")); v != "" {
		c.Runners.JobDockerBackend = v
	}

	// When container mode is on and container_image is not set, derive from MANAGER_IMAGE first
	if c.Runners.ContainerMode && strings.TrimSpace(c.Runners.ContainerImage) == "" {
		if derived := containerImageFromManagerImage(); derived != "" {
			c.Runners.ContainerImage = derived
		} else {
			c.Runners.ContainerImage = DefaultRunnerContainerImage()
		}
	}
}

// Config application configuration
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Runners RunnersConfig `yaml:"runners"`
}

// ServerConfig HTTP server configuration
type ServerConfig struct {
	Port int    `yaml:"port"`
	Addr string `yaml:"addr"`
}

// RunnersConfig root Runner configuration
type RunnersConfig struct {
	BasePath string       `yaml:"base_path"` // root directory where all runners are installed
	Items    []RunnerItem `yaml:"items"`

	// Container mode: Runners run in separate containers, the Manager starts and stops them via the Docker API and gets status through the Agent
	ContainerMode    bool   `yaml:"container_mode"`    // When true, start/stop and status all go through the container
	ContainerImage   string `yaml:"container_image"`   // Runner container image; if unset, DefaultRunnerContainerImage() decides (FLEET_IMAGE_TAG or v2.0.0)
	ContainerNetwork string `yaml:"container_network"` // Network the container joins, same as the Manager so it can reach the Agent; default runner-net
	AgentPort        int    `yaml:"agent_port"`        // Agent port inside the container; default 8081
	// Job Docker backend: the backend used when jobs run docker commands inside the Runner container. dind=DinD service; host-socket=mount the host socket; none=no Docker provided
	JobDockerBackend string `yaml:"job_docker_backend"` // dind | host-socket | none; default dind
	DindHost         string `yaml:"dind_host"`          // Only used when job_docker_backend=dind; DinD hostname; default runner-dind
	VolumeHostPath   string `yaml:"volume_host_path"`   // Host-side root path for runners in container mode, used by docker create -v; required when the Manager itself runs in a container (e.g. /data/runners)
	// Resources are the resource limits applied when creating Runner containers in container mode.
	// Without limits, a single runaway job (such as a Gradle daemon) can exhaust the machine's memory and take the Manager down with it.
	Resources ResourceLimits `yaml:"resources,omitempty"`
	// DockerGID is only used when job_docker_backend=host-socket: the docker group GID added when creating the Runner container (--group-add),
	// so the in-container app (UID 1001) can access the mounted docker.sock; 0 means auto-detect the group that owns docker.sock
	DockerGID int `yaml:"docker_gid"`
}

// ResourceLimits are the Runner container resource limits; leaving a field empty means no limit.
// Values are passed straight through to docker create, with the same semantics as Docker's.
type ResourceLimits struct {
	CPUs       string `yaml:"cpus,omitempty"`        // --cpus, e.g. "2" or "1.5"
	Memory     string `yaml:"memory,omitempty"`      // --memory, e.g. "4g", "512m"
	MemorySwap string `yaml:"memory_swap,omitempty"` // --memory-swap, e.g. "4g"; set to "-1" for unlimited swap
	PidsLimit  int    `yaml:"pids_limit,omitempty"`  // --pids-limit; positive values apply, -1 means unlimited
}

// dockerCPUsRe matches the positive numbers docker --cpus accepts (e.g. 2, 1.5, 0.25)
var dockerCPUsRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// dockerSizeRe matches Docker's memory size formats: a plain byte count, or one with a b/k/m/g unit
var dockerSizeRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bkmgBKMG]?$`)

// Args converts the resource limits into docker create arguments; unset fields produce no arguments
func (r ResourceLimits) Args() []string {
	var args []string
	if v := strings.TrimSpace(r.CPUs); v != "" {
		args = append(args, "--cpus", v)
	}
	if v := strings.TrimSpace(r.Memory); v != "" {
		args = append(args, "--memory", v)
	}
	if v := strings.TrimSpace(r.MemorySwap); v != "" {
		args = append(args, "--memory-swap", v)
	}
	if r.PidsLimit != 0 {
		args = append(args, "--pids-limit", strconv.Itoa(r.PidsLimit))
	}
	return args
}

// Validate checks value formats, so invalid values fail here instead of being passed to docker and only erroring when the container is created
func (r ResourceLimits) Validate() error {
	if v := strings.TrimSpace(r.CPUs); v != "" {
		// The regex rejects forms like 1e3 or NaN that ParseFloat accepts but docker does not, then requires the value to be positive
		f, err := strconv.ParseFloat(v, 64)
		if !dockerCPUsRe.MatchString(v) || err != nil || f <= 0 {
			return fmt.Errorf("runners.resources.cpus must be a positive number (for example \"2\" or \"1.5\"), got %q", r.CPUs)
		}
	}
	if v := strings.TrimSpace(r.Memory); v != "" && !dockerSizeRe.MatchString(v) {
		return fmt.Errorf("runners.resources.memory must use docker memory notation (for example \"512m\" or \"4g\"), got %q", r.Memory)
	}
	if v := strings.TrimSpace(r.MemorySwap); v != "" && v != "-1" && !dockerSizeRe.MatchString(v) {
		return fmt.Errorf("runners.resources.memory_swap must use docker memory notation or be \"-1\", got %q", r.MemorySwap)
	}
	if r.PidsLimit < -1 {
		return fmt.Errorf("runners.resources.pids_limit must be a positive number, or -1 for unlimited, got %d", r.PidsLimit)
	}
	memory := strings.TrimSpace(r.Memory)
	swap := strings.TrimSpace(r.MemorySwap)
	if swap != "" && memory == "" {
		return fmt.Errorf("runners.resources.memory_swap needs memory set as well, or docker refuses to create the container")
	}
	// docker requires memory-swap (total memory plus swap) to be at least memory, otherwise it only errors when the container is created
	if swap != "" && swap != "-1" && memory != "" {
		memBytes, memErr := parseDockerSize(memory)
		swapBytes, swapErr := parseDockerSize(swap)
		if memErr == nil && swapErr == nil && swapBytes < memBytes {
			return fmt.Errorf("runners.resources.memory_swap(%s) cannot be smaller than memory(%s): it is the total of memory plus swap, and docker refuses to create the container", r.MemorySwap, r.Memory)
		}
	}
	return nil
}

// parseDockerSize parses Docker's memory format into a byte count, for comparing across units.
// Accepts a plain byte count or one with a b/k/m/g suffix (case-insensitive), consistent with dockerSizeRe.
func parseDockerSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if !dockerSizeRe.MatchString(v) {
		return 0, fmt.Errorf("not a valid docker memory value: %q", v)
	}
	mult := int64(1)
	switch last := v[len(v)-1]; last {
	case 'b', 'B':
		v = v[:len(v)-1]
	case 'k', 'K':
		mult, v = 1024, v[:len(v)-1]
	case 'm', 'M':
		mult, v = 1024*1024, v[:len(v)-1]
	case 'g', 'G':
		mult, v = 1024*1024*1024, v[:len(v)-1]
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("not a valid docker memory value: %q", v)
	}
	return int64(n * float64(mult)), nil
}

// RunnerItem is the configuration for a single Runner
type RunnerItem struct {
	Name       string   `yaml:"name"`        // Display name, also used as the directory name
	Path       string   `yaml:"path"`        // Directory relative to base_path; if empty, name is used
	TargetType string   `yaml:"target_type"` // org | repo
	Target     string   `yaml:"target"`      // org name or owner/repo
	Labels     []string `yaml:"labels"`      // Custom labels

	// The following apply only in container mode; if left empty, fall back to the global config of the same name under runners.
	// Different projects on one machine often need different toolchains (e.g. Flutter+Android vs Node),
	// a single global image cannot cover them all, so each Runner may override it.
	ContainerImage   string `yaml:"container_image,omitempty"`    // Container image used by this Runner
	JobDockerBackend string `yaml:"job_docker_backend,omitempty"` // dind | host-socket | none
}

// FindItem looks up a Runner config item by name
func (c *Config) FindItem(name string) (RunnerItem, bool) {
	for _, item := range c.Runners.Items {
		if item.Name == name {
			return item, true
		}
	}
	return RunnerItem{}, false
}

// ContainerImageFor returns the container image this Runner actually uses, in priority order:
// items[].container_image > runners.container_image > DefaultRunnerContainerImage()
func (c *Config) ContainerImageFor(runnerName string) string {
	if item, ok := c.FindItem(runnerName); ok {
		if img := strings.TrimSpace(item.ContainerImage); img != "" {
			return img
		}
	}
	if img := strings.TrimSpace(c.Runners.ContainerImage); img != "" {
		return img
	}
	return DefaultRunnerContainerImage()
}

// JobDockerBackendFor returns the Job Docker backend this Runner actually uses, in priority order:
// items[].job_docker_backend > runners.job_docker_backend > dind
func (c *Config) JobDockerBackendFor(runnerName string) string {
	if item, ok := c.FindItem(runnerName); ok {
		if b := normalizeJobDockerBackend(item.JobDockerBackend); b != "" {
			return b
		}
	}
	if b := normalizeJobDockerBackend(c.Runners.JobDockerBackend); b != "" {
		return b
	}
	return DefaultJobDockerBackend
}

// normalizeJobDockerBackend trims whitespace and lowercases the value; an empty value returns an empty string so the caller can fall back
func normalizeJobDockerBackend(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// InstallPath returns the full install path of this runner
func (r RunnerItem) InstallPath(basePath string) string {
	dir := r.Path
	if dir == "" {
		dir = r.Name
	}
	return filepath.Join(basePath, filepath.Clean(dir))
}

// defaultConfig returns the config matching the defaults in Load (reads no file, applies no env vars), used to build config from env when the file does not exist.
func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Port: 8080,
			Addr: "0.0.0.0",
		},
		Runners: RunnersConfig{
			BasePath:         "./runners",
			Items:            []RunnerItem{},
			ContainerMode:    false,
			ContainerImage:   "",
			ContainerNetwork: "runner-net",
			AgentPort:        8081,
			JobDockerBackend: DefaultJobDockerBackend,
			DindHost:         "runner-dind",
			VolumeHostPath:   "",
			DockerGID:        0, // 0 = auto-detect the group that owns docker.sock
		},
	}
}

// Load loads the config from file. If the file does not exist, it builds the config from the defaults and env vars, writes it to path, then returns it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c := defaultConfig()
			applyEnvOverrides(c)
			if err := Validate(c); err != nil {
				return nil, err
			}
			dir := filepath.Dir(path)
			if mkdirErr := os.MkdirAll(dir, 0755); mkdirErr != nil {
				log.Printf("warning: cannot create the config directory %s: %v, the config file will not be written", dir, mkdirErr)
			} else if saveErr := c.Save(path); saveErr != nil {
				log.Printf("warning: cannot write the config to %s: %v, continuing with the in-memory config", path, saveErr)
			}
			return c, nil
		}
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	// Defaults
	c.Server.Addr = strings.TrimSpace(c.Server.Addr)
	c.Runners.BasePath = strings.TrimSpace(c.Runners.BasePath)
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Runners.BasePath == "" {
		c.Runners.BasePath = "./runners"
	}
	c.Runners.ContainerImage = strings.TrimSpace(c.Runners.ContainerImage)
	c.Runners.ContainerNetwork = strings.TrimSpace(c.Runners.ContainerNetwork)
	c.Runners.DindHost = strings.TrimSpace(c.Runners.DindHost)
	c.Runners.VolumeHostPath = strings.TrimSpace(c.Runners.VolumeHostPath)
	if c.Runners.ContainerMode && c.Runners.ContainerImage == "" {
		c.Runners.ContainerImage = DefaultRunnerContainerImage()
	}
	if c.Runners.ContainerNetwork == "" {
		c.Runners.ContainerNetwork = "runner-net"
	}
	if c.Runners.AgentPort <= 0 {
		c.Runners.AgentPort = 8081
	}
	jobBackend := normalizeJobDockerBackend(c.Runners.JobDockerBackend)
	if jobBackend == "" {
		jobBackend = DefaultJobDockerBackend
	}
	c.Runners.JobDockerBackend = jobBackend
	if c.Runners.JobDockerBackend == DefaultJobDockerBackend && c.Runners.DindHost == "" {
		c.Runners.DindHost = "runner-dind"
	}
	applyEnvOverrides(&c)
	if err := Validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the config: runner name conflicts, etc.
func Validate(c *Config) error {
	seen := make(map[string]bool)
	seenContainerNames := make(map[string]string)
	seenInstallPaths := make(map[string]string)
	jobBackend := normalizeJobDockerBackend(c.Runners.JobDockerBackend)
	if jobBackend == "" {
		jobBackend = DefaultJobDockerBackend
		c.Runners.JobDockerBackend = jobBackend
	}
	if validator.ValidateEnumCaseInsensitive(jobBackend, JobDockerBackends) != nil {
		return fmt.Errorf("runners.job_docker_backend only supports %s, got %q",
			strings.Join(JobDockerBackends, "/"), c.Runners.JobDockerBackend)
	}
	// Port 0 means "not set"; Load fills in the default, so it is not checked here. Only non-zero values are range-checked.
	// Previously each site only required > 0, so MANAGER_PORT=70000 would flow all the way into the config until
	// ListenAndServe failed with a bare "invalid port" that did not mention any config key.
	if c.Server.Port != 0 {
		if err := validator.ValidatePort(c.Server.Port); err != nil {
			return fmt.Errorf("server.port must be between 1 and 65535, got %d (it may also come from MANAGER_PORT/SERVER_PORT)", c.Server.Port)
		}
	}
	if err := c.Runners.Resources.Validate(); err != nil {
		return err
	}
	if c.Runners.DockerGID < 0 {
		return fmt.Errorf("runners.docker_gid cannot be negative (got %d); leave it empty or 0 to detect the group owning docker.sock", c.Runners.DockerGID)
	}
	if !c.Runners.ContainerMode {
		if strings.TrimSpace(c.Runners.VolumeHostPath) != "" {
			return fmt.Errorf("runners.volume_host_path can only be set when container_mode=true")
		}
		if jobBackend != "dind" {
			return fmt.Errorf("runners.job_docker_backend must be dind when container_mode=false (got %q)", jobBackend)
		}
	}
	if c.Runners.ContainerMode {
		if strings.TrimSpace(c.Runners.VolumeHostPath) != "" && !filepath.IsAbs(c.Runners.VolumeHostPath) {
			return fmt.Errorf("runners.volume_host_path must be an absolute path on the host")
		}
		baseClean := filepath.Clean(c.Runners.BasePath)
		if strings.TrimSpace(c.Runners.VolumeHostPath) == "" && strings.HasPrefix(baseClean, "/app") {
			return fmt.Errorf("runners.volume_host_path must be set when container_mode=true and base_path=%s (the absolute path of the runners base directory on the host)", c.Runners.BasePath)
		}
	}
	for i, item := range c.Runners.Items {
		name := strings.TrimSpace(item.Name)
		path := strings.TrimSpace(item.Path)
		targetType := strings.ToLower(strings.TrimSpace(item.TargetType))
		target := strings.TrimSpace(item.Target)
		if name == "" {
			return fmt.Errorf("runners.items[%d].name cannot be empty", i)
		}
		if !IsSafeRunnerNameOrPath(name) {
			return fmt.Errorf("runners.items[%d].name contains an illegal character (.. / \\\\ are not allowed): %s", i, name)
		}
		if path != "" && !IsSafeRunnerNameOrPath(path) {
			return fmt.Errorf("runners.items[%d].path contains an illegal character (.. / \\\\ are not allowed): %s", i, path)
		}
		if err := ValidateTarget(targetType, target); err != nil {
			return fmt.Errorf("runners.items[%d]: %w", i, err)
		}
		itemBackend := normalizeJobDockerBackend(item.JobDockerBackend)
		itemImage := strings.TrimSpace(item.ContainerImage)
		if itemBackend != "" && validator.ValidateEnumCaseInsensitive(itemBackend, JobDockerBackends) != nil {
			return fmt.Errorf("runners.items[%d].job_docker_backend only supports %s, got %q",
				i, strings.Join(JobDockerBackends, "/"), item.JobDockerBackend)
		}
		if !c.Runners.ContainerMode {
			// Consistent with how runners.volume_host_path is handled: container-only fields may not be set outside container mode,
			// so the config does not appear to take effect while actually being ignored
			if itemImage != "" {
				return fmt.Errorf("runners.items[%d].container_image can only be set when container_mode=true", i)
			}
			if itemBackend != "" {
				return fmt.Errorf("runners.items[%d].job_docker_backend can only be set when container_mode=true", i)
			}
		}
		if seen[name] {
			return fmt.Errorf("runners.items has two runners with the same name: %s", name)
		}
		seen[name] = true
		installPath := item.InstallPath(c.Runners.BasePath)
		installKey := filepath.Clean(installPath)
		if existing, ok := seenInstallPaths[installKey]; ok {
			return fmt.Errorf("runners.items has an install directory conflict: %s and %s both map to %s", existing, name, installKey)
		}
		seenInstallPaths[installKey] = name
		if c.Runners.ContainerMode {
			containerName := NormalizedContainerName(name)
			if existing, ok := seenContainerNames[containerName]; ok {
				return fmt.Errorf("runners.items has a container name conflict after mapping: %s and %s both map to %s", existing, name, containerName)
			}
			seenContainerNames[containerName] = name
		}
	}
	return nil
}

// NormalizedContainerName converts a runner name into a valid container name (keeps only letters, digits and hyphens, and adds a prefix), shared by the config and runner packages
func NormalizedContainerName(name string) string {
	safe := runnerContainerNameSanitizeRe.ReplaceAllString(name, "-")
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "runner"
	}
	return "github-runner-" + safe
}

// IsSafeRunnerNameOrPath checks that name/path has no path traversal or illegal characters (forbids .. / \)
func IsSafeRunnerNameOrPath(s string) bool {
	if s == "" {
		return false
	}
	return !strings.Contains(s, "..") && !strings.Contains(s, "/") && !strings.Contains(s, "\\")
}

// ValidateTarget checks the target format: org is an organization name (no /), repo is owner/repo (exactly one slash, non-empty on both sides)
func ValidateTarget(targetType, target string) error {
	t := strings.TrimSpace(target)
	if t == "" {
		return fmt.Errorf("target cannot be empty")
	}
	tt := strings.ToLower(strings.TrimSpace(targetType))
	switch tt {
	case "org":
		if strings.Contains(t, "/") {
			return fmt.Errorf("when target_type is org, target must be the organization name and cannot contain /")
		}
		return nil
	case "repo":
		parts := strings.SplitN(t, "/", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return fmt.Errorf("when target_type is repo, target must be owner/repo, with both parts non-empty")
		}
		if strings.Contains(parts[1], "/") {
			return fmt.Errorf("target can contain only one /, in the form owner/repo")
		}
		return nil
	default:
		return fmt.Errorf("target_type must be org or repo")
	}
}

// Save writes the config back to file. The caller must take the lock itself; for writes, use LoadAndSave
func (c *Config) Save(path string) error {
	if err := Validate(c); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	// Replace atomically instead of os.WriteFile: every API request, the frontend's 15-second poll, and the background loops
	// all read this file, so truncating and rewriting it in place would let them read an empty or half-written YAML (when reading empty,
	// yaml.Unmarshal reports no error, and the UI briefly shows "no runners at all").
	return atomicfile.WriteFile(path, data, 0644)
}

// LoadAndSave loads the config under the lock, runs fn, then writes it back. Used by every write that modifies the config, to avoid concurrent overwrites
func LoadAndSave(path string, fn func(*Config) error) error {
	mu.Lock()
	defer mu.Unlock()
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	if err := Validate(cfg); err != nil {
		return err
	}
	return cfg.Save(path)
}
