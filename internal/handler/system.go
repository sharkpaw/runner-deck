package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

// System resources endpoint behind the "System resources" page.
//
// Docker's numbers are expensive: `docker stats --no-stream` waits for a second
// sample and `docker system df` walks every image and volume, and walking a runner's
// _work directory can touch millions of files. The page polls every 5 seconds, so
// each source is cached for its own lifetime and sizes are computed in the background.
const (
	systemStatsTTL   = 4 * time.Second
	systemDockerTTL  = time.Minute
	systemDirSizeTTL = 5 * time.Minute
	systemCmdTimeout = 30 * time.Second
	systemCgroupTTL  = 30 * time.Second
)

// SystemDisk is the filesystem that holds the runner directories.
type SystemDisk struct {
	Path       string `json:"path"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

// SystemDockerUsage is one row of `docker system df`.
type SystemDockerUsage struct {
	Type        string `json:"type"`
	Count       string `json:"count"`
	Size        string `json:"size"`
	Reclaimable string `json:"reclaimable"`
}

// SystemRunner joins a configured runner with its live container numbers.
type SystemRunner struct {
	Name            string  `json:"name"`
	Target          string  `json:"target"`
	Running         bool    `json:"running"`
	HasStats        bool    `json:"has_stats"`
	CPUPct          float64 `json:"cpu_pct"`
	CPULimit        string  `json:"cpu_limit"`
	MemLimitSetting string  `json:"memory_limit"`
	CPUOverride     string  `json:"cpu_override,omitempty"`
	MemoryOverride  string  `json:"memory_override,omitempty"`
	OOMKills        uint64  `json:"oom_kills"`
	ThrottledPct    float64 `json:"cpu_throttled_pct"`
	MemBytes        uint64  `json:"mem_bytes"`
	MemLimit        uint64  `json:"mem_limit_bytes"`
	DiskBytes       *uint64 `json:"disk_bytes"`
}

// SystemOthers sums containers that are not runners, including those a job started
// through the host socket. They cost the same machine but are not limited by a
// runner's own resource limits.
type SystemOthers struct {
	Count    int     `json:"count"`
	CPUPct   float64 `json:"cpu_pct"`
	MemBytes uint64  `json:"mem_bytes"`
}

// SystemLimits are the limits every runner gets unless it has its own.
type SystemLimits struct {
	CPUs   string `json:"cpus"`
	Memory string `json:"memory"`
	// Nil in a request leaves the value as it is.
	AutoCleanDays    *int `json:"auto_clean_days,omitempty"`
	DiskGuardPercent *int `json:"disk_guard_percent,omitempty"`
}

// cgroupStats says whether a runner has hit its limits since its container started.
type cgroupStats struct {
	OOMKills     uint64
	ThrottledPct float64
}

// SystemResponse is the body of GET /api/system.
type SystemResponse struct {
	Disk          *SystemDisk         `json:"disk"`
	Runners       []SystemRunner      `json:"runners"`
	Others        SystemOthers        `json:"others"`
	Docker        []SystemDockerUsage `json:"docker"`
	ContainerMode bool                `json:"container_mode"`
	Limits        SystemLimits        `json:"limits"`
	// HostCPUs scales the CPU meters: docker stats reports 100% per core.
	HostCPUs   int    `json:"host_cpus"`
	StatsError string `json:"stats_error,omitempty"`
}

type containerStat struct {
	Name     string
	CPUPct   float64
	MemBytes uint64
	MemLimit uint64
}

var (
	dockerRun = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).Output()
	}

	dockerRunCombined = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}

	statsCache  ttlCache[[]containerStat]
	cgroupCache ttlCache[map[string]cgroupStats]
	dockerCache ttlCache[[]SystemDockerUsage]

	dirSizesMu       sync.Mutex
	dirSizes         = map[string]uint64{}
	dirSizesAt       time.Time
	dirSizesScanning bool

	sizeRe = regexp.MustCompile(`^([0-9.]+)\s*([A-Za-z]*)$`)
)

// ttlCache keeps one value for a fixed time. The lock is held while loading, so
// concurrent pollers share a single docker process instead of starting one each.
type ttlCache[T any] struct {
	mu   sync.Mutex
	at   time.Time
	val  T
	good bool
}

// get keeps the last good value when a load fails, so one slow or failed docker call on a busy
// host does not blank the page. The error only surfaces while there is nothing to show.
func (c *ttlCache[T]) get(ttl time.Duration, load func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.good && time.Since(c.at) < ttl {
		return c.val, nil
	}
	val, err := load()
	if err != nil {
		if c.good {
			return c.val, nil
		}
		return val, err
	}
	c.val, c.good, c.at = val, true, time.Now()
	return val, nil
}

// parseSize reads the human sizes docker prints: "12.5MiB", "1.2GB", "512kB", "0B".
func parseSize(s string) uint64 {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	mult := 1.0
	switch strings.ToLower(m[2]) {
	case "kb":
		mult = 1e3
	case "mb":
		mult = 1e6
	case "gb":
		mult = 1e9
	case "tb":
		mult = 1e12
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	}
	return uint64(v * mult)
}

// parseStats reads `docker stats --format '{{json .}}'`, one JSON object per line.
// A malformed line is skipped: one odd container must not blank the whole page.
func parseStats(out []byte) []containerStat {
	var stats []containerStat
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		var row struct {
			Name     string `json:"Name"`
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil || row.Name == "" {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(row.CPUPerc), "%"), 64)
		used, limit, _ := strings.Cut(row.MemUsage, "/")
		stats = append(stats, containerStat{
			Name:     row.Name,
			CPUPct:   cpu,
			MemBytes: parseSize(used),
			MemLimit: parseSize(limit),
		})
	}
	return stats
}

// parseDockerDF reads `docker system df --format '{{json .}}'`.
func parseDockerDF(out []byte) []SystemDockerUsage {
	var rows []SystemDockerUsage
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		var row struct {
			Type        string `json:"Type"`
			TotalCount  any    `json:"TotalCount"`
			Size        string `json:"Size"`
			Reclaimable string `json:"Reclaimable"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil || row.Type == "" {
			continue
		}
		count := ""
		switch v := row.TotalCount.(type) {
		case string:
			count = v
		case float64:
			count = strconv.FormatFloat(v, 'f', -1, 64)
		}
		rows = append(rows, SystemDockerUsage{Type: row.Type, Count: count, Size: row.Size, Reclaimable: row.Reclaimable})
	}
	return rows
}

// parseCgroup reads memory.events and cpu.stat of a cgroup v2 container. The kernel's OOM killer
// usually takes a job's process, not the container's PID 1, so docker inspect's OOMKilled stays
// false; oom_kill counts every case.
func parseCgroup(out []byte) cgroupStats {
	var st cgroupStats
	var periods, throttled uint64
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "oom_kill":
			st.OOMKills = n
		case "nr_periods":
			periods = n
		case "nr_throttled":
			throttled = n
		}
	}
	if periods > 0 {
		st.ThrottledPct = float64(throttled) * 100 / float64(periods)
	}
	return st
}

func loadCgroups(names []string) map[string]cgroupStats {
	out := make(map[string]cgroupStats, len(names))
	for _, name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, err := dockerRun(ctx, "exec", name, "cat", "/sys/fs/cgroup/memory.events", "/sys/fs/cgroup/cpu.stat")
		cancel()
		if err == nil {
			out[name] = parseCgroup(raw)
		}
	}
	return out
}

func diskUsage(path string) *SystemDisk {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil
	}
	bsize := uint64(st.Bsize)
	total := uint64(st.Blocks) * bsize
	free := uint64(st.Bavail) * bsize
	used := uint64(0)
	if total > free {
		used = total - free
	}
	return &SystemDisk{Path: path, TotalBytes: total, FreeBytes: free, UsedBytes: used}
}

// dirSize sums regular files. Unreadable entries are skipped: a partial size is more
// useful here than none, and a job may be deleting files while the walk runs.
func dirSize(root string) uint64 {
	var total uint64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total
}

// refreshDirSizes starts one background scan when the last one is stale. The request
// never waits for it: the page shows the previous sizes, or none on the first load.
func refreshDirSizes(dirs map[string]string) {
	dirSizesMu.Lock()
	defer dirSizesMu.Unlock()
	if dirSizesScanning || (!dirSizesAt.IsZero() && time.Since(dirSizesAt) < systemDirSizeTTL) {
		return
	}
	dirSizesScanning = true
	go func() {
		next := make(map[string]uint64, len(dirs))
		for name, dir := range dirs {
			next[name] = dirSize(dir)
		}
		dirSizesMu.Lock()
		defer dirSizesMu.Unlock()
		dirSizes = next
		dirSizesAt = time.Now()
		dirSizesScanning = false
	}()
}

// buildSystemRunners joins the configured runners with the live stats and returns
// what is left over as the "other containers" total.
func buildSystemRunners(cfg *config.Config, list []runner.RunnerInfo, stats []containerStat, sizes map[string]uint64, cgroups map[string]cgroupStats, containerName func(string) string) ([]SystemRunner, SystemOthers) {
	byName := make(map[string]containerStat, len(stats))
	for _, s := range stats {
		byName[s.Name] = s
	}
	runners := make([]SystemRunner, 0, len(list))
	for _, item := range list {
		limits := cfg.ResourcesFor(item.Name)
		row := SystemRunner{
			Name: item.Name, Target: item.Target, Running: item.Running,
			CPULimit: limits.CPUs, MemLimitSetting: limits.Memory,
			CPUOverride: item.CPUs, MemoryOverride: item.Memory,
		}
		cn := containerName(item.Name)
		if cg, ok := cgroups[cn]; ok {
			row.OOMKills = cg.OOMKills
			row.ThrottledPct = cg.ThrottledPct
		}
		if s, ok := byName[cn]; ok {
			row.HasStats = true
			row.Running = true
			row.CPUPct = s.CPUPct
			row.MemBytes = s.MemBytes
			row.MemLimit = s.MemLimit
			delete(byName, cn)
		}
		if size, ok := sizes[item.Name]; ok {
			row.DiskBytes = &size
		}
		runners = append(runners, row)
	}
	var others SystemOthers
	for _, s := range byName {
		others.Count++
		others.CPUPct += s.CPUPct
		others.MemBytes += s.MemBytes
	}
	return runners, others
}

func systemLimitsOf(cfg *config.Config) SystemLimits {
	days, guard := cfg.AutoCleanAfter(), cfg.DiskGuardAt()
	return SystemLimits{CPUs: cfg.Runners.Resources.CPUs, Memory: cfg.Runners.Resources.Memory, AutoCleanDays: &days, DiskGuardPercent: &guard}
}

func collectSystem(cfg *config.Config, list []runner.RunnerInfo) SystemResponse {
	resp := SystemResponse{
		ContainerMode: cfg.Runners.ContainerMode,
		HostCPUs:      runtime.NumCPU(),
		Disk:          diskUsage(cfg.Runners.BasePath),
		Limits:        systemLimitsOf(cfg),
	}

	dirs := make(map[string]string, len(list))
	for _, item := range list {
		dirs[item.Name] = item.InstallDir
	}
	refreshDirSizes(dirs)
	dirSizesMu.Lock()
	sizes := make(map[string]uint64, len(dirSizes))
	for k, v := range dirSizes {
		sizes[k] = v
	}
	dirSizesMu.Unlock()

	// Process runners have no containers, and any Docker on this machine is not ours to report on.
	if !cfg.Runners.ContainerMode {
		resp.Runners, resp.Others = buildSystemRunners(cfg, list, nil, sizes, nil, runner.ContainerName)
		running := make(map[string]bool, len(list))
		for _, item := range list {
			running[item.Name] = item.Running
		}
		for i := range resp.Runners {
			resp.Runners[i].Running = running[resp.Runners[i].Name]
		}
		return resp
	}

	// Not tied to the request: the result is shared, so a reloaded page must not kill the docker call.
	stats, err := statsCache.get(systemStatsTTL, func() ([]containerStat, error) {
		cctx, cancel := context.WithTimeout(context.Background(), systemCmdTimeout)
		defer cancel()
		out, err := dockerRun(cctx, "stats", "--no-stream", "--format", "{{json .}}")
		if err != nil {
			return nil, err
		}
		return parseStats(out), nil
	})
	if err != nil {
		resp.StatsError = err.Error()
	}
	cgroups, _ := cgroupCache.get(systemCgroupTTL, func() (map[string]cgroupStats, error) {
		names := make([]string, 0, len(list))
		running := make(map[string]bool, len(stats))
		for _, st := range stats {
			running[st.Name] = true
		}
		for _, item := range list {
			if cn := runner.ContainerName(item.Name); running[cn] {
				names = append(names, cn)
			}
		}
		return loadCgroups(names), nil
	})
	resp.Runners, resp.Others = buildSystemRunners(cfg, list, stats, sizes, cgroups, runner.ContainerName)

	// `docker system df` failing only hides its table, so the error is not surfaced.
	resp.Docker, _ = dockerCache.get(systemDockerTTL, func() ([]SystemDockerUsage, error) {
		cctx, cancel := context.WithTimeout(context.Background(), systemCmdTimeout)
		defer cancel()
		out, err := dockerRun(cctx, "system", "df", "--format", "{{json .}}")
		if err != nil {
			return nil, err
		}
		return parseDockerDF(out), nil
	})
	return resp
}

// SystemInfo serves GET /api/system.
func SystemInfo(c echo.Context) error {
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	// No agent probe here: `docker stats` only lists running containers, so it already
	// answers "is it running", and probing every runner each poll would slow the page.
	list := runner.List(cfg)
	return c.JSON(http.StatusOK, collectSystem(cfg, list))
}

// applyLimitsQuietly applies a runner's limits to its running container. Failing to is not fatal:
// StartRunnerContainer applies them again on the next start.
func applyLimitsQuietly(parent context.Context, cfg *config.Config, name string) []string {
	ctx, cancel := context.WithTimeout(parent, systemCmdTimeout)
	defer cancel()
	if err := runner.UpdateContainerLimits(ctx, cfg, name); err != nil {
		return []string{name + ": " + err.Error()}
	}
	return nil
}

// UpdateResourceLimits serves PUT /api/settings/resources: the limits every runner gets unless it
// has its own. Running containers are updated in place, so no job is interrupted.
func UpdateResourceLimits(c echo.Context) error {
	var req SystemLimits
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.bad_request", err))
	}
	req.CPUs = strings.TrimSpace(req.CPUs)
	req.Memory = strings.TrimSpace(req.Memory)
	if req.CPUs == "" || req.Memory == "" {
		return echo.NewHTTPError(http.StatusBadRequest, tr(c, "api.limits_required"))
	}
	if err := (config.ResourceLimits{CPUs: req.CPUs, Memory: req.Memory}).Validate(); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if d := req.AutoCleanDays; d != nil && (*d < 0 || *d > config.MaxAutoCleanDays) {
		return echo.NewHTTPError(http.StatusBadRequest, trf(c, "api.bad_request", *d))
	}
	if err := config.LoadAndSave(ConfigPath, func(cfg *config.Config) error {
		if req.AutoCleanDays != nil {
			cfg.Runners.AutoCleanDays = req.AutoCleanDays
		}
		if req.DiskGuardPercent != nil {
			cfg.Runners.DiskGuardPercent = req.DiskGuardPercent
		}
		cfg.Runners.Resources.CPUs = req.CPUs
		cfg.Runners.Resources.Memory = req.Memory
		// A swap total below the new memory would make docker refuse every container.
		cfg.Runners.Resources.MemorySwap = ""
		return nil
	}); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, trf(c, "api.config_save_failed", err))
	}
	recordEvent("", eventlog.LevelInfo, "limits_updated", req.CPUs+" CPU / "+req.Memory)
	cfg, err := getConfig(c)
	if err != nil {
		return err
	}
	var failed []string
	if cfg.Runners.ContainerMode {
		for _, item := range cfg.Runners.Items {
			failed = append(failed, applyLimitsQuietly(c.Request().Context(), cfg, item.Name)...)
		}
	}
	if len(failed) > 0 {
		return c.JSON(http.StatusOK, map[string]any{"message": trf(c, "api.limits_partly_applied", strings.Join(failed, "; "))})
	}
	return c.JSON(http.StatusOK, map[string]any{"message": tr(c, "api.limits_saved")})
}
