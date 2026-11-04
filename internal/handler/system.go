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
	Name      string  `json:"name"`
	Target    string  `json:"target"`
	Running   bool    `json:"running"`
	HasStats  bool    `json:"has_stats"`
	CPUPct    float64 `json:"cpu_pct"`
	MemBytes  uint64  `json:"mem_bytes"`
	MemLimit  uint64  `json:"mem_limit_bytes"`
	DiskBytes *uint64 `json:"disk_bytes"`
}

// SystemOthers sums containers that are not runners, including those a job started
// through the host socket. They cost the same machine but are not limited by a
// runner's own resource limits.
type SystemOthers struct {
	Count    int     `json:"count"`
	CPUPct   float64 `json:"cpu_pct"`
	MemBytes uint64  `json:"mem_bytes"`
}

// SystemResponse is the body of GET /api/system.
type SystemResponse struct {
	Disk          *SystemDisk         `json:"disk"`
	Runners       []SystemRunner      `json:"runners"`
	Others        SystemOthers        `json:"others"`
	Docker        []SystemDockerUsage `json:"docker"`
	ContainerMode bool                `json:"container_mode"`
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

	statsCache  ttlCache[[]containerStat]
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
func buildSystemRunners(list []runner.RunnerInfo, stats []containerStat, sizes map[string]uint64, containerName func(string) string) ([]SystemRunner, SystemOthers) {
	byName := make(map[string]containerStat, len(stats))
	for _, s := range stats {
		byName[s.Name] = s
	}
	runners := make([]SystemRunner, 0, len(list))
	for _, item := range list {
		row := SystemRunner{Name: item.Name, Target: item.Target, Running: item.Running}
		cn := containerName(item.Name)
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

func collectSystem(cfg *config.Config, list []runner.RunnerInfo) SystemResponse {
	resp := SystemResponse{ContainerMode: cfg.Runners.ContainerMode, HostCPUs: runtime.NumCPU(), Disk: diskUsage(cfg.Runners.BasePath)}

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
	resp.Runners, resp.Others = buildSystemRunners(list, stats, sizes, runner.ContainerName)

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
