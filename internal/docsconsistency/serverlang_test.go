package docsconsistency

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The language the server speaks is something the docs promise.
//
// The README and the six guides all say "the UI and messages come in six languages." The product so far only delivered the shell:
// the text users actually read after clicking a button (toasts, errors, self-check items, logs) all comes from handlers
// and Chinese string literals in the runner. Switch the UI to English and the shell is English, but every toast is Chinese.
//
// Both sides are now fixed: messages tied to a request look up the i18n table (api.* keys), and places with no request to rely on
// (logs, registration results written to disk by background tasks, config validation errors) are always English. This case guards
// against it sliding back. Adding a new log.Printf or fmt.Errorf line and writing Chinese in it is the easiest path,
// and it won't turn any test red, and no one will check it line by line in review.
//
// Use the AST rather than grep. Chinese in comments is fine (this repo's comments are already written in Chinese),
// only literals matter. go/parser naturally separates the two; grep cannot.

// cjkRe matches CJK ideographs, Japanese kana, and Hangul.
//
// Not only Han characters: ja/ko translations must not appear in Go literals either, and checking only Han characters would let
// a purely kana Japanese sentence pass through. Full-width punctuation gets its own paragraph, because historically it is exactly what slipped through
// (the full-width colon separator in app.js rendered as "Check failed: ..." on the English UI,
// with not a single Han character, the problem is exactly the same).
var cjkRe = regexp.MustCompile(`[\p{Han}\p{Hiragana}\p{Katakana}\p{Hangul}\x{3000}-\x{303F}\x{FF00}-\x{FFEF}]`)

// goRoots lists the directories to scan. cmd must not be missed: the CSRF guard intercepts before routing, and the code lives in cmd/runner-manager,
// but the 403 it returns reaches the UI through the same path as handler errors.
var goRoots = []string{"internal", "cmd"}

// TestServerLanguage_NoCJKInGoStringLiterals
// No CJK literals may appear again in non-test Go code.
func TestServerLanguage_NoCJKInGoStringLiterals(t *testing.T) {
	repo := repoRoot(t)

	scanned := 0
	for _, root := range goRoots {
		err := filepath.WalkDir(filepath.Join(repo, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
				return err
			}
			if strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, errParse := parser.ParseFile(fset, path, nil, 0)
			if errParse != nil {
				return errParse
			}
			rel, _ := filepath.Rel(repo, path)
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				scanned++
				v, errUnquote := strconv.Unquote(lit.Value)
				if errUnquote != nil {
					return true
				}
				if cjkRe.MatchString(v) {
					t.Errorf("%s:%d string literal contains CJK text; server messages must be English or use an i18n key: %s",
						filepath.ToSlash(rel), fset.Position(lit.Pos()).Line, lit.Value)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Walking the wrong directory, or breaking the skip rules, would let this case declare a pass without checking a single literal.
	if scanned < 500 {
		t.Fatalf("only scanned %d string literals; the walk probably no longer reaches the code", scanned)
	}
}

// uiAssetAllowed lists the CJK text allowed in UI assets: each language's self-name in the language picker.
//
// The Chinese self-name should not be translated as "Chinese" -
// the only form useful to them is the name written in their own script.
var uiAssetAllowed = []string{"中文", "日本語", "한국어"}

// commentRes strips the four comment kinds in order of appearance. It only removes text and never adds, so the worst case is a missed report, never a false positive.
var commentRes = []*regexp.Regexp{
	regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`),  // Go template comment
	regexp.MustCompile(`(?s)<!--.*?-->`),         // HTML comment
	regexp.MustCompile(`(?s)/\*.*?\*/`),          // CSS / JS block comment
	regexp.MustCompile(`(?m)(^|[\s;{},)])//.*$`), // JS line comment
}

// TestServerLanguage_NoCJKInUIAssets
// Outside comments, templates and frontend assets may not contain CJK text.
//
// UI copy should always go through a t('key') lookup; a hardcoded Chinese label is a clear defect in the six-language UI.
// This case has actually caught something: the full-width colon separator in app.js was hardcoded as a separator,
// rendering as "Check failed: token expired" on the English UI - not a single Han character,
// but it was Chinese typography leaking into a Latin sentence.
func TestServerLanguage_NoCJKInUIAssets(t *testing.T) {
	repo := repoRoot(t)
	assets := []string{
		"cmd/runner-manager/templates/index.html",
		"cmd/runner-manager/static/app.js",
		"cmd/runner-manager/static/app.css",
	}
	for _, rel := range assets {
		b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, re := range commentRes {
			src = re.ReplaceAllString(src, "$1")
		}
		for i, line := range strings.Split(src, "\n") {
			stripped := line
			for _, ok := range uiAssetAllowed {
				stripped = strings.ReplaceAll(stripped, ok, "")
			}
			if loc := cjkRe.FindStringIndex(stripped); loc != nil {
				t.Errorf("%s:%d CJK text %q outside comments; UI copy must go through t('key'): %s",
					rel, i+1, stripped[loc[0]:loc[1]], strings.TrimSpace(line))
			}
		}
	}
}

// This gives the unicode import a purpose: cjkRe uses class names like \\p{Han},
// It also pins down that the full-width colon really falls in the range we check, so a regex mistake does not go unnoticed.
func TestServerLanguage_CJKRegexpCoversFullWidthPunctuation(t *testing.T) {
	for _, r := range []rune{'：', '，', '、', '。', '（', 'あ', 'ア', '한', '中'} {
		if !cjkRe.MatchString(string(r)) {
			t.Errorf("cjkRe does not recognize %q (U+%04X)", r, r)
		}
	}
	for _, r := range []rune{':', 'a', 'é', 'ß', '-'} {
		if cjkRe.MatchString(string(r)) {
			t.Errorf("cjkRe rejected %q (U+%04X)", r, r)
		}
		if unicode.Is(unicode.Han, r) {
			t.Errorf("%q should not be a Han character", r)
		}
	}
}
