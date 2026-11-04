package secrets

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Find a file that only exists at the repo root, so a wrong path fails right away instead of silently checking nothing.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found (%s): %v", root, err)
	}
	return root
}

// secretsPkgDir is the only place where the old path may appear
const secretsPkgDir = "internal/secrets"

// patFileName is the old file name of the PAT. It is written as a literal here on purpose, not referenced via LegacyRunnerTokenFile:
// This test checks where this literal still appears. Checking a constant with the constant itself would just let it pass by default.
const patFileName = ".github_check_token"

// legacyConstName is the old path constant name
const legacyConstName = "LegacyRunnerTokenFile"

// goFilesUnderRepo walks the non-test Go files in the repo and returns relative paths
func goFilesUnderRepo(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "runners", "config":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		// Only check product code. Using the old path in tests is legitimate, since the migration itself has to create the old file to be tested.
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no Go files were scanned, the walk itself went wrong")
	}
	return out
}

func inSecretsPkg(rel string) bool {
	return strings.HasPrefix(filepath.ToSlash(rel), secretsPkgDir+"/")
}

// TestNoManagerSecretsUnderRunnerDirs guards that the Manager's PAT is never read from a Runner directory again.
//
// Once this is fixed, it is easy to quietly revert: `filepath.Join(installDir, ".github_check_token")` is one line to write,
// and the file it reads is mounted entirely into the Runner container in container mode,
// and the Job runs as the same UID 1001, so file permissions cannot block it. So this pins down that only the migration code in internal/secrets
// may mention the old path.
//
// Use the AST rather than regex: regex can be bypassed by comments, string concatenation and line breaks, while what we need to decide is
// whether the path is actually being built.
func TestNoManagerSecretsUnderRunnerDirs(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()

	for _, rel := range goFilesUnderRepo(t) {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", rel, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.BasicLit:
				// The literal for the old file name may only appear in internal/secrets
				if v.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(v.Value)
				if err != nil {
					return true
				}
				if strings.Contains(s, patFileName) && !inSecretsPkg(rel) {
					t.Errorf("%s:%d contains the PAT's old file name %q."+
						"The PAT is now only read and written through secrets.Store ("+PathDescription+"), "+
						"a Runner directory is mounted into the Runner container in container mode",
						rel, fset.Position(v.Pos()).Line, s)
				}
			case *ast.Ident:
				// Same for the old path constant name
				if v.Name == legacyConstName && !inSecretsPkg(rel) {
					t.Errorf("%s:%d references %s. It is only kept for the migration code in %s",
						rel, fset.Position(v.Pos()).Line, legacyConstName, secretsPkgDir)
				}
			case *ast.CallExpr:
				// filepath.Join(<anything>, <something that mentions the token>) must not appear outside secrets.
				// This is what the line that puts the credential back into the Runner directory looks like.
				if inSecretsPkg(rel) || !isFilepathJoin(v) {
					return true
				}
				for _, arg := range v.Args {
					if desc, ok := mentionsPATToken(arg); ok {
						t.Errorf("%s:%d argument %s of filepath.Join looks like it builds the PAT's path."+
							"The Manager's credential does not live under the Runner directory, use secrets.Store instead",
							rel, fset.Position(v.Pos()).Line, desc)
					}
				}
			}
			return true
		})
	}
}

// isFilepathJoin reports whether this is filepath.Join(...)
func isFilepathJoin(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "filepath"
}

// mentionsPATToken reports whether an argument refers to the PAT file.
//
// Only PAT is matched: .agent_token legitimately lives in the Runner directory (the Agent runs in that container,
// and the token is also injected via an environment variable), so this guard does not cover it. See "considered but not adopted" in the PR.
func mentionsPATToken(arg ast.Expr) (string, bool) {
	switch v := arg.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		if strings.Contains(s, patFileName) {
			return strconv.Quote(s), true
		}
	case *ast.Ident:
		if isPATTokenName(v.Name) {
			return v.Name, true
		}
	case *ast.SelectorExpr:
		if isPATTokenName(v.Sel.Name) {
			return v.Sel.Name, true
		}
	}
	return "", false
}

// isPATTokenName treats a name containing both github and token as the PAT
func isPATTokenName(name string) bool {
	l := strings.ToLower(name)
	return strings.Contains(l, "token") && strings.Contains(l, "github")
}

// The guard itself has to be able to catch something, otherwise it is just code that always passes.
// Here a temporary file containing "violating code" is used to verify that it really reports.
func TestGuardrailCatchesTheViolation(t *testing.T) {
	const bad = `package x

import "path/filepath"

const RunnerTokenFile = ".github_check_token"

func read(installDir string) string {
	_ = filepath.Join(installDir, RunnerTokenFile)
	return filepath.Join(installDir, ".github_check_token")
}
`
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.go")
	if err := os.WriteFile(p, []byte(bad), 0600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	var hits int
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil && strings.Contains(s, patFileName) {
					hits++
				}
			}
		case *ast.CallExpr:
			if !isFilepathJoin(v) {
				return true
			}
			for _, arg := range v.Args {
				if _, ok := mentionsPATToken(arg); ok {
					hits++
				}
			}
		}
		return true
	})
	// Two literals + two Join arguments (the constant name RunnerTokenFile does not contain github,
	// but its value is a literal, which is already caught by the first category; the Join with .github_check_token is also caught)
	if hits < 3 {
		t.Fatalf("the guard should catch this violating code, but only matched %d place(s)", hits)
	}
}
