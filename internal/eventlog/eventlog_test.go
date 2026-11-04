package eventlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_AppendAndFilter(t *testing.T) {
	s := ForConfig(filepath.Join(t.TempDir(), "config.yaml"))
	s.Append(Event{Runner: "a", Level: LevelInfo, Code: "started"})
	s.Append(Event{Runner: "b", Level: LevelError, Code: "oom", Detail: "2"})
	s.Append(Event{Runner: "a", Level: LevelError, Code: "job_failed", Detail: "verify"})

	all := s.Read(Query{})
	if len(all) != 3 || all[0].Code != "job_failed" || all[2].Code != "started" {
		t.Fatalf("newest first expected: %+v", all)
	}
	if all[0].Time.IsZero() {
		t.Error("a missing time must be filled in")
	}
	if got := s.Read(Query{Runner: "a"}); len(got) != 2 {
		t.Errorf("runner filter: %+v", got)
	}
	if got := s.Read(Query{Level: LevelError}); len(got) != 2 {
		t.Errorf("level filter: %+v", got)
	}
	if got := s.Read(Query{Limit: 1}); len(got) != 1 || got[0].Code != "job_failed" {
		t.Errorf("limit: %+v", got)
	}
}

func TestStore_ReadSurvivesMissingAndDamagedFiles(t *testing.T) {
	dir := t.TempDir()
	if got := New(filepath.Join(dir, "missing")).Read(Query{}); got == nil || len(got) != 0 {
		t.Errorf("missing file: %#v", got)
	}
	var nilStore *Store
	nilStore.Append(Event{Code: "x"})
	if got := nilStore.Read(Query{}); len(got) != 0 {
		t.Errorf("nil store: %+v", got)
	}
	path := filepath.Join(dir, "e.jsonl")
	if err := os.WriteFile(path, []byte("{broken\n{\"code\":\"ok\",\"level\":\"info\"}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := New(path).Read(Query{}); len(got) != 1 || got[0].Code != "ok" {
		t.Errorf("damaged line: %+v", got)
	}
}

func TestStore_AppendFailureDoesNotPanic(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "no-such-dir", "e.jsonl"))
	s.Append(Event{Code: "x", Time: time.Now()})
	if got := s.Read(Query{}); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestStore_RotationKeepsNewestWholeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	s := &Store{path: path, maxBytes: 2000, keepBytes: 500}
	for i := 0; i < 100; i++ {
		s.Append(Event{Level: LevelInfo, Code: "c", Detail: strings.Repeat("x", 30) + string(rune('a'+i%26))})
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 2000 {
		t.Errorf("size = %d", info.Size())
	}
	got := s.Read(Query{})
	if len(got) == 0 || !strings.HasSuffix(got[0].Detail, string(rune('a'+99%26))) {
		t.Fatalf("newest event lost: %+v", got)
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), "\n") != len(got) {
		t.Errorf("a partial line survived rotation")
	}
}
