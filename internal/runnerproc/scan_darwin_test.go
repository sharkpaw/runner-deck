//go:build darwin

package runnerproc

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	procfind "github.com/soulteary/procfind-kit"
)

func TestParseProcArgs(t *testing.T) {
	raw := binary.LittleEndian.AppendUint32(nil, 2)
	raw = append(raw, "/bin/bash\x00\x00\x00/bin/bash\x00/my runners/a/run.sh\x00SECRET=1\x00"...)
	want := []string{"/bin/bash", "/my runners/a/run.sh"}
	if got := parseProcArgs(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if got := parseProcArgs([]byte{1}); got != nil {
		t.Fatalf("short buffer: %q", got)
	}
}

func TestMatchProcesses_ScriptsFirstAndNoMentions(t *testing.T) {
	spec := procfind.Spec{Scripts: []string{"run.sh"}, Executables: []string{"bin/Runner.Listener"}}
	table := map[int][]string{
		5: {"/r/a/bin/Runner.Listener", "run"},
		9: {"/bin/bash", "/r/a/run.sh"},
		7: {"/usr/bin/tail", "/r/a/run.sh"},
		8: {"/bin/bash", "/r/b/run.sh"},
	}
	got := matchProcesses(table, []string{"/r/a", "/r/none"}, spec)
	if !reflect.DeepEqual(got["/r/a"], []int{9, 5}) || got["/r/none"] != nil {
		t.Fatalf("got %v", got)
	}
}

func TestSysctlScanner_FindsARealScript(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for !Running(dir) {
		if time.Now().After(deadline) {
			t.Fatal("running script not found")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := Find(dir); len(got) != 1 || got[0] != cmd.Process.Pid {
		t.Fatalf("got %v, want %d", got, cmd.Process.Pid)
	}
}
