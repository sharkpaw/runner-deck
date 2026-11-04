// Package handler implements the Manager's HTTP API and WebUI logic.
// Manager responsibilities: config read/write, runner registration orchestration (install/register scripts), container start/stop scheduling, and status aggregation; it does not host runner processes directly.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/atomicfile"
	"github.com/sharkpaw/runner-deck/internal/childenv"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/githubcheck"
	"github.com/sharkpaw/runner-deck/internal/runner"
	"github.com/sharkpaw/runner-deck/internal/runnerproc"
	"github.com/sharkpaw/runner-deck/internal/secrets"
	secure "github.com/soulteary/secure-kit/v2"
)

// Supported UI languages, same as docs (en, zh, fr, ja, ko, de).
var supportedLangs = []string{"en", "tr", "zh", "fr", "ja", "ko", "de"}

// I18nLoader loads translations for a language code (e.g. "en", "zh"). Set by main from embed.
var I18nLoader func(lang string) (map[string]string, error)

// installRunnerScriptPath is the script path used to auto-install the runner inside the container (present in the Docker image).
// It is a var rather than a const only so tests can point it at a fake script; the "should we install first"
// branch in the registration flow can only be reached by actually downloading actions/runner without changing this.
var installRunnerScriptPath = "/app/scripts/install-runner.sh"

// ConfigPath is the config file path, injected by main
var ConfigPath string

// The location of the Secrets Manager-side credential (optional PAT), injected by main, same as ConfigPath.
var Secrets *secrets.Store

// secretStore returns the credential storage location; when main does not inject one, it is derived from ConfigPath.
//
// This derivation exists for tests: they only set ConfigPath and need not remember to set another package-level variable,
// since the credential directory is already determined by the config file path, so the two can never point at different places.
func secretStore() *secrets.Store {
	if Secrets != nil {
		return Secrets
	}
	return secrets.NewStore(ConfigPath)
}

// lifecycleContext derives the context for write operations like "start/stop runner": it keeps only the timeout and drops the request's cancellation signal.
//
// It must not be attached directly to c.Request().Context(). A browser refresh or navigation cancels in-flight requests, and container operations
// ultimately run in the docker child process started by exec.CommandContext, and cancelling the context means SIGKILL:
// docker create gets killed midway, and the log only shows "docker create failed. Output: (no output): signal: killed",
// while the daemon may already have created the container, so the next start hits a name conflict.
// Once started, a start/stop should run to completion, regardless of whether the HTTP connection that initiated it is still alive --
// RemoveRunnerByName and the background registration worker already work this way, so this aligns with them.
// Values in the request context (such as trace) are still kept; only cancellation is no longer propagated.
func lifecycleContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), d)
}

// registrationJob is the task that installs and registers a runner in the background
type registrationJob struct {
	BasePath   string
	InstallDir string
	RunnerName string
	URL        string
	Token      string
	Labels     []string
}

// registrationQueue is the background task queue, run sequentially by a single worker to avoid multiple tasks exhausting resources at once while keeping the API non-blocking
var registrationQueue = make(chan registrationJob, 128)

// StartRegistrationWorker starts the background worker; it should be called once in main
func StartRegistrationWorker() {
	startRegistrationWorker(registrationQueue)
}

// startRegistrationWorker reports completion when its queue closes, so callers
// can wait for the final job before releasing resources shared with the worker.
func startRegistrationWorker(queue <-chan registrationJob) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for j := range queue {
			runRegistrationJob(j)
		}
	}()
	return done
}

// runRegistrationJob performs a single install, register, and start (called in a background goroutine)
func runRegistrationJob(j registrationJob) {
	installDir := j.InstallDir
	configScript := filepath.Join(installDir, runner.ConfigScriptName())
	if _, err := os.Stat(configScript); err != nil {
		runnerSegment := filepath.Base(installDir)
		installOut, installErr := runInstallRunnerScript(j.BasePath, runnerSegment, 3*time.Minute)
		if installErr != nil {
			msg := "installing the runner automatically failed: " + installErr.Error()
			writeRegistrationResult(installDir, false, msg)
			log.Printf("[registration] %s install failed: %v\noutput: %s", j.RunnerName, installErr, string(installOut))
			return
		}
		if _, err2 := os.Stat(configScript); err2 != nil {
			writeRegistrationResult(installDir, false, "the install finished but "+runner.ConfigScriptName()+" was not found")
			log.Printf("[registration] %s config script missing after install\noutput: %s", j.RunnerName, string(installOut))
			return
		}
	}
	out, err := runConfigScript(installDir, j.URL, j.Token, j.RunnerName, j.Labels, 2*time.Minute)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(string(out), "Must not run with sudo") {
			msg += " (run the container as a non-root user, or set RUNNER_ALLOW_RUNASROOT=1)"
		}
		outLower := strings.ToLower(string(out))
		if strings.Contains(outLower, "token") &&
			(strings.Contains(outLower, "invalid") || strings.Contains(outLower, "expired") ||
				strings.Contains(outLower, "already") || strings.Contains(outLower, "used")) {
			msg += ". Generate a fresh registration token on GitHub for each runner."
		}
		// Under --unattended, a duplicate name exits with an immediate failure, and the error only says "a runner with the same name exists",
		// but it does not say where to delete it: the GitHub-side runner list is outside this tool's scope
		if strings.Contains(outLower, "runner exists with the same name") ||
			strings.Contains(outLower, "a runner exists with the same name") {
			msg += ". GitHub already lists a runner with this name: delete it under Settings → Actions → Runners on the target repository or organization, then try again."
		}
		writeRegistrationResult(installDir, false, msg)
		log.Printf("[registration] %s registration failed: %s", j.RunnerName, msg)
		return
	}
	writeRegistrationResult(installDir, true, "registered")
	cfg, _ := config.Load(ConfigPath)
	if cfg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if startErr := runner.StartIfInstalled(ctx, cfg, j.RunnerName, installDir); startErr != nil {
			log.Printf("[registration] %s registered but failed to start: %v", j.RunnerName, startErr)
		} else {
			log.Printf("[registration] %s registered and started", j.RunnerName)
		}
	}
}

func getConfig(c echo.Context) (*config.Config, error) {
	cfg, err := config.Load(ConfigPath)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.config_load_failed", err))
	}
	return cfg, nil
}

func applyProbeFailure(info *runner.RunnerInfo, statusErr error) {
	info.Running = false
	info.Status = runner.StatusUnknown
	pt := runner.DetectProbeErrorType(statusErr)
	info.Probe = &runner.ProbeInfo{
		Error:        statusErr.Error(),
		Type:         string(pt),
		Suggestion:   runner.ProbeSuggestion(pt),
		CheckCommand: runner.ProbeCheckCommand(pt),
		FixCommand:   runner.ProbeFixCommand(pt),
	}
}

func clearProbe(info *runner.RunnerInfo) {
	info.Probe = nil
}

// applyContainerStatus overwrites each item's Running/Status in list with the Agent status in container mode, modifying in place
func applyContainerStatus(ctx context.Context, cfg *config.Config, list []runner.RunnerInfo) {
	for i := range list {
		applyContainerStatusOne(ctx, cfg, &list[i])
	}
}

// applyContainerStatusOne overwrites the Running/Status/Probe of a single info with the Agent status in container mode
func applyContainerStatusOne(ctx context.Context, cfg *config.Config, info *runner.RunnerInfo) {
	running, status, drift, statusErr := runner.ContainerRunnerStatus(ctx, cfg, info.Name, info.InstallDir)
	// Drift information is independent of the probe result: it is still included when the probe fails, so the UI can warn that "this container was built from an old config"
	info.ContainerDrift = drift
	if statusErr != nil {
		log.Printf("[container-status] name=%s: %v", info.Name, statusErr)
		applyProbeFailure(info, statusErr)
		return
	}
	clearProbe(info)
	info.Running = running
	info.Status = status
}

// shortRandomSuffixLen is the length of the suggested name suffix; the charset is fixed to lowercase letters plus digits,
// because it gets concatenated into container and directory names.
const shortRandomSuffixLen = 6

// shortRandomSuffix generates a 6-character random suffix of lowercase letters and digits, used to deduplicate runner names.
//
// The earlier implementation was letters[int(b[i])%len(letters)]: the 36-character set does not divide 256 evenly,
// so a/b/c/d are about 14% more likely than the other characters. The suffix is only for deduplication, not a security boundary,
// but there is no reason to keep a biased implementation -- secure.RandomString uses crypto/rand.Int
// with modulo by the charset length, which is naturally unbiased.
//
// It returns an empty string when no random number is available, and the caller skips this round: it must never fall back to a predictable value,
// since that would let two concurrent adds get the same suggested name, creating exactly the conflict the suggestion is meant to avoid.
func shortRandomSuffix() string {
	s, err := secure.RandomString(shortRandomSuffixLen, secure.CharsetAlphanumericLower)
	if err != nil {
		log.Printf("[precheck] cannot generate the suffix for the suggested name: %v", err)
		return ""
	}
	return s
}

// runInstallRunnerScript runs install-runner.sh inside the container, downloading and extracting the runner to basePath/runnerName; returns an error on timeout
func runInstallRunnerScript(basePath, runnerName string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, installRunnerScriptPath, runnerName)
	cmd.Env = append(childenv.Environ(), "RUNNERS_BASE_PATH="+basePath)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, context.DeadlineExceeded
	}
	return out, err
}

// runConfigScript runs the config script under installDir to register with GitHub, with a 2-minute timeout; returns the output and error
// Converts installDir to an absolute path, to avoid relative paths being resolved against the process CWD at exec time, which would fail to find the config script
//
// --name must be passed: without it, config.sh uses the local hostname as the runner name, and that hostname is inside the Manager container
// the one that ran, so every Runner in a deployment registered under the same name (the Manager container's hostname).
// As a result, GitHub only showed one Runner, and its name was a container ID that did not match the name shown in the UI.
//
// --unattended must be passed: on a name collision, --unattended makes it fail and exit immediately. Without it, it enters an interactive retry loop
// ("Failed to replace the runner. Try again or ctrl-c to quit"). There is no TTY here,
// so that loop runs until the timeout. Registration runs sequentially on a single worker, so every queued Runner behind it is blocked,
// which shows up as "three were added, only the first registered, and the other two have not logged a single line".
func runConfigScript(installDir, url, token, name string, labels []string, timeout time.Duration) ([]byte, error) {
	absDir, err := filepath.Abs(installDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve the runner path: %w", err)
	}
	installDir = absDir
	configScript := filepath.Join(installDir, runner.ConfigScriptName())
	args := []string{"--url", url, "--token", token, "--unattended"}
	if name != "" {
		args = append(args, "--name", name)
	}
	if len(labels) > 0 {
		args = append(args, "--labels", strings.Join(labels, ","))
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, configScript, args...)
	cmd.Dir = installDir
	cmd.Env = childenv.Environ()
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, context.DeadlineExceeded
	}
	return out, err
}

// writeRegistrationResult writes the result of this registration to the runner directory.
func writeRegistrationResult(installDir string, success bool, message string) {
	p := filepath.Join(installDir, runner.RegistrationResultFile)
	body := struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		At      string `json:"at"`
	}{Success: success, Message: message, At: time.Now().Format(time.RFC3339)}
	b, _ := json.Marshal(body)
	// Replaces atomically, for the same reason as runner.WriteGitHubStatus. Errors are still ignored:
	// failing to write the registration result should not make a successful registration report an error. At worst, the UI is missing one result line.
	_ = atomicfile.WriteFile(p, b, 0644)
}

// ListRunners lists all runners. In container mode, the in-container Agent status overrides Running/Status.
func ListRunners(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	list := runner.List(cfg)
	if cfg.Runners.ContainerMode {
		applyContainerStatus(c.Request().Context(), cfg, list)
	}
	return c.JSON(http.StatusOK, map[string]any{"runners": list})
}

// viewData assembles the data the template needs. The index page and the runnerRows fragment render the same set of fields,
// and copying it in both places would eventually miss one of them (such as the container-mode status override), so it is kept in one place.
func viewData(c echo.Context) (map[string]any, error) {
	cfg, err := getConfig(c)
	if err != nil {
		return nil, err
	}
	list := runner.List(cfg)
	if cfg.Runners.ContainerMode {
		applyContainerStatus(c.Request().Context(), cfg, list)
	}
	lang := resolveLang(c)
	var T map[string]string
	if I18nLoader != nil {
		T, _ = I18nLoader(lang)
	}
	if T == nil && I18nLoader != nil {
		T, _ = I18nLoader("en")
	}
	if T == nil {
		T = make(map[string]string)
	}
	tjson, _ := json.Marshal(T)
	return map[string]any{
		"Runners":      list,
		"Config":       cfg,
		"T":            T,
		"Lang":         lang,
		"TJSON":        template.JS(tjson),
		"AssetVersion": AssetVersion,
		"AuthEnabled":  os.Getenv("BASIC_AUTH_PASSWORD") != "",
	}, nil
}

// PageData is what a page without runner data needs: language, translations and asset version.
func PageData(c echo.Context) map[string]any {
	lang := resolveLang(c)
	var T map[string]string
	if I18nLoader != nil {
		T, _ = I18nLoader(lang)
		if T == nil {
			T, _ = I18nLoader("en")
		}
	}
	if T == nil {
		T = make(map[string]string)
	}
	return map[string]any{"T": T, "Lang": lang, "AssetVersion": AssetVersion}
}

// Index serves the Manager home page. In container mode, the in-container Agent status overrides it.
func Index(c echo.Context) error {
	data, err := viewData(c)
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "index.html", data)
}

// RunnerRows renders only the list's <tbody> fragment, so the UI can replace it wholesale on periodic refresh.
// Using a server-rendered fragment instead of having JS build rows from the /api/runners JSON is because several of the row fields
// use three-state checks (GitHubYes/GitHubNo, GitHubBusyYes) where a false pointer is still non-nil.
// Rewriting that logic in JS is exactly where it is easiest to show "idle" as "busy".
func RunnerRows(c echo.Context) error {
	data, err := viewData(c)
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "runnerRows", data)
}

// AddRunnerRequest is the request for adding a runner.
//
// It deliberately has only json tags, not form tags. A cross-site form submission is a "simple request" in CORS terms,
// and can carry cached Basic Auth credentials without a preflight. Without form tags, such a request, even if it got past csrfGuardMiddleware,
// would only bind to an empty struct and then be rejected by required-field validation.
// The UI already submits JSON (FormData is only used to read form fields in place), so this does not affect it.
type AddRunnerRequest struct {
	Name              string   `json:"name"`
	Path              string   `json:"path"`
	TargetType        string   `json:"target_type"`
	Target            string   `json:"target"`
	Labels            []string `json:"labels"`
	RegistrationToken string   `json:"registration_token"`
	// AutoRename true keeps the old behavior: on a name conflict, rename automatically. The default is now to return 409 with a suggested name,
	// so that entering foo in the UI does not silently create foo-ab12cd and leave the user thinking they are operating on foo.
	AutoRename bool `json:"auto_rename"`
}

// AddRunner adds a new runner and optionally registers it.
func AddRunner(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	var req AddRunnerRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.bad_request", err))
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Path = strings.TrimSpace(req.Path)
	req.TargetType = strings.TrimSpace(req.TargetType)
	req.Target = strings.TrimSpace(req.Target)
	req.Labels = normalizeLabels(req.Labels)
	if req.Name == "" || req.TargetType == "" || req.Target == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_target_required"))
	}
	targetTypeNorm := strings.ToLower(strings.TrimSpace(req.TargetType))
	if err := config.ValidateTarget(targetTypeNorm, req.Target); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if !config.IsSafeRunnerNameOrPath(req.Name) || (req.Path != "" && !config.IsSafeRunnerNameOrPath(req.Path)) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_path_invalid"))
	}
	targetNorm := req.Target
	// Conflict precheck: same-named Runner, container name collision, install directory already in use, leftover registered directory on disk, and same-named container on the host.
	// It uses the same logic as /api/runner-precheck, so the UI hint and the result here always agree.
	name := req.Name
	lookupCtx, lookupCancel := lifecycleContext(c.Request().Context(), 5*time.Second)
	defer lookupCancel()
	var lookup containerLookup
	if cfg.Runners.ContainerMode {
		lookup = dockerContainerLookup(lookupCtx)
	}
	willRegister := req.RegistrationToken != ""
	conflicts := collectRunnerConflicts(cfg, name, req.Path, lookup, willRegister)
	if hasErrorConflict(conflicts) {
		if !req.AutoRename {
			suggested := ""
			if req.Path == "" {
				suggested = suggestRunnerName(cfg, name, req.Path, lookup, willRegister)
			}
			resp := map[string]any{
				"message":        conflicts[0].Message,
				"name":           name,
				"conflicts":      conflicts,
				"suggested_name": suggested,
				"install_dir":    config.RunnerItem{Name: name, Path: req.Path}.InstallPath(cfg.Runners.BasePath),
			}
			if cfg.Runners.ContainerMode {
				resp["container_name"] = config.NormalizedContainerName(name)
			}
			return c.JSON(http.StatusConflict, resp)
		}
		// auto_rename: keep the old behavior and automatically find an available name.
		if req.Path != "" {
			return echo.NewHTTPError(http.StatusConflict, trf(c, "api.rename_with_path", conflicts[0].Message))
		}
		name = suggestRunnerName(cfg, name, req.Path, lookup, willRegister)
		if name == "" {
			return echo.NewHTTPError(http.StatusConflict, tr(c, "api.name_taken_no_suggestion"))
		}
	}
	item := config.RunnerItem{
		Name:       name,
		Path:       req.Path,
		TargetType: targetTypeNorm,
		Target:     targetNorm,
		Labels:     req.Labels,
	}
	installDir, err := runner.EnsureRunnerDir(cfg, item.Name, item.Path)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.mkdir_failed", err))
	}
	if err := config.LoadAndSave(ConfigPath, func(cur *config.Config) error {
		for _, i := range cur.Runners.Items {
			if i.Name == item.Name {
				return echo.NewHTTPError(http.StatusConflict, trf(c, "api.name_taken", item.Name))
			}
		}
		cur.Runners.Items = append(cur.Runners.Items, item)
		return nil
	}); err != nil {
		if he, ok := err.(*echo.HTTPError); ok {
			return he
		}
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.config_save_failed", err))
	}
	if req.RegistrationToken != "" {
		configScript := filepath.Join(installDir, runner.ConfigScriptName())
		if _, err := os.Stat(configScript); err != nil {
			// If the directory is empty, install is needed first: if an auto-install script exists, hand it to the background worker so the request is not blocked.
			if _, scriptErr := os.Stat(installRunnerScriptPath); scriptErr == nil {
				url := "https://github.com/" + targetNorm
				select {
				case registrationQueue <- registrationJob{
					BasePath:   cfg.Runners.BasePath,
					InstallDir: installDir,
					RunnerName: item.Name,
					URL:        url,
					Token:      req.RegistrationToken,
					Labels:     item.Labels,
				}:
					return c.JSON(http.StatusOK, map[string]any{
						"message":     tr(c, "api.added_installing"),
						"name":        item.Name,
						"install_dir": installDir,
						"queued":      true,
					})
				default:
					return c.JSON(http.StatusServiceUnavailable, map[string]any{
						"message": tr(c, "api.queue_full"),
						"name":    item.Name,
					})
				}
			}
			return c.JSON(http.StatusOK, map[string]any{
				"message":     trf(c, "api.added_extract_manually", installDir, runner.ConfigScriptName()),
				"name":        item.Name,
				"install_dir": installDir,
			})
		}
		// An existing config script (non-empty directory) only needs registration, which is also run in the background to avoid a long block.
		url := "https://github.com/" + targetNorm
		select {
		case registrationQueue <- registrationJob{
			BasePath:   cfg.Runners.BasePath,
			InstallDir: installDir,
			RunnerName: item.Name,
			URL:        url,
			Token:      req.RegistrationToken,
			Labels:     item.Labels,
		}:
			return c.JSON(http.StatusOK, map[string]any{
				"message":     tr(c, "api.added_registering"),
				"name":        item.Name,
				"install_dir": installDir,
				"queued":      true,
			})
		default:
			return c.JSON(http.StatusServiceUnavailable, map[string]any{
				"message": tr(c, "api.queue_full"),
				"name":    item.Name,
			})
		}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message":     tr(c, "api.added_needs_runner"),
		"name":        item.Name,
		"install_dir": installDir,
	})
}

// GetRunner returns a single runner's config and status (GET /api/runners/:name). In container mode, the Agent status overrides it.
func GetRunner(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	name := c.Param("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_required"))
	}
	if !config.IsSafeRunnerNameOrPath(name) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_invalid"))
	}
	info := runner.GetByName(cfg, name)
	if info == nil {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	if cfg.Runners.ContainerMode {
		applyContainerStatusOne(c.Request().Context(), cfg, info)
	}
	return c.JSON(http.StatusOK, info)
}

// StartRunner starts the specified runner (POST /api/runners/:name/start). In container mode, it starts the Runner container and calls the Agent /start.
func StartRunner(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	name := c.Param("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_required"))
	}
	if !config.IsSafeRunnerNameOrPath(name) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_invalid"))
	}
	info := runner.GetByName(cfg, name)
	if info == nil {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	originalStatus := info.Status
	probeFailed := false
	if cfg.Runners.ContainerMode {
		applyContainerStatusOne(c.Request().Context(), cfg, info)
		if info.Probe != nil {
			probeFailed = true
			log.Printf("[start] status probe failed for container runner name=%s, starting anyway: %v", info.Name, info.Probe.Error)
		}
	}
	// In container mode, if probing fails, the status is marked unknown; the eligibility check before start should fall back to the raw on-disk state.
	startStatus := info.Status
	if probeFailed {
		startStatus = originalStatus
	}
	if startStatus != runner.StatusInstalled {
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.start_requires_registered", string(startStatus)))
	}
	if info.Running {
		return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.already_running")})
	}
	ctx, cancel := lifecycleContext(c.Request().Context(), 60*time.Second)
	defer cancel()
	if err := runner.StartIfInstalled(ctx, cfg, name, info.InstallDir); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.start_failed", err))
	}
	if probeFailed {
		return c.JSON(http.StatusOK, map[string]any{
			"message": tr(c, "api.start_probe_failed"),
			"probe":   info.Probe,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.start_issued")})
}

// RecreateRunner recreates the Runner container from the current config (POST /api/runners/:name/recreate).
//
// When starting a stopped container, a mismatch between creation parameters and config triggers an automatic rebuild, but a running container is not rebuilt,
// and it is likely running a Job right now. This endpoint is the door for "I know this will interrupt, replace it now".
func RecreateRunner(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	name := c.Param("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_required"))
	}
	if !config.IsSafeRunnerNameOrPath(name) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_invalid"))
	}
	if !cfg.Runners.ContainerMode {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.recreate_container_mode_only"))
	}
	info := runner.GetByName(cfg, name)
	if info == nil {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	if info.Status != runner.StatusInstalled {
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.recreate_requires_registered", string(info.Status)))
	}
	ctx, cancel := lifecycleContext(c.Request().Context(), 90*time.Second)
	defer cancel()
	if err := runner.RecreateRunnerContainer(ctx, cfg, name, info.InstallDir); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.recreate_failed", err))
	}
	return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.recreated")})
}

// StopRunner stops the specified runner (POST /api/runners/:name/stop). In container mode, it stops the Runner container.
func StopRunner(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	name := c.Param("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_required"))
	}
	if !config.IsSafeRunnerNameOrPath(name) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_invalid"))
	}
	info := runner.GetByName(cfg, name)
	if info == nil {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	probeFailed := false
	if cfg.Runners.ContainerMode {
		applyContainerStatusOne(c.Request().Context(), cfg, info)
		if info.Probe != nil {
			probeFailed = true
			log.Printf("[stop] status probe failed for container runner name=%s, stopping anyway: %v", info.Name, info.Probe.Error)
		}
	}
	if !info.Running && !probeFailed {
		return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.not_running")})
	}
	if cfg.Runners.ContainerMode {
		// It must be longer than the grace period: StopRunnerContainer runs docker stop -t <grace period>,
		// and the Agent inside the container uses most of that time waiting for the Runner to exit. If the context expires first, the docker
		// child process gets SIGKILLed before the grace period finishes, the UI reports "stop failed", and the container is in fact still stopping.
		// Computed from the constant instead of hardcoding 35: if the grace period is raised, this cap follows, so no need to come back and edit it.
		ctx, cancel := lifecycleContext(c.Request().Context(), runnerproc.StopGracePeriod+5*time.Second)
		defer cancel()
		if err := runner.StopRunnerContainer(ctx, name); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.stop_container_failed", err))
		}
		if probeFailed {
			return c.JSON(http.StatusOK, map[string]any{
				"message": tr(c, "api.stop_probe_failed"),
				"probe":   info.Probe,
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.stopped_container")})
	}
	if err := runner.Stop(info.InstallDir); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.stop_failed", err))
	}
	return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.stop_signalled")})
}

// UpdateRunnerRequest is the request to update a runner (the name cannot change; the URL path parameter is authoritative).
// Same as AddRunnerRequest; accepts JSON only.
type UpdateRunnerRequest struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	TargetType string   `json:"target_type"`
	Target     string   `json:"target"`
	Labels     []string `json:"labels"`
	CPUs       string   `json:"cpus"`
	Memory     string   `json:"memory"`
}

// UpdateRunner updates a runner's config (PUT /api/runners/:name); the name cannot change and must match the directory
func UpdateRunner(c echo.Context) error {
	name := c.Param("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_required"))
	}
	if !config.IsSafeRunnerNameOrPath(name) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_invalid"))
	}
	var req UpdateRunnerRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.bad_request", err))
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Path = strings.TrimSpace(req.Path)
	req.TargetType = strings.TrimSpace(req.TargetType)
	req.Target = strings.TrimSpace(req.Target)
	req.Labels = normalizeLabels(req.Labels)
	// The name cannot change; only the URL path parameter is used
	if req.Name != "" && req.Name != name {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_immutable"))
	}
	if req.TargetType == "" || req.Target == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.target_required"))
	}
	targetTypeNorm := strings.ToLower(strings.TrimSpace(req.TargetType))
	if err := config.ValidateTarget(targetTypeNorm, req.Target); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if req.Path != "" && !config.IsSafeRunnerNameOrPath(req.Path) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.path_invalid"))
	}
	targetNorm := req.Target
	var updated *runner.RunnerInfo
	if err := config.LoadAndSave(ConfigPath, func(cfg *config.Config) error {
		idx := -1
		for i, item := range cfg.Runners.Items {
			if item.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
		}
		item := cfg.Runners.Items[idx]
		item.Name = name
		item.Path = req.Path
		item.TargetType = targetTypeNorm
		item.Target = targetNorm
		item.Labels = req.Labels
		item.CPUs = strings.TrimSpace(req.CPUs)
		item.Memory = strings.TrimSpace(req.Memory)
		if err := (config.ResourceLimits{CPUs: item.CPUs, Memory: item.Memory}).Validate(); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		cfg.Runners.Items[idx] = item
		return nil
	}); err != nil {
		if he, ok := err.(*echo.HTTPError); ok {
			return he
		}
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.config_save_failed", err))
	}
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	updated = runner.GetByName(cfg, name)
	if cfg.Runners.ContainerMode {
		applyLimitsQuietly(c.Request().Context(), cfg, name)
	}
	// If already registered and not running, start it automatically (container and process modes both go through StartIfInstalled)
	msg := tr(c, "api.updated")
	var started bool
	if updated != nil && updated.Status == runner.StatusInstalled && !updated.Running {
		ctx, cancel := lifecycleContext(c.Request().Context(), 60*time.Second)
		defer cancel()
		startErr := runner.StartIfInstalled(ctx, cfg, name, updated.InstallDir)
		started = (startErr == nil)
		if startErr != nil {
			msg = trf(c, "api.updated_autostart_failed", startErr)
		} else {
			msg = tr(c, "api.updated_autostarted")
		}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message": msg,
		"runner":  updated,
		"started": started,
	})
}

// deregisterFromGitHub is a variable so tests can replace it: the real implementation calls the GitHub API,
// while what this test actually pins down is "calling it before the directory is deleted". The PAT that the old docs placed in that directory
// has to be moved out before it can be used, and the source of that move is the directory itself.
var deregisterFromGitHub = githubcheck.Deregister

// RemoveRunnerByName takes the name from the path parameter and removes it (DELETE /api/runners/:name)
// It first stops the runner process, then deletes its install directory, and finally removes it from the config.
func RemoveRunnerByName(c echo.Context) error {
	name := c.Param("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_required"))
	}
	if !config.IsSafeRunnerNameOrPath(name) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_invalid"))
	}
	cfg, err := config.Load(ConfigPath)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.config_load_failed", err))
	}
	info := runner.GetByName(cfg, name)
	if info == nil {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	installDir := info.InstallDir
	// Stop the runner first: in container mode, stop and delete the container; otherwise stop the local process
	if cfg.Runners.ContainerMode {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if err := runner.RemoveRunnerContainer(ctx, name); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.remove_failed", err))
		}
	} else {
		_ = runner.Stop(installDir)
	}
	// Deregistration must happen before the directory is deleted: deleting on the GitHub side needs this runner's PAT, while the copy the old docs placed
	// in that directory has to be moved out before it can be used.
	// Deleting from this tool does not delete from GitHub. The leftover runner would make a later re-add with the same name
	// fail with "a runner with this name already exists".
	store := secretStore()
	deregCtx, deregCancel := context.WithTimeout(context.Background(), 30*time.Second)
	dereg := deregisterFromGitHub(deregCtx, store, installDir, info.TargetType, info.Target, name)
	deregCancel()
	if !dereg.Done {
		log.Printf("[remove] %s: %s", name, trEnf(dereg.MessageKey, dereg.MessageArgs...))
	}
	// Credentials can only be deleted after deregistration, since deregistration still needs them. If they were kept, the next runner with the same name would
	// silently inherit the previous one's PAT.
	// Keep credentials until installation cleanup is confirmed, so failures can be
	// inspected and retried using the existing identity and configuration.
	if installDir != "" && isUnderBasePath(cfg.Runners.BasePath, installDir) {
		if err := os.RemoveAll(installDir); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.remove_failed", err))
		}
	}
	if err := store.Remove(name); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.remove_failed", err))
	}
	if err := config.LoadAndSave(ConfigPath, func(cfg *config.Config) error {
		return removeRunnerFromConfig(c, cfg, name)
	}); err != nil {
		if he, ok := err.(*echo.HTTPError); ok {
			return he
		}
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.remove_failed", err))
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message":             trf(c, "api.removed", trf(c, dereg.MessageKey, dereg.MessageArgs...)),
		"github_deregistered": dereg.Done,
	})
}

// isUnderBasePath reports whether dir is under basePath (used for safe deletion), and dir is not basePath itself
func isUnderBasePath(basePath, dir string) bool {
	baseAbs, err := filepath.Abs(basePath)
	if err != nil {
		return false
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(baseAbs, dirAbs)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func normalizeLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		v := strings.TrimSpace(label)
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// removeRunnerFromConfig removes the specified runner from the in-memory config without writing a file (saving is handled by LoadAndSave).
//
// It takes echo.Context only so the "not found" message can be translated into the request's language. It already returns
// echo.NewHTTPError, so passing the context in adds no more coupling than before. Returning a sentinel error instead and
// translating at the call site would mean a separate error channel used only by this one place.
func removeRunnerFromConfig(c echo.Context, cfg *config.Config, name string) error {
	newItems := make([]config.RunnerItem, 0, len(cfg.Runners.Items))
	for _, item := range cfg.Runners.Items {
		if item.Name != name {
			newItems = append(newItems, item)
		}
	}
	if len(newItems) == len(cfg.Runners.Items) {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	cfg.Runners.Items = newItems
	return nil
}
