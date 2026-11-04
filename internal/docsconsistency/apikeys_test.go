package docsconsistency

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Key prefixes to check. api.* is passed directly to tr/trf by the handler; github.dereg.* is passed by githubcheck as
// MessageKey literals returned, then rendered by the handler. The latter cannot see the key name at the call site,
// so scan by literal, not by the call shape of tr/trf.
var i18nKeyRe = regexp.MustCompile(`"((?:api|github\.dereg)\.[a-z0-9_.]+)"`)

// TestAPIKeys_EveryKeyTheHandlerUsesExistsInAllLanguages
// Every api.* key used in the handler must exist in all six i18n JSON files.
//
// Server messages were previously hardcoded in Chinese: when the UI switched to English, the shell was English but every toast was still Chinese.
// They now go through tr/trf lookups, and when a lookup misses, it falls back to the **key itself**, so the UI shows "api.start_failed"
// That is easy to spot as a defect, but only if someone sees it. If a new tr call is added without its key, it is invisible to the eye:
// that path may go months without anyone reaching it.
//
// Also check the reverse: api.* keys present in the JSON but unused in code are left over from deleted code, and keeping them only leads
// the next person to think they are still in use.
func TestAPIKeys_EveryKeyTheHandlerUsesExistsInAllLanguages(t *testing.T) {
	repo := repoRoot(t)
	conf := loadDocsConf(t, repo)

	used := map[string]string{} // key -> first occurrence location
	// cmd must be scanned too: the CSRF guard intercepts before routing, and the code lives in cmd/runner-manager, but the 403 it returns
	// reaches the UI through the same path as handler errors. If only internal is scanned, those two keys would be checked in reverse
	// and flagged as unused, even though they are the easiest to miss precisely because they are not in the handler.
	for _, root := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repo, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
				return err
			}
			if strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			b, errRead := os.ReadFile(path)
			if errRead != nil {
				return errRead
			}
			rel, _ := filepath.Rel(repo, path)
			for _, m := range i18nKeyRe.FindAllStringSubmatch(string(b), -1) {
				if _, seen := used[m[1]]; !seen {
					used[m[1]] = filepath.ToSlash(rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(used) < 20 {
		// If the regex is broken, it silently scans nothing, and this test case would always pass.
		t.Fatalf("Only found %d api.* keys; i18nKeyRe may no longer match", len(used))
	}

	langs := append([]string{"en"}, conf.langs...)
	inJSON := map[string]map[string]bool{}
	for _, lang := range langs {
		p := filepath.Join(repo, "cmd", "runner-manager", "i18n", lang+".json")
		b, errRead := os.ReadFile(p)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var m map[string]string
		if errJSON := json.Unmarshal(b, &m); errJSON != nil {
			t.Fatalf("%s.json parse failed: %v", lang, errJSON)
		}
		have := map[string]bool{}
		for k, v := range m {
			if isI18nKey(k) {
				have[k] = true
				if strings.TrimSpace(v) == "" {
					t.Errorf("%s.json has an empty %s; an empty value makes tr fall back, which is the same as no translation", lang, k)
				}
			}
		}
		inJSON[lang] = have
	}

	keys := make([]string, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, lang := range langs {
		for _, k := range keys {
			if !inJSON[lang][k] {
				t.Errorf("%s.json is missing %s (%s is in use)", lang, k, used[k])
			}
		}
		for k := range inJSON[lang] {
			if _, ok := used[k]; !ok {
				t.Errorf("%s.json has %s, which no code uses anymore; delete it", lang, k)
			}
		}
	}
}

// isI18nKey stays consistent with the prefixes recognized by i18nKeyRe.
func isI18nKey(k string) bool {
	return strings.HasPrefix(k, "api.") || strings.HasPrefix(k, "github.dereg.")
}
