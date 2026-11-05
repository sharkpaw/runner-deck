package handler

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
)

// Metrics is the resource history. Nil until main sets it, which makes recording a no-op.
var Metrics *eventlog.MetricStore

const (
	metricSampleEvery = time.Minute
	metricFlushEvery  = 5 * time.Minute
	statsDefaultDays  = 30
	statsMaxDays      = 365
)

// JobCounts counts finished jobs.
type JobCounts struct {
	Total      int     `json:"total"`
	Succeeded  int     `json:"succeeded"`
	Failed     int     `json:"failed"`
	Canceled   int     `json:"canceled"`
	AvgSeconds float64 `json:"avg_seconds"`

	durationSum float64
	timed       int
}

// RunnerJobCounts are one runner's counts.
type RunnerJobCounts struct {
	Runner string `json:"runner"`
	JobCounts
}

// DayJobCounts are one day's counts.
type DayJobCounts struct {
	Date string `json:"date"`
	JobCounts
}

// JobStats is the body of GET /api/stats.
type JobStats struct {
	JobCounts
	Runners []RunnerJobCounts `json:"runners"`
	Days    []DayJobCounts    `json:"days"`
}

func (c *JobCounts) add(code string, seconds float64, timed bool) {
	c.Total++
	switch code {
	case "job_succeeded":
		c.Succeeded++
	case "job_failed":
		c.Failed++
	case "job_canceled":
		c.Canceled++
	}
	if timed {
		c.durationSum += seconds
		c.timed++
		c.AvgSeconds = c.durationSum / float64(c.timed)
	}
}

// buildJobStats counts finished jobs since the given time from the event history. events are
// newest first, as Store.Read returns them. A job's duration runs from the same runner's latest
// start of a job with that name, and is left out when that start is not in the history.
func buildJobStats(events []eventlog.Event, since time.Time, loc *time.Location) JobStats {
	stats := JobStats{Runners: []RunnerJobCounts{}, Days: []DayJobCounts{}}
	byRunner := map[string]*RunnerJobCounts{}
	byDay := map[string]*DayJobCounts{}
	started := map[string]time.Time{}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		key := e.Runner + "\x00" + e.Detail
		switch e.Code {
		case "job_started":
			started[key] = e.Time
			continue
		case "job_succeeded", "job_failed", "job_canceled":
		default:
			continue
		}
		var seconds float64
		begin, timed := started[key]
		if timed {
			seconds = e.Time.Sub(begin).Seconds()
			delete(started, key)
		}
		if !e.Time.After(since) {
			continue
		}
		stats.add(e.Code, seconds, timed)
		r := byRunner[e.Runner]
		if r == nil {
			r = &RunnerJobCounts{Runner: e.Runner}
			byRunner[e.Runner] = r
		}
		r.add(e.Code, seconds, timed)
		date := e.Time.In(loc).Format("2006-01-02")
		d := byDay[date]
		if d == nil {
			d = &DayJobCounts{Date: date}
			byDay[date] = d
		}
		d.add(e.Code, seconds, timed)
	}
	for _, r := range byRunner {
		stats.Runners = append(stats.Runners, *r)
	}
	sort.Slice(stats.Runners, func(i, j int) bool { return stats.Runners[i].Runner < stats.Runners[j].Runner })
	for _, d := range byDay {
		stats.Days = append(stats.Days, *d)
	}
	sort.Slice(stats.Days, func(i, j int) bool { return stats.Days[i].Date < stats.Days[j].Date })
	return stats
}

// JobStatistics serves GET /api/stats?days=30.
func JobStatistics(c echo.Context) error {
	days, err := strconv.Atoi(c.QueryParam("days"))
	if err != nil || days <= 0 {
		days = statsDefaultDays
	}
	if days > statsMaxDays {
		days = statsMaxDays
	}
	now := time.Now()
	return c.JSON(http.StatusOK, buildJobStats(Events.Read(eventlog.Query{}), now.AddDate(0, 0, -days), now.Location()))
}

// metricBucketFor picks a resolution that keeps a chart to a few hundred points per runner.
func metricBucketFor(hours int) time.Duration {
	switch {
	case hours <= 24:
		return metricFlushEvery
	case hours <= 7*24:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

// downsample merges points of each runner that fall in the same bucket: averages stay averages
// and maximums stay maximums.
func downsample(points []eventlog.MetricPoint, bucket time.Duration) []eventlog.MetricPoint {
	type key struct {
		runner string
		at     int64
	}
	type agg struct {
		p        eventlog.MetricPoint
		cpu, mem float64
		n        int
	}
	merged := map[key]*agg{}
	order := make([]key, 0, len(points))
	for _, p := range points {
		at := p.Time.Truncate(bucket)
		k := key{p.Runner, at.Unix()}
		a := merged[k]
		if a == nil {
			a = &agg{p: eventlog.MetricPoint{Time: at, Runner: p.Runner}}
			merged[k] = a
			order = append(order, k)
		}
		a.cpu += p.CPUAvg
		a.mem += float64(p.MemAvg)
		a.n++
		if p.CPUMax > a.p.CPUMax {
			a.p.CPUMax = p.CPUMax
		}
		if p.MemMax > a.p.MemMax {
			a.p.MemMax = p.MemMax
		}
	}
	out := make([]eventlog.MetricPoint, 0, len(order))
	for _, k := range order {
		a := merged[k]
		a.p.CPUAvg = a.cpu / float64(a.n)
		a.p.MemAvg = uint64(a.mem / float64(a.n))
		out = append(out, a.p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// MetricHistory serves GET /api/metrics?hours=24.
func MetricHistory(c echo.Context) error {
	hours, err := strconv.Atoi(c.QueryParam("hours"))
	if err != nil || hours <= 0 {
		hours = 24
	}
	if hours > 31*24 {
		hours = 31 * 24
	}
	points := Metrics.Read(time.Now().Add(-time.Duration(hours) * time.Hour))
	return c.JSON(http.StatusOK, map[string]any{"hours": hours, "points": downsample(points, metricBucketFor(hours))})
}

// metricCollector averages per-minute samples into the points written to disk.
type metricCollector struct {
	lastSample time.Time
	lastFlush  time.Time
	sums       map[string]*eventlog.MetricPoint
	counts     map[string]int
}

func (m *metricCollector) add(stats []containerStat, names map[string]string) {
	if m.sums == nil {
		m.sums = map[string]*eventlog.MetricPoint{}
		m.counts = map[string]int{}
	}
	for _, st := range stats {
		name, ok := names[st.Name]
		if !ok {
			continue
		}
		p := m.sums[name]
		if p == nil {
			p = &eventlog.MetricPoint{Runner: name}
			m.sums[name] = p
		}
		p.CPUAvg += st.CPUPct
		p.MemAvg += st.MemBytes
		if st.CPUPct > p.CPUMax {
			p.CPUMax = st.CPUPct
		}
		if st.MemBytes > p.MemMax {
			p.MemMax = st.MemBytes
		}
		m.counts[name]++
	}
}

func (m *metricCollector) flush(now time.Time) []eventlog.MetricPoint {
	points := make([]eventlog.MetricPoint, 0, len(m.sums))
	for name, p := range m.sums {
		n := m.counts[name]
		p.Time = now
		p.CPUAvg /= float64(n)
		p.MemAvg /= uint64(n)
		points = append(points, *p)
	}
	m.sums, m.counts = nil, nil
	return points
}

// sampleMetrics reads only the runner containers, which costs far less than `docker stats` for the
// whole host, and writes a summary every few minutes. names maps container to runner name.
func (m *metricCollector) sampleMetrics(ctx context.Context, names map[string]string, now time.Time) {
	if now.Sub(m.lastSample) >= metricSampleEvery && len(names) > 0 {
		m.lastSample = now
		args := []string{"stats", "--no-stream", "--format", "{{json .}}"}
		for container := range names {
			args = append(args, container)
		}
		// A runner stopping in between fails the whole call; the next sample covers it.
		if out, err := dockerRun(ctx, args...); err == nil {
			m.add(parseStats(out), names)
		}
	}
	if m.lastFlush.IsZero() {
		m.lastFlush = now
	}
	if now.Sub(m.lastFlush) >= metricFlushEvery {
		m.lastFlush = now
		Metrics.Append(m.flush(now), now)
	}
}
