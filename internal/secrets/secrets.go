// Package secrets manages credentials that belong only to the Manager (currently just the optional GitHub PAT).
//
// They cannot live under runners/<name>/: in container mode that whole directory is mounted into the runner container at
// /runner, and jobs run as the same UID (1001), so file permissions cannot stop them. A `chmod 600` file owned by
// that UID is still readable by the job itself. Anyone who can push a workflow to this repo could then run just
// `cat /runner/.github_check_token` and get the PAT. An org-target PAT carries
// admin:org, which amounts to organization admin rights.
//
// The new location is tokens/ in the directory containing the config file. It was chosen because it is mounted only into the Manager,
// never into runner containers, and it is already on the documented backup list.
//
// base_path/.something was rejected: runner names may start with a dot, so a runner named exactly that
// would make its install directory the secrets directory, and that directory gets mounted into the container.
package secrets

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sharkpaw/runner-deck/internal/config"
)

// LegacyRunnerTokenFile is the old PAT location: this file inside each runner's install directory.
//
// It is kept only for migration. Nothing outside this package should reference it. Read the PAT through Store only,
// as enforced by TestNoManagerSecretsUnderRunnerDirs.
const LegacyRunnerTokenFile = ".github_check_token"

// tokensDirName is the name of the subdirectory in the config directory that holds the credentials
const tokensDirName = "tokens"

// PathDescription is the generic form of the new location, used in messages where no concrete path is available.
// The actual location depends on -config. With the default compose mount, this is what it resolves to.
const PathDescription = "config/tokens/<runner name>"

// dirMode and fileMode are the permissions for the secrets directory and the secret files
const (
	dirMode  os.FileMode = 0700
	fileMode os.FileMode = 0600
)

// Store addresses the Manager-side credentials by runner name.
// Dir is <config file directory>/tokens. With the default compose mount this is /app/config/tokens.
type Store struct{ Dir string }

// NewStore derives the credentials directory from the -config path.
// Passing config/config.yaml yields config/tokens.
func NewStore(configPath string) *Store {
	return &Store{Dir: filepath.Join(filepath.Dir(configPath), tokensDirName)}
}

// Path returns the credential file path for a runner, or an error if the name is invalid.
//
// The name is validated again here rather than trusted from the caller. It is spliced into a path, and
// config.IsSafeRunnerNameOrPath exists to reject / \ and .. . Config loading already validates it,
// but a path-building site that does not validate for itself relies on every call chain never missing a check.
func (s *Store) Path(runnerName string) (string, error) {
	name := strings.TrimSpace(runnerName)
	if !config.IsSafeRunnerNameOrPath(name) {
		return "", fmt.Errorf("runner name %q cannot be used as a file name (.. / \\ are not allowed)", runnerName)
	}
	return filepath.Join(s.Dir, name), nil
}

// GitHubToken reads the PAT for this runner. An empty string means none is configured.
// This matches the old TokenForRunner: having no credential is not an error, since this tool does not require one.
func (s *Store) GitHubToken(runnerName string) string {
	p, err := s.Path(runnerName)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Migrate moves a PAT from the old location (inside the runner directory) to the new one and deletes the old file.
//
// migrated is true only when this call actually wrote a PAT to the new location. Deleting the old file, not just copying it,
// is the whole point: while the old file sits in the mounted directory, the exposure is not closed.
//
// It is called on every check cycle, so it must be cheap when there is nothing to do, and idempotent.
// Because it runs every cycle, a user who still follows the old docs and places the PAT in the runner directory
// will have it moved within one cycle at most.
//
// If writing the new location fails, the old file is kept and the error is returned to the caller. Deleting first and then writing would lose the user's PAT.
func (s *Store) Migrate(runnerName, installDir string) (migrated bool, err error) {
	if installDir == "" {
		return false, nil
	}
	legacy := filepath.Join(installDir, LegacyRunnerTokenFile)
	b, err := os.ReadFile(legacy)
	if err != nil {
		// A missing file is the normal case (most runners have no PAT) and is not an error.
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("cannot read the legacy PAT %s: %w", legacy, err)
	}
	old := strings.TrimSpace(string(b))
	if old == "" {
		// An empty file holds no credential, so there is nothing to move and no exposure to close.
		// It is not deleted: this tool did not write the file, and removing a user file that contains nothing gains nothing.
		return false, nil
	}

	newPath, err := s.Path(runnerName)
	if err != nil {
		return false, err
	}
	existing, readErr := os.ReadFile(newPath)
	switch {
	case readErr == nil && strings.TrimSpace(string(existing)) == old:
		// Both locations hold the same value: the new one already exists, so only the copy in the mounted directory needs removing.
		if err := os.Remove(legacy); err != nil {
			return false, fmt.Errorf("cannot remove the legacy PAT %s: %w", legacy, err)
		}
		return false, nil
	case readErr == nil:
		// Both locations hold different values: the new one wins. The old file is still an exposure and is deleted anyway,
		// but the message must say that the deleted value differed from the kept one, or the user will assume the value they just placed is in effect.
		if err := os.Remove(legacy); err != nil {
			return false, fmt.Errorf("cannot remove the legacy PAT %s: %w", legacy, err)
		}
		log.Printf("warning: runner %s had a PAT in both %s and its runner directory, and the two differ. "+
			"Kept the one in %s and removed the one in the runner directory, which container mode mounts into the runner container. "+
			"If the removed one was the current PAT, write it to %s", runnerName, newPath, newPath, newPath)
		return false, nil
	case !os.IsNotExist(readErr):
		return false, fmt.Errorf("cannot read the PAT %s: %w", newPath, readErr)
	}

	// The new location does not exist yet: write there first, and delete the old one only after the write succeeds
	if err := s.write(newPath, old); err != nil {
		return false, err
	}
	if err := os.Remove(legacy); err != nil {
		return false, fmt.Errorf("wrote the PAT to %s but cannot remove the legacy copy %s: %w", newPath, legacy, err)
	}
	log.Printf("moved the PAT for runner %s out of its runner directory to %s", runnerName, newPath)
	return true, nil
}

// Remove deletes the credentials for this Runner. It is called when a Runner is removed. A missing file is not an error.
func (s *Store) Remove(runnerName string) error {
	p, err := s.Path(runnerName)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot remove the PAT %s: %w", p, err)
	}
	return nil
}

// write atomically writes a credentials file with mode 0600.
//
// It writes a temp file in the same directory and then renames it. Writing directly to the final name could leave a
// partial PAT if interrupted midway, and callers would read a string that looks like a token but is not usable.
// os.CreateTemp already creates files with mode 0600, so the intermediate state is never more permissive than the final file.
func (s *Store) write(path, token string) error {
	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		return fmt.Errorf("cannot create the token directory %s: %w", s.Dir, err)
	}
	f, err := os.CreateTemp(s.Dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create a temporary file in %s: %w", s.Dir, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(append([]byte(token), '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	// CreateTemp already uses 0600. Set it again explicitly here so the mode does not depend on factors beyond the umask.
	if err := os.Chmod(tmp, fileMode); err != nil {
		return fmt.Errorf("cannot set the mode of %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("cannot publish the PAT to %s: %w", path, err)
	}
	return nil
}

// MigrateAll migrates each Runner in the config once and returns how many were moved.
// It is called at startup so that PATs at the old deployed path do not have to wait for the first check cycle.
func MigrateAll(cfg *config.Config, store *Store) int {
	if cfg == nil || store == nil {
		return 0
	}
	moved := 0
	for _, item := range cfg.Runners.Items {
		ok, err := store.Migrate(item.Name, item.InstallPath(cfg.Runners.BasePath))
		if err != nil {
			// A migration failure does not block startup: the PAT only affects the optional visibility check and deregistration on removal.
			// But it must still be reported: the old file is still in the mounted directory, and that is the exposure this package is meant to close.
			log.Printf("warning: cannot move the PAT for runner %s out of its runner directory: %v", item.Name, err)
			continue
		}
		if ok {
			moved++
		}
	}
	return moved
}
