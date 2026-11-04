package docsconsistency

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/runner"
)

// Troubleshooting's first step in the docs: docker compose logs runner-manager | grep '\[preflight'
var grepLogsRe = regexp.MustCompile(`logs runner-manager \| grep '([^']*)'`)

// TestPreflightMarker_DocsGrepWhatTheCodeActuallyLogs: the keyword the docs tell people to grep for
// must really be the line-start marker the code prints.
//
// The troubleshooting section opens with this command in all six languages. It is the concrete form of "check the startup preflight first when something is wrong."
// Nothing at compile time links the keyword to the marker: if runner.PreflightLogMarker changes and the docs don't,
// the first troubleshooting step in all six docs silently becomes "grep for a word that doesn't exist." The command still exits 0,
// but returns nothing at all, so readers conclude "preflight reported no problems."
//
// This check is itself half of item 1.2: it swaps the marker from "[self-check …]" to ASCII
// [preflight so readers of English, French, German, Japanese and Korean can type it too. It is imported rather than read from a file,
// so that deleting the constant breaks compilation here.
func TestPreflightMarker_DocsGrepWhatTheCodeActuallyLogs(t *testing.T) {
	repo := repoRoot(t)
	marker := runner.PreflightLogMarker

	checked := 0
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repo, path)
		for _, m := range grepLogsRe.FindAllStringSubmatch(string(b), -1) {
			checked++
			// The docs contain grep regexes, so [ must be escaped there; it is unescaped to a literal before comparison.
			pattern := strings.ReplaceAll(m[1], `\[`, "[")
			if !strings.HasPrefix(marker, pattern) {
				t.Errorf("%s tells people to grep for %q, but the preflight log marker is %q",
					filepath.ToSlash(rel), m[1], marker)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// If nothing matches, the command was probably rewritten and this check wasn't updated to follow,
	// in which case it would always pass and no longer verify what it was written to verify.
	if checked == 0 {
		t.Fatalf("no troubleshooting command of the form %q was found in any doc; "+
			"if the command is rewritten, grepLogsRe must be updated too", grepLogsRe)
	}
}
