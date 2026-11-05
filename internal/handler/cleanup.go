package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

const (
	cleanTimeout     = 10 * time.Minute
	cleanBusyExit    = 3
	cleanBuildMinAge = "24h"
	cleanImageMinAge = "168h"
)

// cleanWorkScript empties a runner's work directory, except the tool cache, which only saves
// downloads. It refuses while a job runs: `[R]unner` keeps this script from matching itself.
const cleanWorkScript = `for p in /proc/[0-9]*/cmdline; do
  if tr '\0' ' ' < "$p" 2>/dev/null | grep -q '[R]unner\.Worker'; then exit 3; fi
done
find /runner/_work -mindepth 1 -maxdepth 1 ! -name _tool -exec rm -rf {} +`

var (
	cleanMu     sync.Mutex
	reclaimedRe = regexp.MustCompile(`(?m)^Total(?: reclaimed space:|:)\s*(\S+)`)
	exitCodeOf  = func(err error) int {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return -1
	}
)

// forgetDiskCaches makes the next system page show the result of a cleanup.
func forgetDiskCaches() {
	dockerCache = ttlCache[[]SystemDockerUsage]{}
	dirSizesMu.Lock()
	dirSizesAt = time.Time{}
	dirSizesMu.Unlock()
}

// cleanContainerWork empties a runner container's work directory.
func cleanContainerWork(ctx context.Context, name string) error {
	out, err := dockerRunCombined(ctx, "exec", "-u", "0", runner.ContainerName(name), "sh", "-c", cleanWorkScript)
	if err == nil {
		return nil
	}
	if exitCodeOf(err) == cleanBusyExit {
		return runner.ErrWorkBusy
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return err
}

// CleanRunnerWork serves POST /api/runners/:name/clean. It runs inside the container as root,
// because a job may have left root-owned files that the Manager cannot delete.
func CleanRunnerWork(c echo.Context) error {
	name := c.Param("name")
	if !config.IsSafeRunnerNameOrPath(name) {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.name_invalid"))
	}
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	item, ok := cfg.FindItem(name)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanTimeout)
	defer cancel()
	if cfg.Runners.ContainerMode {
		err = cleanContainerWork(ctx, name)
	} else {
		err = runner.CleanWork(item.InstallPath(cfg.Runners.BasePath))
	}
	if err != nil {
		if errors.Is(err, runner.ErrWorkBusy) {
			return echo.NewHTTPError(http.StatusConflict, tr(c, "api.clean_busy"))
		}
		recordEvent(name, eventlog.LevelError, "clean_failed", err.Error())
		return echo.NewHTTPError(http.StatusBadGateway, trf(c, "api.clean_failed", err.Error()))
	}
	recordEvent(name, eventlog.LevelInfo, "work_cleaned", "")
	forgetDiskCaches()
	return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.work_cleaned")})
}

// CleanDocker serves POST /api/docker/clean with {"target": "build-cache" | "images"}. Volumes
// are not offered: a volume looks unused while its service is stopped, and it may hold a database.
func CleanDocker(c echo.Context) error {
	var req struct {
		Target string `json:"target"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.bad_request", err))
	}
	var args []string
	switch req.Target {
	case "build-cache":
		args = []string{"builder", "prune", "-af", "--filter", "until=" + cleanBuildMinAge}
	case "images":
		args = []string{"image", "prune", "-af", "--filter", "until=" + cleanImageMinAge}
	default:
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.bad_request", req.Target))
	}
	if !cleanMu.TryLock() {
		return echo.NewHTTPError(http.StatusConflict, tr(c, "api.clean_running"))
	}
	defer cleanMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), cleanTimeout)
	defer cancel()
	out, err := dockerRunCombined(ctx, args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		recordEvent("", eventlog.LevelError, "clean_failed", msg)
		return echo.NewHTTPError(http.StatusBadGateway, trf(c, "api.clean_failed", msg))
	}
	reclaimed := "0B"
	if m := reclaimedRe.FindSubmatch(out); m != nil {
		reclaimed = string(m[1])
	}
	recordEvent("", eventlog.LevelInfo, "docker_cleaned", reclaimed)
	forgetDiskCaches()
	return c.JSON(http.StatusOK, map[string]any{"message": trf(c, "api.docker_cleaned", reclaimed), "reclaimed": reclaimed})
}
