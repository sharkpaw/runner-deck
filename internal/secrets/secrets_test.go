package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// legacyPath / storePath: the paths of the two locations, used repeatedly in the tests
func legacyPath(installDir string) string { return filepath.Join(installDir, LegacyRunnerTokenFile) }

func writeLegacy(t *testing.T, installDir, token string) {
	t.Helper()
	if err := os.MkdirAll(installDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath(installDir), []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeStore(t *testing.T, s *Store, name, token string) {
	t.Helper()
	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		t.Fatal(err)
	}
	p, err := s.Path(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(token+"\n"), fileMode); err != nil {
		t.Fatal(err)
	}
}

func TestNewStore_DirIsBesideTheConfigFile(t *testing.T) {
	for _, tc := range []struct{ configPath, want string }{
		{"config/config.yaml", filepath.Join("config", "tokens")},
		{"/app/config/config.yaml", filepath.Join("/app/config", "tokens")},
		{"config.yaml", "tokens"},
	} {
		if got := NewStore(tc.configPath).Dir; got != tc.want {
			t.Errorf("NewStore(%q).Dir = %q, expected %q", tc.configPath, got, tc.want)
		}
	}
}

// The new location must sit under the config directory, which is not mounted into the Runner container.
// This is the whole reason this package exists. Pin it so nobody later changes Dir back to base_path.
func TestNewStore_DirIsNotUnderTheRunnerBasePath(t *testing.T) {
	store := NewStore("/app/config/config.yaml")
	const basePath = "/app/runners"
	rel, err := filepath.Rel(basePath, store.Dir)
	if err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("credential directory %s is under runner directory %s; container mode would mount it into the container", store.Dir, basePath)
	}
}

// TestMigrate covers 4 combinations. Each case asserts whether the old file still exists.
// If files are only copied and not deleted, the old files stay in the mounted directory and the hole is not closed, which is the point of this table.
func TestMigrate(t *testing.T) {
	cases := []struct {
		name         string
		legacy       string // Empty means the old location has no files
		existing     string // Empty means the new location has no files
		wantMigrated bool
		wantToken    string // The contents the new location should have after migration
	}{
		{name: "旧无新无：什么也不做"},
		{name: "旧无新有：什么也不做", existing: "new-pat", wantToken: "new-pat"},
		{name: "旧有新无：搬过去并删掉旧的", legacy: "old-pat", wantMigrated: true, wantToken: "old-pat"},
		{name: "旧有新有内容相同：删掉旧的", legacy: "same-pat", existing: "same-pat", wantToken: "same-pat"},
		{name: "旧有新有内容不同：以新为准，删掉旧的", legacy: "old-pat", existing: "new-pat", wantToken: "new-pat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			installDir := filepath.Join(root, "runners", "alpha")
			if err := os.MkdirAll(installDir, 0700); err != nil {
				t.Fatal(err)
			}
			store := &Store{Dir: filepath.Join(root, "config", "tokens")}
			if tc.legacy != "" {
				writeLegacy(t, installDir, tc.legacy)
			}
			if tc.existing != "" {
				writeStore(t, store, "alpha", tc.existing)
			}

			migrated, err := store.Migrate("alpha", installDir)
			if err != nil {
				t.Fatalf("Migrate error: %v", err)
			}
			if migrated != tc.wantMigrated {
				t.Errorf("migrated = %v, expected %v", migrated, tc.wantMigrated)
			}
			if got := store.GitHubToken("alpha"); got != tc.wantToken {
				t.Errorf("GitHubToken = %q, expected %q", got, tc.wantToken)
			}
			// If the old location had files, none may remain after migration: it sits in the directory mounted into the container
			if tc.legacy != "" {
				if _, err := os.Stat(legacyPath(installDir)); !os.IsNotExist(err) {
					t.Errorf("old location %s still exists (err=%v); the job can still read it", legacyPath(installDir), err)
				}
			}
		})
	}
}

// Called every check cycle, so it must be idempotent: a second call should neither error nor change the result
func TestMigrate_IsIdempotent(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "runners", "alpha")
	store := &Store{Dir: filepath.Join(root, "config", "tokens")}
	writeLegacy(t, installDir, "old-pat")

	if migrated, err := store.Migrate("alpha", installDir); err != nil || !migrated {
		t.Fatalf("first migration should succeed: migrated=%v err=%v", migrated, err)
	}
	migrated, err := store.Migrate("alpha", installDir)
	if err != nil {
		t.Fatalf("second migration should not error: %v", err)
	}
	if migrated {
		t.Error("migrated should be false when there is nothing to move")
	}
	if got := store.GitHubToken("alpha"); got != "old-pat" {
		t.Errorf("token should remain old-pat, got %q", got)
	}
}

// Empty file has no credentials: nothing to move, and the user's file is not deleted
func TestMigrate_EmptyLegacyFileIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "runners", "alpha")
	store := &Store{Dir: filepath.Join(root, "config", "tokens")}
	writeLegacy(t, installDir, "   ")

	migrated, err := store.Migrate("alpha", installDir)
	if err != nil {
		t.Fatalf("Migrate error: %v", err)
	}
	if migrated {
		t.Error("An empty file should not count as a migration")
	}
	if _, err := os.Stat(legacyPath(installDir)); err != nil {
		t.Errorf("Empty file has no credentials and should be left as is: %v", err)
	}
	if got := store.GitHubToken("alpha"); got != "" {
		t.Errorf("New location should not be written, got %q", got)
	}
}

// The legacy file must survive a failed write: delete-then-write would lose the user's only PAT.
//
// Pick the failure trigger carefully. A read-only directory won't work: tests often run as root, and root can still write there,
// so the assertion would pass by luck. Making Dir's parent path a regular file won't work either: then
// os.ReadFile(new location) also fails with ENOTDIR, and Migrate returns on the "cannot read new location" path,
// so it never reaches the write (that path has its own test, TestMigrate_UnreadableNewLocationKeepsTheLegacyFile).
//
// Use a symlink pointing to a nonexistent target: reading the new location gets ENOENT (so it proceeds to the write),
// while MkdirAll fails on this name. Both conditions hold at once, independent of UID.
func TestMigrate_WriteFailureKeepsTheLegacyFile(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "runners", "alpha")
	writeLegacy(t, installDir, "old-pat")

	dir := filepath.Join(root, "tokens")
	if err := os.Symlink(filepath.Join(root, "nonexistent", "target"), dir); err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: dir}
	// Pin the precondition: the new location must truly not exist, otherwise this case tests a different path than the write failure
	newPath, err := store.Path("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(newPath); !os.IsNotExist(err) {
		t.Fatalf("Precondition not met: reading the new location should give \"not exist\", got %v", err)
	}

	migrated, err := store.Migrate("alpha", installDir)
	if err == nil {
		t.Fatal("Write cannot succeed, Migrate should return an error")
	}
	if migrated {
		t.Error("Migrated must be false when the write fails")
	}
	b, readErr := os.ReadFile(legacyPath(installDir))
	if readErr != nil {
		t.Fatalf("The old file must stay when the write fails, or the PAT is lost: %v", readErr)
	}
	if strings.TrimSpace(string(b)) != "old-pat" {
		t.Errorf("The old file content should be unchanged, got %q", b)
	}
}

// If the new location can't be read (not "missing" but a read failure), the old file must not be deleted either:
// Without knowing what's at the new location, we must not drop the only copy we know exists.
func TestMigrate_UnreadableNewLocationKeepsTheLegacyFile(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "runners", "alpha")
	writeLegacy(t, installDir, "old-pat")

	blocker := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: filepath.Join(blocker, "tokens")}

	migrated, err := store.Migrate("alpha", installDir)
	if err == nil {
		t.Fatal("Migrate should return an error when the new location can't be read")
	}
	if migrated {
		t.Error("migrated must be false when the move didn't happen")
	}
	if _, err := os.Stat(legacyPath(installDir)); err != nil {
		t.Fatalf("The old file must stay: %v", err)
	}
}

// Also test the read-only directory path, but the precondition doesn't hold under root, so skip it
func TestMigrate_WriteFailureOnReadOnlyDirKeepsTheLegacyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("Read-only directories are still writable when running as root, so this precondition doesn't hold")
	}
	root := t.TempDir()
	installDir := filepath.Join(root, "runners", "alpha")
	writeLegacy(t, installDir, "old-pat")

	parent := filepath.Join(root, "ro")
	if err := os.MkdirAll(parent, 0500); err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: filepath.Join(parent, "tokens")}

	if _, err := store.Migrate("alpha", installDir); err == nil {
		t.Fatal("The write can't succeed, so Migrate should return an error")
	}
	if _, err := os.Stat(legacyPath(installDir)); err != nil {
		t.Fatalf("The old file must stay when the write fails: %v", err)
	}
}

// Permissions are half of this: directory 0700, file 0600
func TestMigrate_Permissions(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "runners", "alpha")
	store := &Store{Dir: filepath.Join(root, "config", "tokens")}
	writeLegacy(t, installDir, "old-pat")

	if _, err := store.Migrate("alpha", installDir); err != nil {
		t.Fatalf("Migrate error: %v", err)
	}
	di, err := os.Stat(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != dirMode {
		t.Errorf("Credentials directory permissions are %04o, expected %04o", got, dirMode)
	}
	p, err := store.Path("alpha")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != fileMode {
		t.Errorf("Credentials file permissions are %04o, expected %04o", got, fileMode)
	}
}

// After migration, no temp files should remain: only the one named after the runner should be under tokens/
func TestMigrate_LeavesNoTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "runners", "alpha")
	store := &Store{Dir: filepath.Join(root, "config", "tokens")}
	writeLegacy(t, installDir, "old-pat")

	if _, err := store.Migrate("alpha", installDir); err != nil {
		t.Fatalf("Migrate error: %v", err)
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "alpha" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Only alpha should be under tokens/, got %v", names)
	}
}

func TestMigrate_NoInstallDirIsNotAnError(t *testing.T) {
	store := &Store{Dir: filepath.Join(t.TempDir(), "tokens")}
	if migrated, err := store.Migrate("alpha", ""); err != nil || migrated {
		t.Fatalf("An empty installDir should return silently: migrated=%v err=%v", migrated, err)
	}
}

func TestGitHubToken(t *testing.T) {
	root := t.TempDir()
	store := &Store{Dir: filepath.Join(root, "config", "tokens")}
	if got := store.GitHubToken("alpha"); got != "" {
		t.Errorf("With no credential, an empty string should be returned, got %q", got)
	}
	writeStore(t, store, "alpha", "  pat-with-space  ")
	if got := store.GitHubToken("alpha"); got != "pat-with-space" {
		t.Errorf("Leading and trailing whitespace should be trimmed, got %q", got)
	}
	if got := store.GitHubToken("beta"); got != "" {
		t.Errorf("Another runner shouldn't be able to read it, got %q", got)
	}
}

func TestRemove(t *testing.T) {
	root := t.TempDir()
	store := &Store{Dir: filepath.Join(root, "config", "tokens")}
	// Missing isn't an error. When a runner is removed, most of the time there's no PAT.
	if err := store.Remove("alpha"); err != nil {
		t.Fatalf("Removing a nonexistent credential should not error: %v", err)
	}
	writeStore(t, store, "alpha", "pat")
	writeStore(t, store, "beta", "other")
	if err := store.Remove("alpha"); err != nil {
		t.Fatalf("Remove error: %v", err)
	}
	if got := store.GitHubToken("alpha"); got != "" {
		t.Errorf("alpha's credential should be deleted, got %q", got)
	}
	if got := store.GitHubToken("beta"); got != "other" {
		t.Errorf("beta should not be touched, got %q", got)
	}
}

// The name is spliced into the path, so invalid names must be rejected up front rather than building an out-of-bounds path
func TestPath_RejectsUnsafeNames(t *testing.T) {
	store := &Store{Dir: filepath.Join(t.TempDir(), "tokens")}
	for _, name := range []string{"", "..", "../etc/passwd", "a/b", `a\b`, "  "} {
		if _, err := store.Path(name); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
		if got := store.GitHubToken(name); got != "" {
			t.Errorf("name %q should not read anything, got %q", name, got)
		}
		if err := store.Remove(name); err == nil {
			t.Errorf("Remove for name %q should return an error", name)
		}
	}
	// Valid names must pass, including ones starting with a dot. That's exactly what makes the base_path/.something/ approach unworkable.
	for _, name := range []string{"alpha", ".hidden", "a.b-c_d"} {
		if !config.IsSafeRunnerNameOrPath(name) {
			t.Fatalf("The precondition has changed: %q is now judged invalid by config", name)
		}
		if _, err := store.Path(name); err != nil {
			t.Errorf("name %q should be accepted: %v", name, err)
		}
	}
}

func TestMigrateAll(t *testing.T) {
	root := t.TempDir()
	basePath := filepath.Join(root, "runners")
	store := &Store{Dir: filepath.Join(root, "config", "tokens")}

	cfg := &config.Config{}
	cfg.Runners.BasePath = basePath
	cfg.Runners.Items = []config.RunnerItem{
		{Name: "alpha", TargetType: "repo", Target: "o/r"},
		{Name: "beta", TargetType: "repo", Target: "o/r"},
		{Name: "gamma", TargetType: "repo", Target: "o/r", Path: "gamma-dir"},
	}
	// alpha and gamma each have a PAT at the old location; beta has none
	writeLegacy(t, filepath.Join(basePath, "alpha"), "alpha-pat")
	writeLegacy(t, filepath.Join(basePath, "gamma-dir"), "gamma-pat")

	if moved := MigrateAll(cfg, store); moved != 2 {
		t.Errorf("Should have moved 2, got %d", moved)
	}
	if got := store.GitHubToken("alpha"); got != "alpha-pat" {
		t.Errorf("alpha's token is %q", got)
	}
	// Runners whose path and name differ also need handling: the old file is under path, the new one is addressed by name
	if got := store.GitHubToken("gamma"); got != "gamma-pat" {
		t.Errorf("gamma's token is %q", got)
	}
	if got := store.GitHubToken("beta"); got != "" {
		t.Errorf("beta never had a PAT, got %q", got)
	}
	for _, dir := range []string{"alpha", "gamma-dir"} {
		if _, err := os.Stat(legacyPath(filepath.Join(basePath, dir))); !os.IsNotExist(err) {
			t.Errorf("%s's legacy file still exists (err=%v)", dir, err)
		}
	}
	if moved := MigrateAll(nil, store); moved != 0 {
		t.Errorf("Should return 0 when cfg is nil, got %d", moved)
	}
	if moved := MigrateAll(cfg, nil); moved != 0 {
		t.Errorf("Should return 0 when store is nil, got %d", moved)
	}
}
