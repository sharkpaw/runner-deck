package atomicfile

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// swapRename swaps out the package-level rename and swaps it back when the test ends.
//
// Tests in this package never call t.Parallel: rename is a package-level variable, so running in parallel is a data race under -race,
// and this package exists precisely for concurrency safety, so it makes no sense for a test to trigger a race itself.
func swapRename(t *testing.T, fn func(oldpath, newpath string) error) {
	t.Helper()
	orig := rename
	rename = fn
	t.Cleanup(func() { rename = orig })
}

// captureLog takes over log's output and returns a function that reads the captured content.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf.String
}

// tempLeftovers returns the temporary files left behind in the directory.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWriteFile_NewFileUsesPerm creates the target with perm when it does not exist.
//
// The assertion checks exact equality, not 'no more than perm': the implementation explicitly chmods to perm, bypassing umask,
// so the result here is deterministic. This differs from os.WriteFile on purpose: config.yaml should be 0644
// and stay 0644 regardless of the container's umask. That is why it is worth pinning down.
func TestWriteFile_NewFileUsesPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, path); got != "hello" {
		t.Errorf("content %q, expected %q", got, "hello")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0644 {
		t.Errorf("mode %v, expected %v", fi.Mode().Perm(), os.FileMode(0644))
	}
}

// TestWriteFile_PreservesExistingMode keeps the target's existing permission bits when it already exists,
// matching os.WriteFile. Switching the write approach must not reset a user's chmod'ed file back to 0644.
func TestWriteFile_PreservesExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil { // Bypass umask to make sure the starting point really is 0600
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, path); got != "new" {
		t.Errorf("content %q, expected %q", got, "new")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("mode %v, expected to keep %v", fi.Mode().Perm(), os.FileMode(0600))
	}
}

// TestWriteFile_NoTempLeftovers checks that neither the success nor the failure path leaves temporary files behind.
//
// If they were left behind, config/ would accumulate a pile of .config.yaml.tmp-* files with every failed save,
// and this is the directory the docs tell people to back up.
func TestWriteFile_NoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := WriteFile(path, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if leftovers := tempLeftovers(t, dir); len(leftovers) != 0 {
		t.Errorf("temp files still present after successful write: %v", leftovers)
	}

	// A rename failure other than EBUSY/EXDEV does not fall back; the error must be returned to the caller as-is.
	injected := &os.LinkError{Op: "rename", Err: syscall.EIO}
	swapRename(t, func(string, string) error { return injected })
	err := WriteFile(path, []byte("boom"), 0644)
	if err == nil {
		t.Fatal("should return an error when rename fails")
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("error does not expose the underlying cause: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name which file: %v", err)
	}
	if leftovers := tempLeftovers(t, dir); len(leftovers) != 0 {
		t.Errorf("temp files still present after failed write: %v", leftovers)
	}
	if got := mustRead(t, path); got != "ok" {
		t.Errorf("target should keep old content after rename fails, got %q", got)
	}
}

// TestWriteFile_TargetUntouchedUntilRename deterministically proves atomicity:
// at the moment all data is written but rename has not yet run, reading the target still returns the old content.
//
// Mutation check: swap WriteFile's implementation back to os.WriteFile and this test turns red --
// that path never calls the injected rename, so called stays false.
func TestWriteFile_TargetUntouchedUntilRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	called := false
	var seen string
	swapRename(t, func(oldpath, newpath string) error {
		called = true
		// The temp file must be in the same directory as the target; otherwise rename is not atomic across filesystems (EXDEV),
		// and the whole package would be pointless. Putting it in /tmp is the easiest mistake to make.
		if filepath.Dir(oldpath) != filepath.Dir(newpath) {
			t.Errorf("temp file is in %s, target is in %s, different directories", filepath.Dir(oldpath), filepath.Dir(newpath))
		}
		b, err := os.ReadFile(newpath)
		if err != nil {
			t.Errorf("target should still exist before rename: %v", err)
		}
		seen = string(b)
		return os.Rename(oldpath, newpath)
	})

	if err := WriteFile(path, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("did not go through rename, so the replacement is not atomic")
	}
	if seen != "old" {
		t.Errorf("target was changed before rename, read %q, expected %q", seen, "old")
	}
	if got := mustRead(t, path); got != "new" {
		t.Errorf("content after rename %q, expected %q", got, "new")
	}
}

// TestWriteFile_FallsBackOnRenameRefused covers single-file bind mounts (EBUSY) and cross-filesystem (EXDEV)
// where rename cannot be used. In that case it falls back to writing in place, and warns only once per path.
//
// The fact that the warning is logged only once must itself be pinned down: config.yaml goes through here on every save,
// Per-attempt printing would flood the runner status polling log.
func TestWriteFile_FallsBackOnRenameRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EBUSY", syscall.EBUSY}, // Single-file bind mount
		{"EXDEV", syscall.EXDEV}, // Temp file is not on the same filesystem as the target
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			swapRename(t, func(string, string) error {
				return &os.LinkError{Op: "rename", Err: tc.err}
			})
			logged := captureLog(t)

			// Write twice: the warning should appear only once.
			for i, want := range []string{"first", "second"} {
				if err := WriteFile(path, []byte(want), 0644); err != nil {
					t.Fatalf("write #%d should fall back to an in-place write and succeed: %v", i+1, err)
				}
				if got := mustRead(t, path); got != want {
					t.Fatalf("content after write #%d is %q, expected %q", i+1, got, want)
				}
			}

			if n := strings.Count(logged(), "writing it in place instead"); n != 1 {
				t.Errorf("warning printed %d times, expected 1:\n%s", n, logged())
			}
			if !strings.Contains(logged(), path) {
				t.Errorf("warning does not name which file:\n%s", logged())
			}
			if leftovers := tempLeftovers(t, dir); len(leftovers) != 0 {
				t.Errorf("temp file still present after fallback: %v", leftovers)
			}
		})
	}
}

// TestWriteFile_FallsBackWhenDirNotWritable covers a deployment where only config.yaml was chowned and config/ was not:
// A temp file cannot be created in the same directory, so it falls back to an in-place write and the save still succeeds.
//
// This is the upgrade-compatibility guard: such deployments saved fine before, so switching the write method must not start producing errors.
func TestWriteFile_FallsBackWhenDirNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, so the 'directory not writable' case cannot be created")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	// t.TempDir cleanup must be able to delete the directory's contents, so the permissions have to be restored
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	logged := captureLog(t)
	if err := WriteFile(path, []byte("new"), 0644); err != nil {
		t.Fatalf("directory not writable should fall back to an in-place write: %v", err)
	}
	if got := mustRead(t, path); got != "new" {
		t.Errorf("content %q, expected %q", got, "new")
	}
	if n := strings.Count(logged(), "writing it in place instead"); n != 1 {
		t.Errorf("warning printed %d times, expected 1:\n%s", n, logged())
	}
}
