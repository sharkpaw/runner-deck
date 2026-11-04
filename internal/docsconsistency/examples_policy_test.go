package docsconsistency

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// docLang is the language policy for a document.
type docLang int

const (
	// langZhOnly is Chinese only, deliberately without a translation.
	langZhOnly docLang = iota
	// langTranslated is English as the original, with a translation for every language in docs_languages.
	langTranslated
)

// examplesPolicy is the language policy for each README under examples/.
//
// This table is a record of decisions, not a copy of the list. The three docs under docs/ are watched by ci-recipes'
// structure check and this package's content check, but examples/ has never had anything watching it, and its two READMEs
// are the densest troubleshooting content in the repo (15 root-cause entries, versus 9 in the English guide.md),
// yet they are Chinese only, and the root README's English entry point sends people straight there.
//
// Chinese-only is a **decided** choice. Of the three options in section 7 of docs-improvement-plan.md, B was chosen:
// only the two .example templates were made bilingual (every user copies them), and examples/ stays Chinese.
// This table makes sure that adding an examples README requires a row here, so you have to reconsider
// whether that trade-off still holds for it.
var examplesPolicy = map[string]docLang{
	"examples/deploy/README.md":        langZhOnly,
	"examples/runner-images/README.md": langZhOnly,
}

// TestExamplesPolicy_EveryReadmeHasADecision requires every README under examples/ to have a row in the policy table.
func TestExamplesPolicy_EveryReadmeHasADecision(t *testing.T) {
	repo := repoRoot(t)

	found := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(repo, "examples"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(d.Name(), "README.md") {
			return err
		}
		rel, _ := filepath.Rel(repo, path)
		rel = filepath.ToSlash(rel)
		found[rel] = true
		if _, ok := examplesPolicy[rel]; !ok {
			t.Errorf("%s has no language policy. Add a row in examplesPolicy,"+
				"and decide whether it needs a translation (docs/docs-improvement-plan.md section 7)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for rel := range examplesPolicy {
		if !found[rel] {
			t.Errorf("%s in examplesPolicy no longer exists, delete this row", rel)
		}
	}
}

// TestExamplesPolicy_DeclaredPolicyMatchesReality requires the declared policies to match the facts on disk.
//
// A table alone is just a memo. This check makes it enforceable: a README declared Chinese-only must really be Chinese, and must really have no
// translated copy; one declared as translated must have every one of its five languages. Changing the policy without touching the files, or touching the files
// without changing the policy, both directions fail.
func TestExamplesPolicy_DeclaredPolicyMatchesReality(t *testing.T) {
	repo := repoRoot(t)
	conf := loadDocsConf(t, repo)

	for rel, policy := range examplesPolicy {
		b, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			continue // The previous case already reports that it does not exist.
		}
		dir := filepath.Dir(rel)

		switch policy {
		case langZhOnly:
			if !hasHan(string(b)) {
				t.Errorf("%s is declared Chinese-only, but its content has no Chinese characters", rel)
			}
			for _, lang := range conf.langs {
				sib := filepath.Join(repo, dir, lang, "README.md")
				if _, err := os.Stat(sib); err == nil {
					t.Errorf("%s is declared Chinese-only, but %s/%s/README.md exists;"+
						"either change the policy to langTranslated, or delete that translation", rel, dir, lang)
				}
			}
		case langTranslated:
			if hasHan(string(b)) {
				t.Errorf("%s is declared as English original + translation, but its body contains Chinese characters", rel)
			}
			for _, lang := range conf.langs {
				sib := filepath.Join(repo, dir, lang, "README.md")
				if _, err := os.Stat(sib); err != nil {
					t.Errorf("%s is declared as English original + translation, but %s/%s/README.md is missing", rel, dir, lang)
				}
			}
		}
	}
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
