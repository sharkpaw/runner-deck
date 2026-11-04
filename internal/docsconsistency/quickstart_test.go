package docsconsistency

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestQuickStart_ReadmeMatchesTheGuide makes sure the quick start commands in the README and the usage guide match.
//
// This is the one deliberate technical duplication left after slimming the README: the 30-second start has to stay on the front page,
// while the full instructions live in the guide, so both need it. Keeping the duplicate means someone has to watch it, and in this repo
// that watching has already failed once: the same commands existed in two forms, and in one of them chown came before mkdir,
// so copying it always errored, while six translations and the README were wrong together and nothing turned red.
//
// Compare only the command lines, not comments: comments in the guide get translated, while in the README they stay in English.
// The CI job `Quick start runs` proves the commands actually work; this test proves the two copies are the same.
func TestQuickStart_ReadmeMatchesTheGuide(t *testing.T) {
	repo := repoRoot(t)

	readme := commandsInFirstBashBlock(t,
		filepath.Join(repo, "README.md"), regexp.MustCompile(`(?m)^## .*Quick start`))
	guide := commandsInFirstBashBlock(t,
		filepath.Join(repo, "docs", "guide.md"), regexp.MustCompile(`(?m)^### .*docker-compose`))

	if strings.Join(readme, "\n") != strings.Join(guide, "\n") {
		t.Errorf("README.md and docs/guide.md have different quick start commands.\nREADME:\n  %s\nguide:\n  %s",
			strings.Join(readme, "\n  "), strings.Join(guide, "\n  "))
	}
	// If neither side yields anything, the comparison above counts as equal, so this case would always pass.
	if len(guide) < 3 {
		t.Fatalf("Only %d commands were extracted from docs/guide.md; the regex used to locate them may no longer match", len(guide))
	}
}

// commandsInFirstBashBlock returns the command lines (comments and blank lines removed) from the first ```bash block after the heading.
func commandsInFirstBashBlock(t *testing.T, path string, heading *regexp.Regexp) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")

	i := 0
	for ; i < len(lines); i++ {
		if heading.MatchString(lines[i]) {
			break
		}
	}
	if i == len(lines) {
		t.Fatalf("no heading matching %s found in %s", filepath.Base(path), heading)
	}

	var out []string
	inBlock := false
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		switch {
		case !inBlock && line == "```bash":
			inBlock = true
		case inBlock && strings.HasPrefix(line, "```"):
			return out
		case inBlock && line != "" && !strings.HasPrefix(line, "#"):
			out = append(out, line)
		}
	}
	t.Fatalf("%s 里那个 ```bash 块没有闭合", filepath.Base(path))
	return nil
}
