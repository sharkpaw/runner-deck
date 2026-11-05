package handler

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

// Events is the runner history. Nil until main sets it, which makes recording a no-op.
var Events *eventlog.Store

const (
	eventsDefaultLimit = 500
	eventsMaxLimit     = 5000
	logsDefaultTail    = 300
	logsMaxTail        = 5000
	// Comfortably more than a runner writes between two watcher passes.
	processLogBytes = 256 << 10
)

func recordEvent(name, level, code, detail string) {
	Events.Append(eventlog.Event{Runner: name, Level: level, Code: code, Detail: detail})
}

// ListEvents serves GET /api/events.
func ListEvents(c echo.Context) error {
	limit, err := strconv.Atoi(c.QueryParam("limit"))
	if err != nil || limit <= 0 {
		limit = eventsDefaultLimit
	}
	if limit > eventsMaxLimit {
		limit = eventsMaxLimit
	}
	return c.JSON(http.StatusOK, map[string]any{
		"events": Events.Read(eventlog.Query{Runner: c.QueryParam("runner"), Level: c.QueryParam("level"), Limit: limit}),
	})
}

// RunnerLogs serves GET /api/runners/:name/logs: the end of the runner container's output.
func RunnerLogs(c echo.Context) error {
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
	if !cfg.Runners.ContainerMode {
		out, err := runner.ReadLog(item.InstallPath(cfg.Runners.BasePath), processLogBytes)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, trf(c, "api.logs_failed", err.Error()))
		}
		return c.JSON(http.StatusOK, map[string]any{"logs": string(out)})
	}
	tail, err := strconv.Atoi(c.QueryParam("tail"))
	if err != nil || tail <= 0 {
		tail = logsDefaultTail
	}
	if tail > logsMaxTail {
		tail = logsMaxTail
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), systemCmdTimeout)
	defer cancel()
	out, err := dockerRunCombined(ctx, "logs", "--timestamps", "--tail", strconv.Itoa(tail), runner.ContainerName(name))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, trf(c, "api.logs_failed", strings.TrimSpace(string(out))))
	}
	return c.JSON(http.StatusOK, map[string]any{"logs": string(out)})
}

var (
	jobStartRe = regexp.MustCompile(`Running job: (.+)$`)
	jobDoneRe  = regexp.MustCompile(`Job (.+) completed with result: (\w+)`)
)

// watchedRunner is what the watcher remembers between passes.
type watchedRunner struct {
	running  bool
	oomKills uint64
	logsFrom time.Time
	// When the runner last did something worth cleaning up after; zero once it has been cleaned.
	dirtySince time.Time
	// Set for a process runner; a container's work directory is reached through docker instead.
	installDir string
}

// Watcher turns changes in runner state into events.
type Watcher struct {
	cfg     func() (*config.Config, error)
	state   map[string]*watchedRunner
	started time.Time
	metrics metricCollector
	now     func() time.Time
	guard   diskGuard
}

// NewWatcher returns a watcher reading the config through load.
func NewWatcher(load func() (*config.Config, error)) *Watcher {
	return &Watcher{cfg: load, state: map[string]*watchedRunner{}, started: time.Now(), now: time.Now}
}

// parseJobEvents reads `docker logs --timestamps` lines. It returns the events found and the time
// of the last line, to continue from.
func parseJobEvents(name string, out []byte, since time.Time) ([]eventlog.Event, time.Time) {
	var events []eventlog.Event
	last := since
	for _, line := range strings.Split(string(out), "\n") {
		stamp, msg, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || !at.After(since) {
			continue
		}
		last = at
		if m := jobDoneRe.FindStringSubmatch(msg); m != nil {
			e := eventlog.Event{Time: at, Runner: name, Level: eventlog.LevelInfo, Code: "job_succeeded", Detail: m[1]}
			switch m[2] {
			case "Succeeded":
			case "Canceled":
				e.Level, e.Code = eventlog.LevelWarn, "job_canceled"
			default:
				e.Level, e.Code = eventlog.LevelError, "job_failed"
			}
			events = append(events, e)
		} else if m := jobStartRe.FindStringSubmatch(msg); m != nil {
			events = append(events, eventlog.Event{Time: at, Runner: name, Level: eventlog.LevelInfo, Code: "job_started", Detail: m[1]})
		}
	}
	return events, last
}

func exitDetails(ctx context.Context, container string) (code string, oom bool) {
	out, err := dockerRun(ctx, "inspect", "--format", "{{.State.ExitCode}} {{.State.OOMKilled}}", container)
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return "", false
	}
	return fields[0], fields[1] == "true"
}

func (w *Watcher) checkContainer(ctx context.Context, name string, st *watchedRunner, running, first bool, days int) {
	container := runner.ContainerName(name)
	if !first && running != st.running {
		if running {
			recordEvent(name, eventlog.LevelInfo, "started", "")
			st.oomKills = 0
		} else {
			code, oom := exitDetails(ctx, container)
			switch {
			// The OOMKilled flag stays set after any kill inside the container, so only 137 means it ended this one.
			case oom && code == "137":
				recordEvent(name, eventlog.LevelError, "stopped_oom", code)
			case code != "" && code != "0":
				recordEvent(name, eventlog.LevelError, "stopped_error", code)
			default:
				recordEvent(name, eventlog.LevelInfo, "stopped", "")
			}
		}
	}
	st.running = running
	if !running {
		return
	}
	if cg, ok := loadCgroups([]string{container})[container]; ok {
		if !first && cg.OOMKills > st.oomKills {
			recordEvent(name, eventlog.LevelError, "oom", strconv.FormatUint(cg.OOMKills-st.oomKills, 10))
		}
		st.oomKills = cg.OOMKills
	}
	out, err := dockerRun(ctx, "logs", "--timestamps", "--since", st.logsFrom.Format(time.RFC3339Nano), container)
	if err != nil {
		return
	}
	events, last := parseJobEvents(name, out, st.logsFrom)
	for _, e := range events {
		Events.Append(e)
		st.dirtySince = e.Time
	}
	st.logsFrom = last
	w.autoClean(ctx, name, st, days)
}

// checkProcessLog reads job results from a process runner's log.
func (w *Watcher) checkProcessLog(ctx context.Context, name string, st *watchedRunner, days int) {
	out, err := runner.ReadLog(st.installDir, processLogBytes)
	if err != nil {
		return
	}
	events, last := parseJobEvents(name, out, st.logsFrom)
	for _, e := range events {
		Events.Append(e)
		st.dirtySince = e.Time
	}
	st.logsFrom = last
	w.autoClean(ctx, name, st, days)
}

// autoClean empties the work directory of a runner idle for the given days. A job that is running
// makes the script refuse, which simply tries again on the next pass.
func (w *Watcher) autoClean(ctx context.Context, name string, st *watchedRunner, days int) {
	if days <= 0 || st.dirtySince.IsZero() || w.now().Sub(st.dirtySince) < time.Duration(days)*24*time.Hour {
		return
	}
	var err error
	if st.installDir != "" {
		err = runner.CleanWork(st.installDir)
	} else {
		err = cleanContainerWork(ctx, name)
	}
	if err != nil {
		if !errors.Is(err, runner.ErrWorkBusy) {
			recordEvent(name, eventlog.LevelError, "clean_failed", err.Error())
			st.dirtySince = w.now()
		}
		return
	}
	recordEvent(name, eventlog.LevelInfo, "work_auto_cleaned", strconv.Itoa(days))
	st.dirtySince = time.Time{}
	forgetDiskCaches()
}

// lastActivity finds when a runner last ran a job, so a Manager restart does not restart the idle
// clock. A runner already cleaned since then, or with no history, stays as it is known.
func (w *Watcher) lastActivity(name string) time.Time {
	for _, e := range Events.Read(eventlog.Query{Runner: name}) {
		switch e.Code {
		case "work_cleaned", "work_auto_cleaned":
			return time.Time{}
		case "job_succeeded", "job_failed", "job_canceled", "job_started":
			return e.Time
		}
	}
	return w.now()
}

// Pass runs one round of checks. The first sight of a runner only sets the baseline, so restarting
// the Manager does not report every runner as having just started.
func (w *Watcher) Pass(ctx context.Context) {
	cfg, err := w.cfg()
	if err != nil {
		return
	}
	seen := make(map[string]bool, len(cfg.Runners.Items))
	active := map[string]string{}
	for _, info := range runner.List(cfg) {
		seen[info.Name] = true
		st, known := w.state[info.Name]
		if !known {
			st = &watchedRunner{logsFrom: w.started, dirtySince: w.lastActivity(info.Name)}
			w.state[info.Name] = st
		}
		if cfg.Runners.ContainerMode {
			exists, status, err := runner.ContainerState(ctx, runner.ContainerName(info.Name))
			if err != nil {
				continue
			}
			running := exists && status == "running"
			w.checkContainer(ctx, info.Name, st, running, !known, cfg.AutoCleanAfter())
			if running {
				active[runner.ContainerName(info.Name)] = info.Name
			}
			continue
		}
		st.installDir = info.InstallDir
		if known && info.Running != st.running {
			if info.Running {
				recordEvent(info.Name, eventlog.LevelInfo, "started", "")
			} else {
				recordEvent(info.Name, eventlog.LevelInfo, "stopped", "")
			}
		}
		st.running = info.Running
		w.checkProcessLog(ctx, info.Name, st, cfg.AutoCleanAfter())
	}
	for name := range w.state {
		if !seen[name] {
			delete(w.state, name)
		}
	}
	w.metrics.sampleMetrics(ctx, active, time.Now())
	w.guardDisk(ctx, cfg)
}

// Run calls Pass on an interval until ctx ends.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		passCtx, cancel := context.WithTimeout(ctx, interval)
		w.Pass(passCtx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
