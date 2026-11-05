package handler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
)

const (
	diskGuardEvery    = 5 * time.Minute
	diskGuardCooldown = 30 * time.Minute
	cacheMinAgeFirst  = "6h"
)

// diskUsageOf is a variable so tests can simulate a full disk.
var diskUsageOf = diskUsage

// diskGuard is what the watcher remembers between checks.
type diskGuard struct {
	lastCheck time.Time
	lastRun   time.Time
}

func usedPercent(d *SystemDisk) int {
	if d == nil || d.TotalBytes == 0 {
		return 0
	}
	return int(d.UsedBytes * 100 / d.TotalBytes)
}

func runPrune(ctx context.Context, args ...string) {
	if out, err := dockerRunCombined(ctx, args...); err != nil {
		log.Printf("warning: docker %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// guardDisk keeps a shared machine from filling up, which would also take down whatever else runs
// on it. It only removes what no running service can need: untagged images and build cache. Tagged
// images are left alone because Coolify keeps them for rollback, and volumes because they hold data.
func (w *Watcher) guardDisk(ctx context.Context, cfg *config.Config) {
	limit := cfg.DiskGuardAt()
	now := w.now()
	// Without container mode this is someone's own machine, and its Docker images are not ours to remove.
	if !cfg.Runners.ContainerMode || limit <= 0 || now.Sub(w.guard.lastCheck) < diskGuardEvery {
		return
	}
	w.guard.lastCheck = now
	before := usedPercent(diskUsageOf(cfg.Runners.BasePath))
	if before < limit || now.Sub(w.guard.lastRun) < diskGuardCooldown {
		return
	}
	if !cleanMu.TryLock() {
		return
	}
	defer cleanMu.Unlock()
	w.guard.lastRun = now

	pruneCtx, cancel := context.WithTimeout(context.Background(), cleanTimeout)
	defer cancel()
	runPrune(pruneCtx, "image", "prune", "-f")
	runPrune(pruneCtx, "builder", "prune", "-af", "--filter", "until="+cacheMinAgeFirst)
	after := usedPercent(diskUsageOf(cfg.Runners.BasePath))
	if after >= limit {
		runPrune(pruneCtx, "builder", "prune", "-af")
		after = usedPercent(diskUsageOf(cfg.Runners.BasePath))
	}
	forgetDiskCaches()
	recordEvent("", eventlog.LevelWarn, "disk_guard", fmt.Sprintf("%d%% → %d%%", before, after))
	if after >= limit {
		recordEvent("", eventlog.LevelError, "disk_still_full", fmt.Sprintf("%d%%", after))
	}
}
