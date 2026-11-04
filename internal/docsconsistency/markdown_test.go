package docsconsistency

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is in internal/docsconsistency, so the test's working directory is the package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Use a file that only exists at the repo root; if the path is wrong, fail right away instead of silently checking nothing.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found (%s): %v", root, err)
	}
	return root
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	fenceRe   = regexp.MustCompile("^\\s*```(.*)$")
	bulletRe  = regexp.MustCompile(`^\s*([-*]|\d+\.)\s+`)
	linkRe    = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)
	codeRe    = regexp.MustCompile("`[^`]*`")
)

// section holds the stats for one heading and its body. The body runs up to the next heading.
type section struct {
	line     int // Line number of the heading, used only to locate errors
	tableRow int // Number of table rows (lines starting with |)
	blocks   []codeBlock
	bullets  int
	links    []string // Link target resolved to a repo-relative path; pure anchor links are not counted
}

type codeBlock struct {
	info  string // Language tag after ```
	lines int    // Number of non-comment, non-blank lines
}

// parseSections splits a document by heading and counts the content of each section.
//
// Lines inside fences are not recognized as anything. guide.md has a bash block containing `# edit config/config.yaml…`
// comment lines like this, which would count as headings and throw off the section count (the ci-recipes heading check also skips fences,
// so both sides must see the same split).
func parseSections(t *testing.T, path string) []section {
	t.Helper()
	lines := readLines(t, path)

	// Content from the start of the file up to the first heading goes into a sentinel section so indexes line up across all files.
	secs := []section{{line: 0}}
	inFence := false
	var fenceInfo string
	var fenceCount int

	for i, raw := range lines {
		if m := fenceRe.FindStringSubmatch(raw); m != nil {
			if inFence {
				cur := &secs[len(secs)-1]
				cur.blocks = append(cur.blocks, codeBlock{info: fenceInfo, lines: fenceCount})
				inFence = false
			} else {
				inFence = true
				fenceInfo = strings.TrimSpace(m[1])
				fenceCount = 0
			}
			continue
		}
		if inFence {
			s := strings.TrimSpace(raw)
			// Comment lines are translated per language and not compared; blank lines likewise.
			if s != "" && !strings.HasPrefix(s, "#") && !strings.HasPrefix(s, "//") {
				fenceCount++
			}
			continue
		}

		if headingRe.MatchString(raw) {
			secs = append(secs, section{line: i + 1})
			continue
		}

		cur := &secs[len(secs)-1]
		if strings.HasPrefix(strings.TrimSpace(raw), "|") {
			cur.tableRow++
		}
		if bulletRe.MatchString(raw) {
			cur.bullets++
		}
		cur.links = append(cur.links, docLinks(path, raw)...)
	}
	return secs
}

// docLinks extracts the Markdown links from a line and resolves them to repo-relative paths.
//
// Parsing instead of comparing raw literals, because the same target is necessarily written differently in English and translations:
// docs/guide.md writes ../examples/, docs/zh/guide.md writes ../../examples/,
// and both point to the same place. Pure anchor links (#4-security…) are expected to differ between the two, so skip them.
func docLinks(path, raw string) []string {
	// The language nav line at the top is naturally different on each side (the current language is not a link), so don't compare it.
	if strings.HasPrefix(strings.TrimSpace(raw), "**文档") {
		return nil
	}
	// [x](y) inside inline code is displayed text, not a link.
	line := codeRe.ReplaceAllString(raw, "")

	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
		href := strings.TrimSpace(m[2])
		if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") ||
			strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "#") {
			continue
		}
		target := href
		if i := strings.Index(target, "#"); i >= 0 {
			target = target[:i]
		}
		if target == "" {
			continue
		}
		out = append(out, filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(path), target))))
	}
	return out
}
