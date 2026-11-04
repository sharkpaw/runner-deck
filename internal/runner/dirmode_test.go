package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// Runner install directory holds .credentials_rsaparams, which config.sh writes—the RSA private key the Runner uses to prove its identity to GitHub.
// actions/runner does not set permissions on these files, so the directory's permission bits are the last line of defense.
func TestEnsureRunnerDir_CreatesPrivateDirectory(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: base}}

	dir, err := EnsureRunnerDir(cfg, "alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("install directory permission is %o, other users can enter; credential files follow umask and become 0644", perm)
	}
	if info.Mode().Perm() != RunnerDirMode {
		t.Fatalf("install directory permission should be %o, got %o", RunnerDirMode, info.Mode().Perm())
	}
}

// base_path is not tightened: it is only a container with no credentials inside, and tightening it would affect host mounts
func TestEnsureRunnerDir_LeavesBasePathTraversable(t *testing.T) {
	base := filepath.Join(t.TempDir(), "runners")
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: base}}

	if _, err := EnsureRunnerDir(cfg, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("base_path should be created: %v", err)
	}
	if info.Mode().Perm()&0o055 == 0 {
		t.Fatalf("base_path permission is %o, it should not be tightened", info.Mode().Perm())
	}
}

// Existing directories are not modified: on deployments with mismatched UIDs, tightening them unilaterally would break what currently works,
// so the startup self-check should name them and leave the decision to a human
func TestEnsureRunnerDir_DoesNotChangeExistingDirectory(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "alpha")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: base}}

	if _, err := EnsureRunnerDir(cfg, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(existing)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing directory permission was changed to %o", info.Mode().Perm())
	}
}

// The self-check should name these directories and give a chmod that can be run as-is
func TestPreflight_ReportsWorldTraversableRunnerDirs(t *testing.T) {
	base := t.TempDir()
	loose := filepath.Join(base, "loose")
	tight := filepath.Join(base, "tight")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tight, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tight, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: base, Items: []config.RunnerItem{
		{Name: "loose", Path: "loose"},
		{Name: "tight", Path: "tight"},
	}}}

	got := checkRunnerDirPermissions(cfg)
	if got.Level != CheckWarn {
		t.Fatalf("directories others can enter should trigger a warning, got %+v", got)
	}
	if !strings.Contains(got.Message, loose) {
		t.Fatalf("should name %s, actual message %q", loose, got.Message)
	}
	if strings.Contains(got.Message, tight) {
		t.Fatalf("a 0700 directory should not be named, actual message %q", got.Message)
	}
	if got.Hint != "chmod 700 "+loose {
		t.Fatalf("the fix suggestion should be a directly runnable chmod, got %q", got.Hint)
	}
}

func TestPreflight_QuietWhenAllRunnerDirsArePrivate(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: base,
		Items: []config.RunnerItem{{Name: "a", Path: "a"}}}}
	if got := checkRunnerDirPermissions(cfg); got.Level != CheckOK {
		t.Fatalf("no warning should fire when all are 0700, got %+v", got)
	}
}

// Directories not yet created are outside this item's scope; do not report "not installed" as a permission problem
func TestPreflight_IgnoresMissingRunnerDirs(t *testing.T) {
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: t.TempDir(),
		Items: []config.RunnerItem{{Name: "never-created", Path: "never-created"}}}}
	if got := checkRunnerDirPermissions(cfg); got.Level != CheckOK {
		t.Fatalf("no warning should fire when the directory does not exist, got %+v", got)
	}
}
