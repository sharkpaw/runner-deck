package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
)

// No test in this package may reach the real docker through these: the disk guard prunes for real.
func init() {
	dockerRunCombined = func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("real docker is not available in tests")
	}
	diskUsageOf = func(string) *SystemDisk { return &SystemDisk{TotalBytes: 100, UsedBytes: 1} }
}

type guardFixture struct {
	w       *Watcher
	cfg     *config.Config
	now     time.Time
	used    []uint64
	calls   []string
	failAll bool
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	useEventStore(t)
	f := &guardFixture{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), cfg: &config.Config{}}
	f.cfg.Runners.ContainerMode = true
	f.w = NewWatcher(nil)
	f.w.now = func() time.Time { return f.now }
	origUsage, origDocker := diskUsageOf, dockerRunCombined
	diskUsageOf = func(string) *SystemDisk {
		pct := f.used[0]
		if len(f.used) > 1 {
			f.used = f.used[1:]
		}
		return &SystemDisk{TotalBytes: 100, UsedBytes: pct}
	}
	dockerRunCombined = func(_ context.Context, args ...string) ([]byte, error) {
		f.calls = append(f.calls, strings.Join(args, " "))
		if f.failAll {
			return []byte("daemon down"), errors.New("exit 1")
		}
		return nil, nil
	}
	t.Cleanup(func() { diskUsageOf, dockerRunCombined = origUsage, origDocker })
	return f
}

func TestGuardDisk_BelowLimitDoesNothing(t *testing.T) {
	f := newGuardFixture(t)
	f.used = []uint64{84}
	f.w.guardDisk(context.Background(), f.cfg)
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestGuardDisk_RemovesOnlyUntaggedImagesAndCache(t *testing.T) {
	f := newGuardFixture(t)
	f.used = []uint64{94, 70}
	f.w.guardDisk(context.Background(), f.cfg)
	want := "image prune -f|builder prune -af --filter until=6h"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Fatalf("calls = %s", got)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "volume") || strings.Contains(c, "image prune -a") {
			t.Errorf("must not remove volumes or tagged images: %s", c)
		}
	}
	evs := Events.Read(eventlog.Query{})
	if len(evs) != 1 || evs[0].Code != "disk_guard" || evs[0].Detail != "94% → 70%" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestGuardDisk_EscalatesAndWarnsWhenStillFull(t *testing.T) {
	f := newGuardFixture(t)
	f.used = []uint64{95, 92, 91}
	f.failAll = true
	f.w.guardDisk(context.Background(), f.cfg)
	if len(f.calls) != 3 || f.calls[2] != "builder prune -af" {
		t.Fatalf("calls = %v", f.calls)
	}
	evs := Events.Read(eventlog.Query{})
	if len(evs) != 2 || evs[0].Code != "disk_still_full" || evs[0].Detail != "91%" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestGuardDisk_RespectsIntervalCooldownAndOff(t *testing.T) {
	f := newGuardFixture(t)
	f.used = []uint64{95}
	f.w.guardDisk(context.Background(), f.cfg)
	first := len(f.calls)
	f.now = f.now.Add(time.Minute)
	f.w.guardDisk(context.Background(), f.cfg)
	f.now = f.now.Add(10 * time.Minute)
	f.w.guardDisk(context.Background(), f.cfg)
	if len(f.calls) != first {
		t.Errorf("must wait for the cooldown: %v", f.calls)
	}
	f.now = f.now.Add(time.Hour)
	f.w.guardDisk(context.Background(), f.cfg)
	if len(f.calls) == first {
		t.Error("must run again after the cooldown")
	}

	f.calls = nil
	off := 0
	f.cfg.Runners.DiskGuardPercent = &off
	f.now = f.now.Add(2 * time.Hour)
	f.w.guardDisk(context.Background(), f.cfg)
	if len(f.calls) != 0 {
		t.Errorf("0 turns it off: %v", f.calls)
	}
}

func TestGuardDisk_WaitsForARunningCleanup(t *testing.T) {
	f := newGuardFixture(t)
	f.used = []uint64{95}
	cleanMu.Lock()
	f.w.guardDisk(context.Background(), f.cfg)
	cleanMu.Unlock()
	if len(f.calls) != 0 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestUsedPercent(t *testing.T) {
	if usedPercent(nil) != 0 || usedPercent(&SystemDisk{}) != 0 {
		t.Error("an unknown disk must not trigger cleanup")
	}
}

func TestGuardDisk_LeavesAMachineWithoutContainerModeAlone(t *testing.T) {
	f := newGuardFixture(t)
	f.cfg.Runners.ContainerMode = false
	f.used = []uint64{99}
	f.w.guardDisk(context.Background(), f.cfg)
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v", f.calls)
	}
}
