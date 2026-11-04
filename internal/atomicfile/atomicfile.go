// Package atomicfile replaces a file by writing a temp file in the same directory, fsync, then rename,
// so concurrent readers only ever see either the old or the new content, and a process killed midway leaves no half-written file.
//
// It replaces os.WriteFile, which runs O_TRUNC, write, close in that order, so two things can happen:
// a reader sees an empty or partial file in between (if config.yaml is read empty, yaml.Unmarshal reports no error,
// and the UI briefly shows 'no Runners at all'), and if the process exits after truncation but before the write finishes,
// a broken file stays on disk permanently. rename is an atomic operation within one filesystem, and together they solve both:
// the target name either points to the old inode or the new inode, with no intermediate state.
//
// One behavior differs from os.WriteFile: ownership. rename attaches the newly created inode, owned by the Manager's UID,
// whereas in-place writes keep the original owner. In the documented deployment, config/ and runners/ are already chowned to 1001,
// so this makes no difference. In the rare case where the owner is another UID and the directory is writable by the Manager, the owner becomes the Manager after writing.
//
// The other difference is the permissions of a newly created file: this package explicitly chmods to perm, bypassing umask, whereas os.WriteFile's perm goes through umask.
// This is intentional: config.yaml should stay 0644 when it is 0644, and should not change with the container's umask.
//
// EnsureAgentToken in internal/runner/agenttoken.go does not use this: it needs 'only the first writer wins'
// (os.Link does not overwrite an existing name), whereas here we need 'the last writer overwrites', which is the opposite semantics.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// rename is injected for tests. The production path is always os.Rename.
var rename = os.Rename

// WriteFile writes data to path, aligning with os.WriteFile semantics:
//   - if the target exists, keep its permission bits (os.WriteFile does not change permissions of an existing file, and neither does this);
//   - if the target does not exist, use perm.
//
// When atomic replacement is unavailable in these two environments, fall back to in-place writing (the old behavior) and warn once per path,
// rather than failing a save that should succeed; an upgrade should not stop a deployment that is already running:
//   - the directory is not writable but the file is, e.g. only config.yaml was chowned, not config/,
//     so a temp file cannot be created in the same directory;
//   - the target is a single-file bind mount (-v ./config.yaml:/app/config/config.yaml),
//     in which case rename fails with EBUSY; EXDEV occurs when the temp file and target are on different filesystems.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	mode := perm
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	dir := filepath.Dir(path)
	// The temp file must be in the same directory as the target: rename is atomic only within one filesystem,
	// so placing it in /tmp would give EXDEV across filesystems, making this package pointless.
	// The name starts with a dot, so tools that list configuration files in a directory won't pick it up.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return writeInPlace(path, data, mode, err)
		}
		return fmt.Errorf("cannot create a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()

	if err := writeTemp(f, data, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	if err := rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV) {
			return writeInPlace(path, data, mode, err)
		}
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}

	syncDir(dir)
	return nil
}

// writeTemp writes all data to the temp file and closes it. On failure f is already closed; deleting the temp file is the caller's responsibility.
func writeTemp(f *os.File, data []byte, mode os.FileMode) error {
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot write the temporary file %s: %w", tmp, err)
	}
	// fsync before rename. In the reverse order, after a crash the name may be attached while the content is still in the page cache,
	// which is exactly the empty file this package is meant to eliminate, just produced somewhere else.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot flush the temporary file %s: %w", tmp, err)
	}
	// os.CreateTemp creates the file with 0600; change it to the target's intended permissions. Use f.Chmod rather than os.Chmod(tmp):
	// it changes the inode the fd points to and does not depend on the temp file's name still being valid.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot set the mode of the temporary file %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write the temporary file %s: %w", tmp, err)
	}
	return nil
}

// writeInPlace falls back to the old os.WriteFile behavior and warns once per path.
func writeInPlace(path string, data []byte, mode os.FileMode, cause error) error {
	warnOnce(path, cause)
	return os.WriteFile(path, data, mode)
}

// warnedPaths warns only once per path: every save of config.yaml goes through here,
// and printing each time would be spam; never printing would swallow the fact that 'saving on this machine is not crash-safe'.
var warnedPaths sync.Map // path -> *sync.Once

func warnOnce(path string, cause error) {
	v, _ := warnedPaths.LoadOrStore(path, &sync.Once{})
	v.(*sync.Once).Do(func() {
		log.Printf("warning: cannot replace %s atomically (%v); writing it in place instead. "+
			"Make its directory writable by the Manager's user to get crash-safe saves", path, cause)
	})
}

// syncDir flushes the directory entry to disk on a best-effort basis. Without fsync on the directory after rename, after a crash
// the new inode's content may be on disk while the name pointing to it is not; the file reverts to the old content,
// without corruption, so a failure here only means one fewer layer of protection and is not worth returning an error for an already successful replacement.
// Opening the directory read-only and fsyncing it is also not supported by every filesystem, so this error is ignored too.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	_ = d.Sync()
}
