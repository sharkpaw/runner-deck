package eventlog

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sharkpaw/runner-deck/internal/atomicfile"
)

// MetricsFileName is the metrics file next to config.yaml.
const MetricsFileName = "metrics.jsonl"

// MetricsRetention is how long resource history is kept.
const MetricsRetention = 31 * 24 * time.Hour

// MetricPoint summarizes one runner's use over a short period.
type MetricPoint struct {
	Time   time.Time `json:"t"`
	Runner string    `json:"r"`
	CPUAvg float64   `json:"c"`
	CPUMax float64   `json:"cm"`
	MemAvg uint64    `json:"m"`
	MemMax uint64    `json:"mm"`
}

// MetricStore appends points to a JSON-lines file and drops those past the retention.
type MetricStore struct {
	mu         sync.Mutex
	path       string
	lastPruned time.Time
}

// NewMetricStore returns a store writing to path.
func NewMetricStore(path string) *MetricStore {
	return &MetricStore{path: path}
}

// MetricsForConfig returns the store for a config file path.
func MetricsForConfig(configPath string) *MetricStore {
	return NewMetricStore(filepath.Join(filepath.Dir(configPath), MetricsFileName))
}

// Append records points. A failure is only logged: history must never affect the runners.
func (s *MetricStore) Append(points []MetricPoint, now time.Time) {
	if s == nil || len(points) == 0 {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, p := range points {
		if err := enc.Encode(p); err != nil {
			log.Printf("warning: cannot encode a metric point: %v", err)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		log.Printf("warning: cannot open %s: %v", s.path, err)
		return
	}
	_, err = f.Write(buf.Bytes())
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		log.Printf("warning: cannot write %s: %v %v", s.path, err, closeErr)
		return
	}
	if now.Sub(s.lastPruned) >= 24*time.Hour {
		s.prune(now)
		s.lastPruned = now
	}
}

func (s *MetricStore) prune(now time.Time) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		log.Printf("warning: cannot prune %s: %v", s.path, err)
		return
	}
	cutoff := now.Add(-MetricsRetention)
	var keep bytes.Buffer
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var p MetricPoint
		if json.Unmarshal([]byte(line), &p) == nil && p.Time.After(cutoff) {
			keep.WriteString(line)
			keep.WriteByte('\n')
		}
	}
	if keep.Len() == len(data) {
		return
	}
	if err := atomicfile.WriteFile(s.path, keep.Bytes(), 0o640); err != nil {
		log.Printf("warning: cannot prune %s: %v", s.path, err)
	}
}

// Read returns points newer than since, oldest first. A damaged line is skipped.
func (s *MetricStore) Read(since time.Time) []MetricPoint {
	points := []MetricPoint{}
	if s == nil {
		return points
	}
	s.mu.Lock()
	data, err := os.ReadFile(s.path)
	s.mu.Unlock()
	if err != nil {
		return points
	}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var p MetricPoint
		if json.Unmarshal([]byte(line), &p) == nil && p.Time.After(since) {
			points = append(points, p)
		}
	}
	return points
}
