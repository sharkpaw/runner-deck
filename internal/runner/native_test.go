package runner

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStampWriter_StampsWholeLines(t *testing.T) {
	var out bytes.Buffer
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	w := &stampWriter{out: &out, now: func() time.Time { return at }}
	_, _ = w.Write([]byte("Running jo"))
	if out.Len() != 0 {
		t.Fatalf("partial line written: %q", out.String())
	}
	_, _ = w.Write([]byte("b: build\nnext\n"))
	want := "2026-11-05T09:00:00Z Running job: build\n2026-11-05T09:00:00Z next\n"
	if out.String() != want {
		t.Fatalf("got %q", out.String())
	}
}

func TestReadLog_TailStartsAtALine(t *testing.T) {
	dir := t.TempDir()
	if got, err := ReadLog(dir, 10); got != nil || err != nil {
		t.Fatalf("missing log: %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, LogFileName), []byte("first line\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLog(dir, 12)
	if err != nil || string(got) != "third\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestOpenLog_Rotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, LogFileName)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", logRotateBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() != 0 {
		t.Errorf("log was not rotated: %d", info.Size())
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Error(err)
	}
}

func TestCleanWork_KeepsToolCache(t *testing.T) {
	dir := t.TempDir()
	if err := CleanWork(dir); err != nil {
		t.Fatalf("no work directory: %v", err)
	}
	for _, p := range []string{"_work/_tool/node/x", "_work/repo/repo/a.txt"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := CleanWork(dir); err != nil && !errors.Is(err, ErrWorkBusy) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_work/repo")); !os.IsNotExist(err) {
		t.Errorf("work not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_work/_tool/node/x")); err != nil {
		t.Errorf("tool cache removed: %v", err)
	}
}

func TestStart_KeepsOutputInTheLog(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho Running job: build\n"
	if err := os.WriteFile(filepath.Join(dir, RunScriptName()), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Start(dir); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := ReadLog(dir, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "Z Running job: build\n") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log = %q", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOpenLog_ReportsAnUnwritableDirectory(t *testing.T) {
	if _, err := openLog(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("want an error")
	}
	if got, err := ReadLog(t.TempDir()+"\x00", 10); err == nil {
		t.Fatalf("want an error, got %q", got)
	}
}

func TestCleanWork_ReportsAnUnreadableWorkDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "_work"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CleanWork(dir); err == nil {
		t.Fatal("want an error")
	}
}
