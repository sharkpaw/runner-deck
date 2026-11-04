package docsconsistency

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var yamlTagRe = regexp.MustCompile(`yaml:"([a-z][a-z0-9_]*)`)

// TestConfigFields_EveryYAMLFieldIsInTheConfigTable checks that every yaml field in config.go is in the config table.
//
// During the audit, this table was missing six fields: runners.docker_gid, plus items[] name / path /
// target_type / target / labels. As a result, someone who wants to write config.yaml by hand can only read
// config.yaml.example (Chinese only) or the struct tags in config.go, and that is the actual difference
// between "documented" and "undocumented".
//
// It compares whether field names appear, not descriptions: descriptions should be translated, field names should not.
func TestConfigFields_EveryYAMLFieldIsInTheConfigTable(t *testing.T) {
	repo := repoRoot(t)
	conf := loadDocsConf(t, repo)

	src, err := os.ReadFile(filepath.Join(repo, "internal", "config", "config.go"))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]bool{}
	for _, m := range yamlTagRe.FindAllStringSubmatch(string(src), -1) {
		fields[m[1]] = true
	}
	if len(fields) < 15 {
		// If the regex is broken, it silently finds nothing, and this test case would always pass.
		t.Fatalf("only found %d yaml fields; yamlTagRe may no longer match", len(fields))
	}
	// The two top-level containers are not "fields" themselves, and the table has no separate row for them.
	delete(fields, "server")
	delete(fields, "runners")

	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)

	files := []string{filepath.Join(conf.root, "guide.md")}
	for _, lang := range conf.langs {
		files = append(files, filepath.Join(conf.root, lang, "guide.md"))
	}
	for _, f := range files {
		b, errRead := os.ReadFile(filepath.Join(repo, f))
		if errRead != nil {
			t.Fatal(errRead)
		}
		text := string(b)
		for _, name := range names {
			// The reason for boundary matching is the same as mentionsEnvVar: target is a prefix of target_type,
			// If only substring matching were used, a missing target row would go undetected.
			if !mentionsConfigField(text, name) {
				t.Errorf("%s does not mention config field %q; the config table is missing this row", filepath.ToSlash(f), name)
			}
		}
	}
}

func mentionsConfigField(text, name string) bool {
	isNameChar := func(b byte) bool {
		return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
	}
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		s := i + j
		e := s + len(name)
		if (s == 0 || !isNameChar(text[s-1])) && (e == len(text) || !isNameChar(text[e])) {
			return true
		}
		i = s + 1
	}
}
