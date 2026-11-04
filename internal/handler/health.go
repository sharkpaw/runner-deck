package handler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	health "github.com/soulteary/health-kit/v4"
)

// serviceName appears in the response bodies of /health and /ready.
const serviceName = "runner-deck"

// livenessAgg is the aggregator for /health: **it registers no checks**.
//
// This is intentional, not unfinished. /health has always been documented as "for Ingress/K8s probes", but K8s'
// livenessProbe failure means **restarting the container**. A broken config file or a lost mount volume will not be fixed by one restart
// either. It just becomes a crashloop, which is harder to debug than a live process that only reports errors.
// So /health keeps its original meaning: if the process is alive, it returns 200.
//
// The real dependency checks live in /ready; see readinessAgg.
var livenessAgg = health.NewAggregator(
	health.DefaultConfig().WithServiceName(serviceName),
)

// Health is a health check for load balancers or K8s probes.
//
// The response body changes from {"status":"ok"} to {"status":"ok","service":"runner-deck"} —
// The status value is unchanged (StatusHealthy in health-kit is "ok"); only one field is added.
// The status code is also unchanged: it is always 200 while the process is alive.
//
// This endpoint is **always exempt from auth** (basicAuthMiddleware has a Skipper that lets it through), so use
// DefaultConfig: IncludeDetails and IncludeChecks are both false, so no internal details are leaked.
func Health(c echo.Context) error {
	return writeHealth(c, livenessAgg)
}

// readinessAgg is the aggregator for /ready; it registers the checks for "can it do work right now".
//
// Both probes only read config and filesystem metadata; they spawn no subprocesses and make no network requests. Probes may run every few seconds
// so they must not run docker exec or open network connections.
var readinessAgg = health.NewAggregator(
	health.DefaultConfig().
		WithServiceName(serviceName).
		// They must be explicitly marked as critical. A non-critical failure only counts as degraded, and health-kit maps
		// degraded to 200 ("Degraded is still functional"), which would make these two
		// probes useless: they would never take the instance out of rotation.
		WithCriticalChecks([]string{"config", "base_path"}),
).AddCheckers(
	health.NewCustomChecker("config", checkConfigLoads),
	health.NewCustomChecker("base_path", checkBasePathUsable),
)

// Ready is the readiness check: can the config be read, and does the runner base directory exist and is it writable.
//
// It is separate from /health because the right response to failure differs: if /health fails, K8s restarts the container,
// but a restart does not fix a broken config; if /ready fails, the instance is just taken out of rotation, which is exactly what we want.
//
// It is also exempt from auth (probes cannot supply Basic Auth credentials), so it also uses DefaultConfig:
// on failure it only returns "status":"unhealthy", without naming the failing check or, much less, any path.
func Ready(c echo.Context) error {
	return writeHealth(c, readinessAgg)
}

func writeHealth(c echo.Context, agg *health.Aggregator) error {
	d := health.Decide(c.Request().Context(), agg, health.RequestSource(c.Request()))
	if d.Forbidden {
		// This is not reached when IPWhitelist is not configured; it is kept so that once one is set, the check does not silently turn into a 200
		return echo.NewHTTPError(d.StatusCode, "Forbidden")
	}
	return c.JSON(d.StatusCode, d.Body)
}

// checkConfigLoads checks whether the config file can be read and parsed.
func checkConfigLoads(context.Context) error {
	if _, err := config.Load(ConfigPath); err != nil {
		return fmt.Errorf("cannot load the configuration: %w", err)
	}
	return nil
}

// checkBasePathUsable checks whether the runner base path exists, is a directory, and is writable.
//
// It only stats the path and creates and deletes one temp file, without recursing. A dropped mount volume is the case this most needs to catch:
// the directory still exists (the mount point itself), but writes fail, so the Manager cannot create any runner.
func checkBasePathUsable(context.Context) error {
	cfg, err := config.Load(ConfigPath)
	if err != nil {
		return fmt.Errorf("cannot load the configuration: %w", err)
	}
	base := cfg.Runners.BasePath
	info, err := os.Stat(base)
	if err != nil {
		return fmt.Errorf("the runner base directory is not reachable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the runner base directory is not a directory: %s", base)
	}
	probe := filepath.Join(base, ".health-write-probe")
	if err := os.WriteFile(probe, []byte{}, 0o600); err != nil {
		return fmt.Errorf("the runner base directory is not writable: %w", err)
	}
	// Failing to delete does not count as a failure: a successful write has already proven what needs proving, and a leftover empty file does no harm.
	_ = os.Remove(probe)
	return nil
}
