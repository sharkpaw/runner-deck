// Package eventlog keeps a small, durable history of what happened to each runner, so a failure
// can still be explained after the container that had it is gone.
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

// Levels an event can have.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event is one line of the history. Code selects the translated message in the interface and
// Detail fills it in, such as a job name or an error.
type Event struct {
	Time   time.Time `json:"time"`
	Runner string    `json:"runner,omitempty"`
	Level  string    `json:"level"`
	Code   string    `json:"code"`
	Detail string    `json:"detail,omitempty"`
}

// Store appends events to a JSON-lines file. Past maxBytes it keeps only the newest keepBytes.
type Store struct {
	mu        sync.Mutex
	path      string
	maxBytes  int64
	keepBytes int
}

// New returns a store writing to path.
func New(path string) *Store {
	return &Store{path: path, maxBytes: 5 << 20, keepBytes: 2 << 20}
}

// FileName is the store's file next to config.yaml.
const FileName = "events.jsonl"

// ForConfig returns the store for a config file path.
func ForConfig(configPath string) *Store {
	return New(filepath.Join(filepath.Dir(configPath), FileName))
}

// Append records an event. A failure is only logged: history must never break the operation
// it describes.
func (s *Store) Append(e Event) {
	if s == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		log.Printf("warning: cannot encode event: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		log.Printf("warning: cannot open %s: %v", s.path, err)
		return
	}
	_, err = f.Write(append(line, '\n'))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		log.Printf("warning: cannot write %s: %v %v", s.path, err, closeErr)
		return
	}
	s.rotate()
}

func (s *Store) rotate() {
	info, err := os.Stat(s.path)
	if err != nil || info.Size() <= s.maxBytes {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		log.Printf("warning: cannot rotate %s: %v", s.path, err)
		return
	}
	tail := data[len(data)-s.keepBytes:]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	if err := atomicfile.WriteFile(s.path, tail, 0o640); err != nil {
		log.Printf("warning: cannot rotate %s: %v", s.path, err)
	}
}

// Query narrows Read. Empty fields match everything.
type Query struct {
	Runner string
	Level  string
	Limit  int
}

// Read returns matching events, newest first. A damaged line is skipped.
func (s *Store) Read(q Query) []Event {
	events := []Event{}
	if s == nil {
		return events
	}
	s.mu.Lock()
	data, err := os.ReadFile(s.path)
	s.mu.Unlock()
	if err != nil {
		return events
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var e Event
		if json.Unmarshal([]byte(lines[i]), &e) != nil {
			continue
		}
		if (q.Runner != "" && e.Runner != q.Runner) || (q.Level != "" && e.Level != q.Level) {
			continue
		}
		events = append(events, e)
		if q.Limit > 0 && len(events) >= q.Limit {
			break
		}
	}
	return events
}
