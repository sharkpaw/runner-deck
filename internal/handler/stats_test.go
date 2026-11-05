package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/eventlog"
)

func TestBuildJobStats(t *testing.T) {
	base := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	oldest := []eventlog.Event{
		{Time: at(-3000), Runner: "a", Code: "job_started", Detail: "old"},
		{Time: at(-2990), Runner: "a", Code: "job_succeeded", Detail: "old"},
		{Time: at(0), Runner: "a", Code: "job_started", Detail: "verify"},
		{Time: at(0), Runner: "b", Code: "started"},
		{Time: at(10), Runner: "a", Code: "job_succeeded", Detail: "verify"},
		{Time: at(20), Runner: "a", Code: "job_started", Detail: "verify"},
		{Time: at(40), Runner: "a", Code: "job_failed", Detail: "verify"},
		{Time: at(50), Runner: "b", Code: "job_canceled", Detail: "no-start"},
	}
	newestFirst := make([]eventlog.Event, 0, len(oldest))
	for i := len(oldest) - 1; i >= 0; i-- {
		newestFirst = append(newestFirst, oldest[i])
	}
	s := buildJobStats(newestFirst, at(-60), time.UTC)
	if s.Total != 3 || s.Succeeded != 1 || s.Failed != 1 || s.Canceled != 1 {
		t.Fatalf("%+v", s.JobCounts)
	}
	if s.AvgSeconds != 900 {
		t.Errorf("avg = %v, want 900 from the two timed jobs", s.AvgSeconds)
	}
	if len(s.Runners) != 2 || s.Runners[0].Runner != "a" || s.Runners[0].Total != 2 || s.Runners[1].Total != 1 {
		t.Errorf("runners = %+v", s.Runners)
	}
	if len(s.Days) != 1 || s.Days[0].Date != "2026-11-05" || s.Days[0].Total != 3 {
		t.Errorf("days = %+v", s.Days)
	}
	if empty := buildJobStats(nil, base, time.UTC); empty.Runners == nil || empty.Days == nil {
		t.Error("empty lists must not encode as null")
	}
}

func TestDownsample(t *testing.T) {
	base := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	got := downsample([]eventlog.MetricPoint{
		{Time: base.Add(2 * time.Hour), Runner: "a", CPUAvg: 5},
		{Time: base, Runner: "a", CPUAvg: 10, CPUMax: 50, MemAvg: 100, MemMax: 200},
		{Time: base.Add(time.Minute), Runner: "a", CPUAvg: 20, CPUMax: 30, MemAvg: 300, MemMax: 400},
		{Time: base, Runner: "b", CPUAvg: 1},
	}, time.Hour)
	if len(got) != 3 || got[2].CPUAvg != 5 {
		t.Fatalf("must be merged and in time order: %+v", got)
	}
	if got[0].CPUAvg != 15 || got[0].CPUMax != 50 || got[0].MemAvg != 200 || got[0].MemMax != 400 {
		t.Errorf("merged = %+v", got[0])
	}
}

func TestMetricBucketFor(t *testing.T) {
	for hours, want := range map[int]time.Duration{24: 5 * time.Minute, 168: time.Hour, 720: 6 * time.Hour} {
		if got := metricBucketFor(hours); got != want {
			t.Errorf("%d: %v", hours, got)
		}
	}
}

func TestMetricCollector_SamplesRunnersAndFlushes(t *testing.T) {
	orig := Metrics
	Metrics = eventlog.NewMetricStore(filepath.Join(t.TempDir(), "m.jsonl"))
	t.Cleanup(func() { Metrics = orig })

	var gotArgs []string
	fail := false
	stubDocker(t, func(args ...string) ([]byte, error) {
		gotArgs = args
		if fail {
			return nil, errors.New("no such container")
		}
		return []byte(`{"Name":"c-a","CPUPerc":"10%","MemUsage":"10MiB / 1GiB"}
{"Name":"other","CPUPerc":"99%","MemUsage":"1GiB / 1GiB"}
`), nil
	})
	names := map[string]string{"c-a": "a"}
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	var m metricCollector
	m.sampleMetrics(context.Background(), names, now)
	if len(gotArgs) != 5 || gotArgs[4] != "c-a" {
		t.Errorf("must ask only for runner containers: %v", gotArgs)
	}
	gotArgs = nil
	m.sampleMetrics(context.Background(), names, now.Add(10*time.Second))
	if gotArgs != nil {
		t.Error("must not sample more than once a minute")
	}
	fail = true
	m.sampleMetrics(context.Background(), names, now.Add(time.Minute))
	fail = false
	m.sampleMetrics(context.Background(), names, now.Add(2*time.Minute))
	m.sampleMetrics(context.Background(), map[string]string{}, now.Add(5*time.Minute))
	got := Metrics.Read(time.Time{})
	if len(got) != 1 || got[0].Runner != "a" || got[0].CPUAvg != 10 || got[0].CPUMax != 10 || got[0].MemMax != 10<<20 {
		t.Fatalf("%+v", got)
	}
}

func TestStatisticsEndpoints(t *testing.T) {
	store := useEventStore(t)
	now := time.Now()
	store.Append(eventlog.Event{Time: now, Runner: "a", Level: eventlog.LevelError, Code: "job_failed", Detail: "x"})
	store.Append(eventlog.Event{Time: now.AddDate(0, 0, -10), Runner: "a", Level: eventlog.LevelInfo, Code: "job_succeeded", Detail: "x"})
	for query, want := range map[string]int{"": 2, "?days=7": 1, "?days=x": 2, "?days=99999": 2} {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/stats"+query, nil), rec)
		if err := JobStatistics(c); err != nil {
			t.Fatal(err)
		}
		var s JobStats
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		if s.Total != want {
			t.Errorf("%q: total %d, want %d", query, s.Total, want)
		}
	}

	orig := Metrics
	Metrics = eventlog.NewMetricStore(filepath.Join(t.TempDir(), "m.jsonl"))
	t.Cleanup(func() { Metrics = orig })
	Metrics.Append([]eventlog.MetricPoint{{Time: now.Add(-time.Hour), Runner: "a", CPUAvg: 3}}, now)
	for query, want := range map[string]float64{"": 24, "?hours=x": 24, "?hours=168": 168, "?hours=99999": 744} {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/metrics"+query, nil), rec)
		if err := MetricHistory(c); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Hours  float64                `json:"hours"`
			Points []eventlog.MetricPoint `json:"points"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Hours != want || len(body.Points) != 1 {
			t.Errorf("%q: %+v", query, body)
		}
	}
}
