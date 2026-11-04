package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/sharkpaw/runner-deck/internal/atomicfile"
	"github.com/sharkpaw/runner-deck/internal/childenv"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
)

// Matches the file name written by the handler, read by cron and the API
const (
	RegistrationResultFile = ".registration_result.json"
	GitHubStatusFile       = ".github_status.json"
)

// Status is the runner directory state
type Status string

const (
	StatusUnknown   Status = "unknown"
	StatusInstalled Status = "installed" // Configured (.runner and related files exist)
	StatusNew       Status = "new"       // Directory exists only, not registered
	StatusMissing   Status = "missing"   // Directory does not exist
)

// RunnerInfo is the runner information shown in the frontend
type RunnerInfo struct {
	Name                  string     `json:"name"`
	Path                  string     `json:"path"`
	TargetType            string     `json:"target_type"`
	Target                string     `json:"target"`
	Labels                []string   `json:"labels"`
	Status                Status     `json:"status"`
	InstallDir            string     `json:"install_dir"`
	Running               bool       `json:"running"`                      // Whether the process is running
	Probe                 *ProbeInfo `json:"probe,omitempty"`              // Structured probe info (error/type/suggestion/check_command/fix_command)
	JobDockerBackend      string     `json:"job_docker_backend"`           // Docker backend inside the job in container mode: dind / host-socket / none
	ContainerDrift        string     `json:"container_drift,omitempty"`    // Differences between container creation parameters and the current config; non-empty means the container was built from an old config
	RegistrationMessage   string     `json:"registration_message"`         // Result of the latest registration (success or failure reason)
	RegistrationCheckedAt string     `json:"registration_checked_at"`      // Time of the registration result
	RegisteredOnGitHub    *bool      `json:"registered_on_github"`         // Cron checks via the GitHub API whether it shows on GitHub; nil means not checked or no answer found
	GitHubCheckAt         string     `json:"github_check_at"`              // Time of the latest GitHub check
	GitHubCheckError      string     `json:"github_check_error,omitempty"` // Reason no answer was found (token expired, rate limited, network unreachable, etc.)
	GitHubBusy            *bool      `json:"github_busy,omitempty"`        // Whether this runner is running a job on the GitHub side; nil means unknown (not checked, not found, or no PAT configured)
	GitHubPublic          *bool      `json:"github_public,omitempty"`      // Whether the target repository is public; nil means unknown (organization target, not found, or not checked yet)
	GitHubURL             string     `json:"github_url,omitempty"`         // The Actions Runners settings page for this target on GitHub; empty when the target is invalid
}

// GitHubYes / GitHubNo / GitHubUnknown let templates check the three states.
//
// Templates cannot use {{if .RegisteredOnGitHub}} directly: html/template only checks whether a pointer is nil,
// a pointer to false is also truthy, so "not on GitHub" would render as "GitHub ✓".
func (r RunnerInfo) GitHubYes() bool { return r.RegisteredOnGitHub != nil && *r.RegisteredOnGitHub }

// GitHubNo means an answer was found and it is "not present"
func (r RunnerInfo) GitHubNo() bool { return r.RegisteredOnGitHub != nil && !*r.RegisteredOnGitHub }

// GitHubUnknown means this check could not reach an answer (distinguished from "never checked" by GitHubCheckAt)
func (r RunnerInfo) GitHubUnknown() bool { return r.RegisteredOnGitHub == nil }

// GitHubBusyYes means that at the last query GitHub reported it was running a job.
// Same as the three above: GitHubBusy is *bool, so {{if .GitHubBusy}} in a template would treat
// a pointer to false as true, so "idle" would show as "busy".
func (r RunnerInfo) GitHubBusyYes() bool { return r.GitHubBusy != nil && *r.GitHubBusy }

// GitHubPublicYes means that at the last query GitHub reported this target repository as public.
// Likewise, {{if .GitHubPublic}} cannot be used in templates: a pointer to false is also truthy,
// so every private repository would get a "public repo" badge, which is exactly the one that must not be misreported.
func (r RunnerInfo) GitHubPublicYes() bool { return r.GitHubPublic != nil && *r.GitHubPublic }

// ProbeInfo is the structured info for a failed container probe.
type ProbeInfo struct {
	Error        string `json:"error"`
	Type         string `json:"type"`
	Suggestion   string `json:"suggestion"`
	CheckCommand string `json:"check_command"`
	FixCommand   string `json:"fix_command"`
}

// GetByName gets a single runner's info by name, returns nil if it does not exist; returns nil safely when cfg is nil
func GetByName(cfg *config.Config, name string) *RunnerInfo {
	if cfg == nil {
		return nil
	}
	for _, item := range cfg.Runners.Items {
		if item.Name != name {
			continue
		}
		installDir := item.InstallPath(cfg.Runners.BasePath)
		info := &RunnerInfo{
			Name:       item.Name,
			Path:       item.Path,
			TargetType: item.TargetType,
			Target:     item.Target,
			Labels:     append([]string(nil), item.Labels...),
			InstallDir: installDir,
		}
		if cfg.Runners.ContainerMode {
			info.JobDockerBackend = cfg.Runners.JobDockerBackend
		}
		if item.Path == "" {
			info.Path = item.Name
		}
		info.Status, info.Running = getStatus(installDir)
		info.RegistrationMessage, info.RegistrationCheckedAt = readRegistrationResult(installDir)
		info.applyGitHubStatus(installDir)
		info.GitHubURL = GitHubSettingsURL(item.TargetType, item.Target)
		return info
	}
	return nil
}

// List lists all runners based on the config and disk state.
//
// Note: in container mode, each item's Running is unreliable, because the runner processes are in their own containers,
// so this process scanning /proc cannot see them. Callers that need the real running state should use ListWithLiveStatus.
func List(cfg *config.Config) []RunnerInfo {
	if cfg == nil {
		return nil
	}
	base := cfg.Runners.BasePath
	list := make([]RunnerInfo, 0, len(cfg.Runners.Items))
	dirs := make([]string, 0, len(cfg.Runners.Items))
	for _, item := range cfg.Runners.Items {
		installDir := item.InstallPath(base)
		info := RunnerInfo{
			Name:       item.Name,
			Path:       item.Path,
			TargetType: item.TargetType,
			Target:     item.Target,
			Labels:     append([]string(nil), item.Labels...),
			InstallDir: installDir,
		}
		if cfg.Runners.ContainerMode {
			info.JobDockerBackend = cfg.Runners.JobDockerBackend
		}
		if item.Path == "" {
			info.Path = item.Name
		}
		info.Status = diskStatus(installDir)
		info.RegistrationMessage, info.RegistrationCheckedAt = readRegistrationResult(installDir)
		info.applyGitHubStatus(installDir)
		info.GitHubURL = GitHubSettingsURL(item.TargetType, item.Target)
		list = append(list, info)
		dirs = append(dirs, installDir)
	}
	// Claim all install directories in one scan: judging each one separately would read the whole process table len(list) times
	procs := runnerproc.FindMany(dirs)
	for i := range list {
		list[i].Running = list[i].Status == StatusInstalled && len(procs[list[i].InstallDir]) > 0
	}
	return list
}

// ListWithLiveStatus fills in the real running state on top of List: in container mode it asks the Agent inside each container one by one,
// In default mode, List already reflects the real state of local processes, so it returns directly.
//
// The two background loops that start "registered but not running" runners must use this, not List: in container mode
// List's Running is always false (Manager and Runner are not in the same PID namespace),
// so every inspection round would start every runner again.
//
// On probe failure, set the status to StatusUnknown instead of keeping installed: starting requires
// knowing it is not running. Docker being unreachable doesn't prove that, so doing nothing is safer than restarting repeatedly.
func ListWithLiveStatus(ctx context.Context, cfg *config.Config) []RunnerInfo {
	if cfg == nil {
		return nil
	}
	list := List(cfg)
	if !cfg.Runners.ContainerMode {
		return list
	}
	for i := range list {
		running, status, _, err := ContainerRunnerStatus(ctx, cfg, list[i].Name, list[i].InstallDir)
		if err != nil {
			list[i].Status = StatusUnknown
			list[i].Running = false
			continue
		}
		list[i].Running = running
		list[i].Status = status
	}
	return list
}

// diskStatus looks only at the directory: missing if it doesn't exist, installed if it has .runner, otherwise new.
// Kept separate from process probing so List can classify a whole batch of runners with one /proc scan.
func diskStatus(installDir string) Status {
	if installDir == "" {
		return StatusMissing
	}
	fi, err := os.Stat(installDir)
	if err != nil || !fi.IsDir() {
		return StatusMissing
	}
	// A registered runner has a .runner file
	if _, err := os.Stat(filepath.Join(installDir, ".runner")); err == nil {
		return StatusInstalled
	}
	return StatusNew
}

func getStatus(installDir string) (Status, bool) {
	status := diskStatus(installDir)
	if status != StatusInstalled {
		return status, false
	}
	return status, isProcessRunning(installDir)
}

// readRegistrationResult reads the registration result written by the handler, returning message and at
func readRegistrationResult(installDir string) (message, at string) {
	b, err := os.ReadFile(filepath.Join(installDir, RegistrationResultFile))
	if err != nil {
		return "", ""
	}
	var v struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		At      string `json:"at"`
	}
	if json.Unmarshal(b, &v) != nil {
		return "", ""
	}
	return v.Message, v.At
}

// GitHubStatus is the content of .github_status.json: the conclusions left by the last GitHub query.
//
// The three *bool fields are tri-state: nil always means unknown, not false. Writing an unqueried result as false
// would make the UI vouch for something that was never checked, which is exactly what the other comments in this file keep warning about.
type GitHubStatus struct {
	// Registered whether the Runner is registered on GitHub
	Registered *bool `json:"registered"`
	// Busy whether GitHub says it is running a Job
	Busy *bool `json:"busy,omitempty"`
	// Public whether the target repository is public. Organization targets, lookup failures, and never-checked cases are all nil.
	Public *bool `json:"public,omitempty"`
	// LastCheck the time (RFC3339) of the most recent "is registered" query.
	//
	// Set by the caller, not stamped with the current time on write: the UI uses emptiness to tell "never checked"
	// apart from "checked but failed". Without a PAT this query never happens,
	// but the visibility half still writes the file (it works anonymously). Stamping the time would mean
	// claiming "checked, failed" for a runner that was never checked.
	LastCheck string `json:"last_check,omitempty"`
	// Error the reason the query could not produce an answer (expired token, rate limiting, network unreachable, etc.)
	Error string `json:"error,omitempty"`
	// VisibilityCheckedAt the time (RFC3339) of the most recent visibility lookup; empty means never asked.
	//
	// Visibility rarely changes, while anonymous requests are limited to 60 per hour; this keeps the lookup to once per repository per day.
	VisibilityCheckedAt string `json:"visibility_checked_at,omitempty"`
}

// ReadGitHubStatus reads the conclusions of the last GitHub query.
//
// A missing or unparseable file is read as the zero value, meaning "never checked", with every field empty.
// In files written by older versions, registered was a plain bool, which deserializes to a non-nil pointer; the meaning is unchanged.
// Old files without public or visibility_checked_at read back as nil and an empty string, meaning "visibility not yet checked".
func ReadGitHubStatus(installDir string) GitHubStatus {
	var v GitHubStatus
	b, err := os.ReadFile(filepath.Join(installDir, GitHubStatusFile))
	if err != nil {
		return GitHubStatus{}
	}
	if json.Unmarshal(b, &v) != nil {
		return GitHubStatus{}
	}
	return v
}

// WriteGitHubStatus records the conclusions of a GitHub query.
//
// The whole file is written at once, so the caller must carry over the fields it did not re-query:
// the usual approach is to call ReadGitHubStatus first to get the previous conclusions, then change only some fields,
// otherwise the registration check every five minutes would wipe the visibility conclusion that is only queried once a day.
func WriteGitHubStatus(installDir string, st GitHubStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	// Atomic replace: list rendering may read this file at any time, and an in-place truncate-and-rewrite could expose half-written JSON,
	// which would be displayed as "this Runner has no GitHub status".
	return atomicfile.WriteFile(filepath.Join(installDir, GitHubStatusFile), b, 0644)
}

// applyGitHubStatus fills info with the conclusions stored on disk.
func (r *RunnerInfo) applyGitHubStatus(installDir string) {
	gh := ReadGitHubStatus(installDir)
	r.RegisteredOnGitHub, r.GitHubBusy, r.GitHubPublic = gh.Registered, gh.Busy, gh.Public
	r.GitHubCheckAt, r.GitHubCheckError = gh.LastCheck, gh.Error
}

// isProcessRunning checks whether a Runner process belonging to this install directory is alive on this machine.
//
// Only meaningful in the default mode, where the Runner process shares a PID namespace with the Manager.
// In container mode each Runner runs in its own container, so the Manager's scan of its own /proc will never find it;
// that check goes through ContainerRunnerStatus, which asks the Agent inside the container. See ListWithLiveStatus.
func isProcessRunning(installDir string) bool {
	return runnerproc.Running(installDir)
}

// EnsureRunnerDir ensures the runner directory exists and returns its path; it must be under base_path (prevents path traversal)
func EnsureRunnerDir(cfg *config.Config, name, subPath string) (string, error) {
	dir := subPath
	if dir == "" {
		dir = name
	}
	dir = filepath.Clean(dir)
	if strings.Contains(dir, "..") || filepath.IsAbs(dir) {
		dir = name
	}
	baseAbs, err := filepath.Abs(cfg.Runners.BasePath)
	if err != nil {
		baseAbs = cfg.Runners.BasePath
	}
	abs := filepath.Join(baseAbs, dir)
	abs, err = filepath.Abs(abs)
	if err != nil {
		abs = filepath.Join(baseAbs, dir)
	}
	// Ensure the install directory is under base_path
	rel, err := filepath.Rel(baseAbs, abs)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", os.ErrInvalid
	}
	// base_path itself is created with 0755 as before; only the Runner's own install directory is tightened to 0700.
	//
	// config.sh writes .credentials_rsaparams into this directory: the RSA private key the Runner uses to identify itself to GitHub.
	// actions/runner does not set permissions on these files (ConfigurationStore only does so on Windows
	// the Hidden attribute. On Unix it follows umask entirely (usually 0644), so if others can enter the directory,
	// any local user on the host can read it, and then impersonate this Runner to claim Jobs and see the secrets passed to Jobs.
	//
	// MkdirAll does not change permissions of existing directories: in older deployments they are still 0755, and the startup self-check names them and gives the
	// chmod command instead of changing them here for the user; tightening permissions in a deployment with a mismatched UID would break things that currently work.
	if err := os.MkdirAll(baseAbs, 0755); err != nil {
		return "", err
	}
	return abs, os.MkdirAll(abs, RunnerDirMode)
}

// RunnerDirMode is the permission for the Runner install directory: only the owner can enter it.
// The directory holds GitHub credentials; see the EnsureRunnerDir notes.
const RunnerDirMode = 0o700

// ConfigScriptName returns the configuration script name for the current system
func ConfigScriptName() string {
	if runtime.GOOS == "windows" {
		return "config.cmd"
	}
	return "config.sh"
}

// RunScriptName returns the run script name for the current system
func RunScriptName() string {
	if runtime.GOOS == "windows" {
		return "run.cmd"
	}
	return "run.sh"
}

var execCommand = exec.Command

// Start launches the runner in the background under installDir (runs run.sh/run.cmd)
// Convert installDir to an absolute path, so a relative path is not resolved against the process CWD at exec time, which would fail to find run.sh
func Start(installDir string) error {
	absDir, err := filepath.Abs(installDir)
	if err != nil {
		return fmt.Errorf("cannot resolve the runner path: %w", err)
	}
	installDir = absDir
	script := filepath.Join(installDir, RunScriptName())
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("run script %s not found: %w", script, err)
	}
	cmd := execCommand(script)
	cmd.Dir = installDir
	// run.sh runs users' Jobs: do not pass the Manager's own credentials down to it, see internal/childenv
	cmd.Env = childenv.Environ()
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// The child process must be waited on, otherwise an exited run.sh becomes a zombie process when running inside a container (where the main process is PID 1).
	// Wait in a background goroutine, so Start returns without blocking.
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

// StartIfInstalled starts the runner if it is registered: in container mode it calls StartRunnerContainer, otherwise it calls Start. Used by main and the handler to share the same 'start if registered but not running' logic
func StartIfInstalled(ctx context.Context, cfg *config.Config, name, installDir string) error {
	if cfg == nil {
		return fmt.Errorf("the configuration is empty")
	}
	if cfg.Runners.ContainerMode {
		return StartRunnerContainer(ctx, cfg, name, installDir)
	}
	return Start(installDir)
}

// Stop sends SIGTERM to the Runner processes under this install directory.
//
// runnerproc.Find orders the supervising script ahead of the listener, so here signals are sent in order: first let run.sh exit,
// so that its while loop does not respawn a new listener after the first one is killed.
// Return an error when none are found; the caller (the 'Stop' button in the UI) needs to know it did not stop anything.
func Stop(installDir string) error {
	absDir, err := filepath.Abs(installDir)
	if err != nil {
		return fmt.Errorf("cannot resolve the runner path: %w", err)
	}
	installDir = absDir
	pids := runnerproc.Find(installDir)
	if len(pids) == 0 {
		return fmt.Errorf("no running runner process found under %s", installDir)
	}
	var firstErr error
	for _, pid := range pids {
		process, err := os.FindProcess(pid)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// A process may exit on its own between the scan and the signal, so ESRCH does not count as a failure
		if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
