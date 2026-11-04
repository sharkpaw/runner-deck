package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type fakeOps struct {
	calls  []string
	chowns []string
	groups []int
	fail   string
}

func (f *fakeOps) ops(euid int) privilegeOps {
	step := func(name string) error {
		f.calls = append(f.calls, name)
		if f.fail == name {
			return errors.New("refused")
		}
		return nil
	}
	return privilegeOps{
		geteuid: func() int { return euid },
		lchown: func(path string, _, _ int) error {
			f.chowns = append(f.chowns, path)
			return step("lchown")
		},
		setgroups: func(g []int) error { f.groups = g; return step("setgroups") },
		setgid:    func(int) error { return step("setgid") },
		setuid:    func(int) error { return step("setuid") },
		setenv:    func(string, string) error { return step("setenv") },
	}
}

func TestDropPrivileges_NonRootChangesNothing(t *testing.T) {
	f := &fakeOps{}
	if err := dropPrivileges(f.ops(1001), "/missing", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestDropPrivileges_ChownsThenDropsInOrder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "docker.sock")
	if err := os.WriteFile(sock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeOps{}
	if err := dropPrivileges(f.ops(0), sock, dir, filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
	if len(f.chowns) != 3 {
		t.Errorf("chowns = %v, want the directory and its two files", f.chowns)
	}
	got := f.calls[len(f.calls)-4:]
	if want := []string{"setgroups", "setgid", "setuid", "setenv"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	_, gid, _ := ownerOf(sock)
	if gid != 0 && gid != appGID && !reflect.DeepEqual(f.groups, []int{appGID, gid}) {
		t.Errorf("groups = %v, want the socket's group %d too", f.groups, gid)
	}
}

func TestDropPrivileges_StopsOnFailure(t *testing.T) {
	for _, step := range []string{"lchown", "setgroups", "setgid", "setuid", "setenv"} {
		f := &fakeOps{fail: step}
		if err := dropPrivileges(f.ops(0), "/missing", t.TempDir()); err == nil {
			t.Errorf("%s failed but dropPrivileges returned nil", step)
		}
		if step != "setuid" && step != "setenv" {
			for _, c := range f.calls {
				if c == "setuid" {
					t.Errorf("%s failed but the uid still changed", step)
				}
			}
		}
	}
}

func TestChownTree_SkipsMissingPaths(t *testing.T) {
	f := &fakeOps{}
	if err := chownTree(f.ops(0), filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	if len(f.chowns) != 0 {
		t.Fatalf("chowns = %v", f.chowns)
	}
}

func TestManagerDirs(t *testing.T) {
	t.Setenv("RUNNERS_BASE_PATH", "")
	if got := managerDirs("/app/config/config.yaml"); !reflect.DeepEqual(got, []string{"/app/config", "/app/runners"}) {
		t.Errorf("default = %v", got)
	}
	t.Setenv("RUNNERS_BASE_PATH", "/data")
	if got := managerDirs("c/config.yaml"); !reflect.DeepEqual(got, []string{"c", "/data"}) {
		t.Errorf("override = %v", got)
	}
}
