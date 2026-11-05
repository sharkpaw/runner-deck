package handler

import (
	"context"
	"errors"
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
	if _, ok := cfg.FindItem(name); !ok {
		return echo.NewHTTPError(http.StatusNotFound, tr(c, "api.runner_not_found"))
	}
	if !cfg.Runners.ContainerMode {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.logs_container_mode_only"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanTimeout)
	defer cancel()
	out, err := dockerRunCombined(ctx, "exec", "-u", "0", runner.ContainerName(name), "sh", "-c", cleanWorkScript)
	if err != nil {
		if exitCodeOf(err) == cleanBusyExit {
			return echo.NewHTTPError(http.StatusConflict, tr(c, "api.clean_busy"))
		}
		msg := strings.TrimSpace(string(out))
		recordEvent(name, eventlog.LevelError, "clean_failed", msg)
		return echo.NewHTTPError(http.StatusBadGateway, trf(c, "api.clean_failed", msg))
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
