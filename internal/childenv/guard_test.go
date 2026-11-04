package childenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// These two test cases are guardrails: they don't verify that Sanitize works correctly, but prevent the next person from
// bypassing it. The four call sites are green after the change, but what really keeps this from recurring is that adding a new
// os.Environ() call site or a new credential-type environment variable turns red immediately.

// repoRoot walks up from this package directory to find go.mod.
// go test sets the working directory to the tested package's source directory, so the starting point here is internal/childenv.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found walking up from %s", start)
		}
		dir = parent
	}
}

// scanDirs are the two trees holding all Go code in this repo.
var scanDirs = []string{"cmd", "internal"}

// exemptPkgDir is the only place allowed to call os.Environ(), since someone has to get the raw environment first before filtering.
const exemptPkgDir = "internal/childenv"

// TestNoRawEnvironForChildProcesses forbids os.Environ() in non-test code.
//
// Uses go/parser instead of grep: mentioning this name in comments and strings is fine (this package's doc comment mentions it several times),
// a text scan would flag those too, and the test would soon need a pile of exceptions and nobody could tell what it guards.
// The AST only looks at call expressions.
//
// Mutation check: change any one call site back to os.Environ() and this test reports that file and line.
func TestNoRawEnvironForChildProcesses(t *testing.T) {
	repo := repoRoot(t)
	fset := token.NewFileSet()

	scanned := 0
	for _, root := range scanDirs {
		err := filepath.WalkDir(filepath.Join(repo, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(repo, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if filepath.ToSlash(filepath.Dir(rel)) == exemptPkgDir {
				return nil
			}
			scanned++
			for _, line := range osEnvironCallLines(t, fset, path) {
				t.Errorf("%s:%d calls os.Environ(); child process environments must use childenv.Environ(),"+
					"which strips the Manager / Agent's own credentials", rel, line)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// If the root directory is wrong or the matcher is broken, this test would quietly scan nothing and stay green forever.
	if scanned < 20 {
		t.Fatalf("only scanned %d non-test .go files; the scan scope may be wrong", scanned)
	}

	// Matcher self-check: the one os.Environ() in this package itself must still be recognized.
	// Without it, the walk above would stay green even if it found no call sites at all.
	self := filepath.Join(repo, exemptPkgDir, "childenv.go")
	if lines := osEnvironCallLines(t, fset, self); len(lines) != 1 {
		t.Fatalf("在 %s 里找到 %d 处 os.Environ()（期望 1 处）；"+
			"either Environ's implementation changed or osEnvironCallLines no longer matches", exemptPkgDir+"/childenv.go", len(lines))
	}
}

// osEnvironCallLines returns the line number of every os.Environ() call in the file.
//
// Only the literal package name os is matched: this repo has no import alias for os and no local variable named os,
// and adding a type checker for these nonexistent cases would only make this test harder to read.
func osEnvironCallLines(t *testing.T, fset *token.FileSet, path string) []int {
	t.Helper()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("%s failed to parse: %v", path, err)
	}
	var lines []int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Environ" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
			lines = append(lines, fset.Position(call.Lparen).Line)
		}
		return true
	})
	return lines
}

// envCallRe is the same pattern as in internal/docsconsistency/envvars_test.go, copied on purpose:
// that is a test file in another package, so the symbol can't be shared, and promoting it to non-test code just to share one regex isn't worth it.
// The case of both being changed together won't happen, since it matches the call shape os.Getenv("X"), not any specific name.
var envCallRe = regexp.MustCompile(`(?:os\.Getenv|env\.Get\w*)\(\s*"([A-Z][A-Z0-9_]*)"`)

// credentialNameRe matches names that by themselves indicate a credential.
// Naming conventions aren't enforced, so this pattern can't be complete; it targets the most common oversight:
// adding a *_TOKEN and forgetting that it also gets passed through environ into the Job. Anything that slips through can only be caught by someone adding it to Denied manually,
// which is how BASIC_AUTH_USER got in.
var credentialNameRe = regexp.MustCompile(`PASSWORD|TOKEN|SECRET|_KEY$`)

// TestDenied_CoversCredentialEnvVars requires that every environment variable the code reads whose name looks like a credential be in Denied.
//
// Currently it should match BASIC_AUTH_PASSWORD and AGENT_TOKEN. The third entry in Denied,
// BASIC_AUTH_USER, is not matched by this pattern. It isn't a miss; it was added on purpose: a username isn't a secret,
// but it still belongs only to the Manager, so it is stripped too, so that what leaks isn't "half a credential".
//
// Mutation check: delete AGENT_TOKEN from Denied and this test reports it along with the file that reads it.
func TestDenied_CoversCredentialEnvVars(t *testing.T) {
	repo := repoRoot(t)

	readers := map[string]string{} // variable name -> file that reads it, used only for error messages
	for _, root := range scanDirs {
		err := filepath.WalkDir(filepath.Join(repo, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(repo, path)
			for _, m := range envCallRe.FindAllStringSubmatch(string(b), -1) {
				if _, seen := readers[m[1]]; !seen {
					readers[m[1]] = filepath.ToSlash(rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// If the regex is broken, this also quietly scans nothing.
	if len(readers) < 10 {
		t.Fatalf("only found %d environment variables; envCallRe may no longer match: %v", len(readers), sortedKeys(readers))
	}

	hits := 0
	for _, name := range sortedKeys(readers) {
		if !credentialNameRe.MatchString(name) {
			continue
		}
		hits++
		if !slices.Contains(Denied, name) {
			t.Errorf("%s (reads it with %s) has a credential-like name but is not in childenv.Denied;"+
				"it gets passed to the Job run by run.sh via environ", name, readers[name])
		}
	}
	if hits == 0 {
		t.Fatal("no credential-type variables were matched at all; credentialNameRe may no longer match anything")
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
