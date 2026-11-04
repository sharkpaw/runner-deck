// Conflict pre-check before adding a runner. While the UI is still on the form, make clear that the name will cause problems,
// rather than letting it fail in various ways later, when writing config, installing the runner, or creating the container.
//
// It checks for several kinds of conflicts that would really fail or produce unexpected results:
//   - A runner with the same name already exists in the config (it used to be silently given a random suffix, which the user was unaware of)
//   - After name normalization, it collides with an existing runner's container name (github-runner-a.b and a-b map to the same one)
//   - The install directory is the same as an existing runner's (path points to someone else's directory)
//   - A registered runner directory already exists on disk (with a token, config.sh will reject it; without a token, it is takeover)
//   - A container with the same name already exists on the host (a previous deletion was incomplete, and it will be reused directly at startup)
package handler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

// Conflict type. The frontend uses it to localize text; unknown types fall back to message
const (
	ConflictNameTaken       = "name_taken"
	ConflictContainerName   = "container_name"
	ConflictInstallDir      = "install_dir"
	ConflictDirRegistered   = "dir_registered"
	ConflictDirAdopt        = "dir_adopt"
	ConflictDirExists       = "dir_exists"
	ConflictContainerExists = "container_exists"
)

// Conflict severity: error means continuing will fail or overwrite an existing runner; warn means it can continue but the user should be aware
const (
	ConflictLevelError = "error"
	ConflictLevelWarn  = "warn"
)

// RunnerConflict is a single conflict. Its fields align with ProbeInfo from container probing so the frontend can render them uniformly.
type RunnerConflict struct {
	Type       string `json:"type"`
	Level      string `json:"level"`
	Message    string `json:"message"`
	Detail     string `json:"detail,omitempty"`      // The object it conflicts with: an existing runner name, directory, or container name
	Suggestion string `json:"suggestion,omitempty"`  // Plain-language suggestion
	FixCommand string `json:"fix_command,omitempty"` // A directly runnable command, language-independent
}

// PrecheckResponse is the response from GET /api/runner-precheck
type PrecheckResponse struct {
	Name          string           `json:"name"`
	InstallDir    string           `json:"install_dir"`
	ContainerName string           `json:"container_name,omitempty"` // Container mode only
	Available     bool             `json:"available"`                // No error-level conflicts
	SuggestedName string           `json:"suggested_name,omitempty"` // Available name offered when there is a conflict
	Conflicts     []RunnerConflict `json:"conflicts"`
}

// containerLookup checks whether a container exists; it exists so tests can inject it. Returning known=false means the lookup failed (e.g. docker is unavailable), and this check is skipped
type containerLookup func(containerName string) (exists bool, status string, known bool)

// dockerContainerLookup is the real container lookup; it silently skips when docker is unavailable:
// The pre-check is a nice-to-have and must not block the add flow just because docker is unavailable
func dockerContainerLookup(ctx context.Context) containerLookup {
	return func(containerName string) (bool, string, bool) {
		exists, status, err := runner.ContainerState(ctx, containerName)
		if err != nil {
			return false, "", false
		}
		return exists, status, true
	}
}

// hasErrorConflict reports whether any error-level conflict exists
func hasErrorConflict(conflicts []RunnerConflict) bool {
	for _, c := range conflicts {
		if c.Level == ConflictLevelError {
			return true
		}
	}
	return false
}

// collectRunnerConflicts gathers name and path conflicts against the current config and the host.
// The container check is skipped when lookup is nil or when not in container mode.
// willRegister means this run will invoke config.sh with a registration token. When the directory already holds a registered runner,
// passing a token is a hard failure (config.sh rejects duplicate configuration), while omitting it simply takes it over into the config, which is normal usage.
func collectRunnerConflicts(cfg *config.Config, name, path string, lookup containerLookup, willRegister bool) []RunnerConflict {
	conflicts := []RunnerConflict{}
	if cfg == nil || name == "" {
		return conflicts
	}
	item := config.RunnerItem{Name: name, Path: path}
	installDir := item.InstallPath(cfg.Runners.BasePath)
	containerName := config.NormalizedContainerName(name)

	nameTaken := false
	for _, existing := range cfg.Runners.Items {
		if existing.Name == name {
			nameTaken = true
			conflicts = append(conflicts, RunnerConflict{
				Type:       ConflictNameTaken,
				Level:      ConflictLevelError,
				Message:    "a runner named " + name + " already exists in the configuration",
				Detail:     name,
				Suggestion: "pick another name (the suggested one below works), or manage the existing runner from the list",
			})
			continue
		}
		if filepath.Clean(existing.InstallPath(cfg.Runners.BasePath)) == filepath.Clean(installDir) {
			conflicts = append(conflicts, RunnerConflict{
				Type:       ConflictInstallDir,
				Level:      ConflictLevelError,
				Message:    fmt.Sprintf("the install directory is the same as runner %s: %s", existing.Name, installDir),
				Detail:     existing.Name,
				Suggestion: "pick another name, or point path at a different subdirectory",
			})
		}
		if cfg.Runners.ContainerMode && config.NormalizedContainerName(existing.Name) == containerName {
			conflicts = append(conflicts, RunnerConflict{
				Type:       ConflictContainerName,
				Level:      ConflictLevelError,
				Message:    fmt.Sprintf("once normalized, the name collides with the container name of runner %s: %s", existing.Name, containerName),
				Detail:     existing.Name,
				Suggestion: "container names keep only letters, digits and hyphens; pick a name that stays distinct after that",
			})
		}
	}
	// The directory and container of a same-named runner already belong to it; reporting them again is just noise
	if nameTaken {
		return conflicts
	}

	if dirState := inspectInstallDir(installDir); dirState != "" {
		switch dirState {
		case "registered":
			if willRegister {
				conflicts = append(conflicts, RunnerConflict{
					Type:       ConflictDirRegistered,
					Level:      ConflictLevelError,
					Message:    installDir + " already holds a registered runner (it has a .runner file); registering again with a token is rejected by config.sh",
					Detail:     installDir,
					Suggestion: "to adopt the registered runner, add it with the registration token left empty; to register afresh, remove the directory and delete the runner on GitHub first",
					FixCommand: "rm -rf " + installDir,
				})
				break
			}
			conflicts = append(conflicts, RunnerConflict{
				Type:       ConflictDirAdopt,
				Level:      ConflictLevelWarn,
				Message:    installDir + " already holds a registered runner, so adding it adopts that runner instead of registering a new one",
				Detail:     installDir,
				Suggestion: "if you meant to create a new runner, pick another name",
			})
		case "nonempty":
			conflicts = append(conflicts, RunnerConflict{
				Type:       ConflictDirExists,
				Level:      ConflictLevelWarn,
				Message:    installDir + " exists and is not empty, so the runner already extracted there is reused",
				Detail:     installDir,
				Suggestion: "if it is left over from an earlier attempt, pick another name or empty the directory first",
			})
		}
	}

	if cfg.Runners.ContainerMode && lookup != nil {
		if exists, status, known := lookup(containerName); known && exists {
			conflicts = append(conflicts, RunnerConflict{
				Type:  ConflictContainerExists,
				Level: ConflictLevelError,
				Message: fmt.Sprintf("the host already has a container named %s (status %s) that belongs to no runner in the current "+
					"configuration. Starting this runner reuses it, and it is still mounting the directory it was created with, "+
					"which need not be this runner's",
					containerName, status),
				Detail:     containerName,
				Suggestion: "pick another name, or once you are sure the container is unused, delete it so the Manager recreates it against this runner's directory",
				FixCommand: "docker rm -f " + containerName,
			})
		}
	}
	return conflicts
}

// inspectInstallDir determines the state of the install directory: registered (a registered runner), nonempty (not empty), "" (does not exist or is empty)
func inspectInstallDir(installDir string) string {
	fi, err := os.Stat(installDir)
	if err != nil || !fi.IsDir() {
		return ""
	}
	if _, err := os.Stat(filepath.Join(installDir, ".runner")); err == nil {
		return "registered"
	}
	dir, err := os.Open(installDir)
	if err != nil {
		return ""
	}
	defer func() { _ = dir.Close() }()
	// This only checks for non-emptiness; there is no need to read, allocate, or sort the whole directory.
	entries, err := dir.ReadDir(1)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return "nonempty"
}

// suggestRunnerName returns the first candidate name with no error-level conflict: name-2, name-3, ...,
// If none work, it falls back to a random suffix. An empty string means nothing could be found.
func suggestRunnerName(cfg *config.Config, name, path string, lookup containerLookup, willRegister bool) string {
	for i := 2; i <= 20; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		if !hasErrorConflict(collectRunnerConflicts(cfg, candidate, path, lookup, willRegister)) {
			return candidate
		}
	}
	for i := 0; i < 20; i++ {
		// If a random suffix cannot be obtained, skip this round instead of building "name-" as the suggested name --
		// That name is not distinctive, and it might just pass the conflict check and get recommended
		suffix := shortRandomSuffix()
		if suffix == "" {
			continue
		}
		candidate := name + "-" + suffix
		if !hasErrorConflict(collectRunnerConflicts(cfg, candidate, path, lookup, willRegister)) {
			return candidate
		}
	}
	return ""
}

// PrecheckRunner pre-checks the name and path (GET /api/runner-precheck?name=&path=).
// Read-only: it does not change config or touch directories; used by the UI to give live hints on the form.
func PrecheckRunner(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.QueryParam("name"))
	path := strings.TrimSpace(c.QueryParam("path"))
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_required"))
	}
	if !config.IsSafeRunnerNameOrPath(name) || (path != "" && !config.IsSafeRunnerNameOrPath(path)) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_path_invalid"))
	}

	// The container lookup needs a timeout: if docker hangs, the form should not hang with it
	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
	defer cancel()
	var lookup containerLookup
	if cfg.Runners.ContainerMode {
		lookup = dockerContainerLookup(ctx)
	}

	// has_token=1: a registration token was entered in the UI, so this run will really register via config.sh
	willRegister := c.QueryParam("has_token") == "1"
	conflicts := collectRunnerConflicts(cfg, name, path, lookup, willRegister)
	resp := PrecheckResponse{
		Name:       name,
		InstallDir: config.RunnerItem{Name: name, Path: path}.InstallPath(cfg.Runners.BasePath),
		Available:  !hasErrorConflict(conflicts),
		Conflicts:  conflicts,
	}
	if cfg.Runners.ContainerMode {
		resp.ContainerName = config.NormalizedContainerName(name)
	}
	if !resp.Available {
		// When path is filled in it is fixed, and changing the name cannot resolve the directory conflict, so no suggested name is given in this case
		if path == "" {
			resp.SuggestedName = suggestRunnerName(cfg, name, path, lookup, willRegister)
		}
	}
	return c.JSON(http.StatusOK, resp)
}
