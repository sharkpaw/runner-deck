package eventlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricStore_AppendReadAndPrune(t *testing.T) {
	s := MetricsForConfig(filepath.Join(t.TempDir(), "config.yaml"))
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	old := MetricPoint{Time: now.Add(-40 * 24 * time.Hour), Runner: "a", CPUAvg: 1}
	recent := MetricPoint{Time: now.Add(-time.Hour), Runner: "a", CPUAvg: 2, MemMax: 9}

	s.lastPruned = now
	s.Append([]MetricPoint{old, recent}, now)
	if got := s.Read(time.Time{}); len(got) != 2 {
		t.Fatalf("before pruning: %+v", got)
	}
	if got := s.Read(now.Add(-2 * time.Hour)); len(got) != 1 || got[0].MemMax != 9 {
		t.Fatalf("since filter: %+v", got)
	}

	s.Append([]MetricPoint{{Time: now, Runner: "a"}}, now.Add(25*time.Hour))
	got := s.Read(time.Time{})
	if len(got) != 2 || got[0].CPUAvg != 2 {
		t.Fatalf("after pruning: %+v", got)
	}
	s.Append(nil, now)
}

func TestMetricStore_SurvivesBadInput(t *testing.T) {
	var nilStore *MetricStore
	nilStore.Append([]MetricPoint{{}}, time.Now())
	if len(nilStore.Read(time.Time{})) != 0 {
		t.Error("nil store")
	}
	dir := t.TempDir()
	if got := NewMetricStore(filepath.Join(dir, "missing")).Read(time.Time{}); got == nil || len(got) != 0 {
		t.Errorf("missing file: %#v", got)
	}
	NewMetricStore(filepath.Join(dir, "no-dir", "m")).Append([]MetricPoint{{Runner: "a"}}, time.Now())

	path := filepath.Join(dir, "m.jsonl")
	line := `{"t":"2026-11-05T00:00:00Z","r":"a","c":1,"cm":1,"m":1,"mm":1}`
	if err := os.WriteFile(path, []byte("{broken\n"+line+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := NewMetricStore(path)
	if got := s.Read(time.Time{}); len(got) != 1 {
		t.Errorf("damaged line: %+v", got)
	}
	s.prune(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "broken") {
		t.Error("pruning should drop damaged lines")
	}
	s.prune(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))
	NewMetricStore(filepath.Join(dir, "gone")).prune(time.Now())
}
