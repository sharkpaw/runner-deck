package docsconsistency

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Direct call sites that read env vars: os.Getenv("X"), env.GetTrimmed("X", …), etc.
var envCallRe = regexp.MustCompile(`(?:os\.Getenv|env\.Get\w*)\(\s*"([A-Z][A-Z0-9_]*)"`)

// loopReadEnvVars holds the few variables whose names don't appear at a call site.
//
// In config.go the ports are read by looping over a slice (`for _, key := range []string{…}`),
// so the rule that "the later name wins" is written only once. The cost is that the regex above can't see them,
// so they are named here. The test below checks in reverse that they are still in that file.
// If the loop is ever removed, this reminds you to delete these two lines instead of guarding two variables that no longer exist.
var loopReadEnvVars = map[string]string{
	"MANAGER_PORT": "internal/config/config.go",
	"SERVER_PORT":  "internal/config/config.go",
}

// scriptEnvVars holds the variables read by scripts/install-runner.sh.
// It is not Go code, so the regex above doesn't match it, but to operators it is no different from the others.
var scriptEnvVars = map[string]string{
	"RUNNER_VERSION":         "scripts/install-runner.sh",
	"RUNNER_SHA256":          "scripts/install-runner.sh",
	"RUNNER_FORCE_REINSTALL": "scripts/install-runner.sh",
}

// TestEnvVars_EveryVariableTheCodeReadsIsDocumented: every env var the code reads must be findable in the usage guide.
//
// This was the largest gap in the audit: guide.md has a single sentence saying "some fields can be overridden via env vars... see .env.example",
// and that file is Chinese-only and was itself missing three variables. Two more (the alias RUNNER_IMAGE for CONTAINER_IMAGE, and the alias
// RUNNERS_VOLUME_HOST_PATH for VOLUME_HOST_PATH) were only discoverable by reading the source.
//
// Adding a table fixes only that one occurrence. This test covers the next one: a new env read added without docs
// fails the test on the spot and reports the variable name and where it should be documented.
func TestEnvVars_EveryVariableTheCodeReadsIsDocumented(t *testing.T) {
	repo := repoRoot(t)
	conf := loadDocsConf(t, repo)

	vars := map[string]string{} // Variable name -> the file that reads it, used only for error messages.
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
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repo, path)
		for _, m := range envCallRe.FindAllStringSubmatch(string(b), -1) {
			if _, seen := vars[m[1]]; !seen {
				vars[m[1]] = filepath.ToSlash(rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) < 10 {
		// If the regex is broken it silently matches nothing, and this test would stay green forever.
		t.Fatalf("only found %d env vars, envCallRe may no longer match: %v", len(vars), keysOf(vars))
	}
	for name, where := range loopReadEnvVars {
		src, errRead := os.ReadFile(filepath.Join(repo, where))
		if errRead != nil {
			t.Fatalf("%s could not be read: %v", where, errRead)
		}
		if !strings.Contains(string(src), `"`+name+`"`) {
			t.Errorf("%s is no longer in %s, remove it from loopReadEnvVars", name, where)
			continue
		}
		vars[name] = where
	}
	for name, where := range scriptEnvVars {
		vars[name] = where
	}

	// The English original and all five translations must mention each variable. Variable names are identifiers and must not be translated,
	// so "missing from one translation" means that translation lacks a line.
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
		for _, name := range keysOf(vars) {
			if !mentionsEnvVar(text, name) {
				t.Errorf("%s is not mentioned in %s (read by %s); the environment variables section is missing this line",
					filepath.ToSlash(f), name, vars[name])
			}
		}
	}
}

// mentionsEnvVar reports whether the document mentions the variable, matching on identifier boundaries.
//
// strings.Contains cannot be used: `FLEET_IMAGE_TAG` is a substring of `FLEET_IMAGE_TAGG`,
// so a typo'd variable with a longer name would still count as mentioned. I hit this myself while writing this test:
// the first version used Contains, and changing one letter of the name in a translation left the test passing.
func mentionsEnvVar(text, name string) bool {
	isNameChar := func(b byte) bool {
		return b == '_' || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		s := i + j
		e := s + len(name)
		beforeOK := s == 0 || !isNameChar(text[s-1])
		afterOK := e == len(text) || !isNameChar(text[e])
		if beforeOK && afterOK {
			return true
		}
		i = s + 1
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestEnvVars_ConfigOverridesAreInTheEnvTemplate: every variable config.go reads must appear in .env.example.
//
// That file is the entire interface for the "full container deployment, edit only .env" path, and it previously lacked three variables, two of which
// (CONTAINER_IMAGE and RUNNERS_VOLUME_HOST_PATH) are aliases that are only discoverable by reading the source.
//
// The scope is deliberately limited to internal/config/config.go: it reads exactly the set that can override the config file,
// which is the full set a .env needs to override. AGENT_PORT and similar variables that the Agent reads inside the container are not included:
// the Manager injects those when it creates the container, so putting them in the template would only be noise.
func TestEnvVars_ConfigOverridesAreInTheEnvTemplate(t *testing.T) {
	repo := repoRoot(t)

	src, err := os.ReadFile(filepath.Join(repo, "internal", "config", "config.go"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]bool{}
	for _, m := range envCallRe.FindAllStringSubmatch(string(src), -1) {
		vars[m[1]] = true
	}
	for name, where := range loopReadEnvVars {
		if where == "internal/config/config.go" {
			vars[name] = true
		}
	}
	if len(vars) < 8 {
		t.Fatalf("only found %d variables in config.go, envCallRe may no longer match", len(vars))
	}

	tmpl, err := os.ReadFile(filepath.Join(repo, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range keysOf(boolKeysAsMap(vars)) {
		if !mentionsEnvVar(string(tmpl), name) {
			t.Errorf("%s is missing from .env.example; someone deploying the full container and editing only .env would not find it", name)
		}
	}
}

func boolKeysAsMap(in map[string]bool) map[string]string {
	out := make(map[string]string, len(in))
	for k := range in {
		out[k] = ""
	}
	return out
}
