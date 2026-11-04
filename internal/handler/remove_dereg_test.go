package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/githubcheck"
	"github.com/sharkpaw/runner-deck/internal/secrets"
)

// tokenPlacement: where the optional PAT in the test is placed
type tokenPlacement int

const (
	noToken          tokenPlacement = iota
	tokenInStore                    // New location: <config dir>/tokens/<name>
	tokenInRunnerDir                // Old location: under the runner directory, moved away by migration before it is read
)

// removeTestSetup builds a config with only one registered runner and points ConfigPath at it.
//
// The directory layout mirrors production: the config dir and base_path are kept separate, because the credential dir is mounted under the config dir,
// if they overlapped, we could not verify that the credentials are not in the tree that gets mounted into the container.
func removeTestSetup(t *testing.T, placement tokenPlacement) (base, installDir string) {
	t.Helper()
	root := t.TempDir()
	base = filepath.Join(root, "runners")
	installDir = filepath.Join(base, "alpha")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".runner"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(root, "config")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{BasePath: base, Items: []config.RunnerItem{
			{Name: "alpha", Path: "alpha", TargetType: "repo", Target: "o/r"},
		}},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	orig := ConfigPath
	ConfigPath = cfgPath
	t.Cleanup(func() { ConfigPath = orig })

	switch placement {
	case tokenInStore:
		store := secrets.NewStore(cfgPath)
		if err := os.MkdirAll(store.Dir, 0700); err != nil {
			t.Fatal(err)
		}
		tp, err := store.Path("alpha")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tp, []byte("pat"), 0600); err != nil {
			t.Fatal(err)
		}
	case tokenInRunnerDir:
		if err := os.WriteFile(filepath.Join(installDir, secrets.LegacyRunnerTokenFile), []byte("pat"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return base, installDir
}

// storedToken reads the PAT from the new location; an empty string means there is none
func storedToken(t *testing.T) string {
	t.Helper()
	return secrets.NewStore(ConfigPath).GitHubToken("alpha")
}

func doRemove(t *testing.T) (int, map[string]any) {
	t.Helper()
	e := echo.New()
	e.DELETE("/api/runners/:name", RemoveRunnerByName)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/runners/alpha", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func stubDereg(t *testing.T, fn func(ctx context.Context, store *secrets.Store, installDir, targetType, target, name string) githubcheck.DeregisterResult) {
	t.Helper()
	orig := deregisterFromGitHub
	deregisterFromGitHub = fn
	t.Cleanup(func() { deregisterFromGitHub = orig })
}

// Deleting a runner must try to deregister it from GitHub: deleting only locally would leave a runner with the same name on GitHub,
// so re-adding it later under the same name would fail registration due to the name clash.
func TestRemoveRunner_DeregistersFromGitHub(t *testing.T) {
	defer withI18n(map[string]string{
		"api.removed":       "REMOVED %s",
		"github.dereg.done": "DEREGISTERED",
	})()
	_, installDir := removeTestSetup(t, tokenInStore)
	var gotDir, gotType, gotTarget, gotName string
	called := false
	stubDereg(t, func(_ context.Context, _ *secrets.Store, dir, tt, target, name string) githubcheck.DeregisterResult {
		called = true
		gotDir, gotType, gotTarget, gotName = dir, tt, target, name
		return githubcheck.DeregisterResult{Done: true, MessageKey: "github.dereg.done"}
	})

	code, body := doRemove(t)
	if code != http.StatusOK {
		t.Fatalf("status code %d, response %v", code, body)
	}
	if !called {
		t.Fatal("deletion did not attempt to deregister from GitHub")
	}
	if gotDir != installDir || gotType != "repo" || gotTarget != "o/r" || gotName != "alpha" {
		t.Fatalf("wrong deregister arguments: dir=%q type=%q target=%q name=%q", gotDir, gotType, gotTarget, gotName)
	}
	if body["github_deregistered"] != true {
		t.Fatalf("response should say deregistered, got %v", body)
	}
	// The deregistration result must be embedded in the response message. Assert by key: the outer layer is api.removed,
	// the inner layer is the key returned by githubcheck; both layers are rendered by the handler in the same request language.
	if !strings.Contains(body["message"].(string), "DEREGISTERED") {
		t.Fatalf("deregistration result should be written into the response message, got %v", body["message"])
	}
}

// Order is a hard constraint: deregistration needs this runner's PAT, and once the credential is deleted it is no longer available.
// The token is no longer in the install directory, but the constraint 'deregister first, then delete the credential' still holds.
func TestRemoveRunner_DeregistersBeforeDeletingTheStoredToken(t *testing.T) {
	_, _ = removeTestSetup(t, tokenInStore)
	tokenReadable := false
	stubDereg(t, func(_ context.Context, store *secrets.Store, _, _, _, _ string) githubcheck.DeregisterResult {
		// At the moment of the call the credential must still exist, otherwise a real implementation could not get it at all.
		// Read through the store passed in, which also pins down that the handler really passes the store down.
		if store != nil && store.GitHubToken("alpha") == "pat" {
			tokenReadable = true
		}
		return githubcheck.DeregisterResult{Done: true}
	})

	if code, body := doRemove(t); code != http.StatusOK {
		t.Fatalf("status code %d, response %v", code, body)
	}
	if !tokenReadable {
		t.Fatal("deregistration happened after the credential was deleted, when the PAT was already gone")
	}
	// The credential must not be left behind after deletion: the next runner with the same name would inherit it unexpectedly
	if got := storedToken(t); got != "" {
		t.Fatalf("after deleting the runner, tokens/alpha should be gone, but read %q", got)
	}
}

// The PAT in the old location must also be usable: before deregistration it gets moved to the credential directory,
// so the constraint 'deregistration must come before deleting the install directory' still applies.
func TestRemoveRunner_DeregistersBeforeDeletingInstallDir(t *testing.T) {
	_, installDir := removeTestSetup(t, tokenInRunnerDir)
	legacy := filepath.Join(installDir, secrets.LegacyRunnerTokenFile)
	legacyReadable := false
	stubDereg(t, func(_ context.Context, _ *secrets.Store, dir, _, _, _ string) githubcheck.DeregisterResult {
		// At the moment of the call the old file must still exist, since migration reads from here
		if b, err := os.ReadFile(filepath.Join(dir, secrets.LegacyRunnerTokenFile)); err == nil && strings.TrimSpace(string(b)) == "pat" {
			legacyReadable = true
		}
		return githubcheck.DeregisterResult{Done: true}
	})

	if code, body := doRemove(t); code != http.StatusOK {
		t.Fatalf("status code %d, response %v", code, body)
	}
	if !legacyReadable {
		t.Fatal("deregistration happened after the directory was deleted, when the PAT in the old location was already gone")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("the install directory should still ultimately be deleted")
	}
	if got := storedToken(t); got != "" {
		t.Fatalf("after deleting the runner, tokens/alpha should be gone, but read %q", got)
	}
}

// Deleting one runner must not also delete someone else's credential
func TestRemoveRunner_LeavesOtherRunnersTokensAlone(t *testing.T) {
	_, _ = removeTestSetup(t, tokenInStore)
	store := secrets.NewStore(ConfigPath)
	other, err := store.Path("beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("beta-pat"), 0600); err != nil {
		t.Fatal(err)
	}
	stubDereg(t, func(_ context.Context, _ *secrets.Store, _, _, _, _ string) githubcheck.DeregisterResult {
		return githubcheck.DeregisterResult{Done: true}
	})

	if code, body := doRemove(t); code != http.StatusOK {
		t.Fatalf("status code %d, response %v", code, body)
	}
	if got := store.GitHubToken("beta"); got != "beta-pat" {
		t.Fatalf("beta's credential should not have been touched, got %q", got)
	}
}

// A failed deregistration does not block local deletion; the user wants it removed from here,
// but we must honestly report that one is still left on GitHub, and where to delete it.
func TestRemoveRunner_DeregisterFailureIsReportedNotSwallowed(t *testing.T) {
	defer withI18n(map[string]string{
		"api.removed":         "REMOVED-FROM-CONFIG %s",
		"github.dereg.no_pat": "NOT-DELETED %s Settings %q",
	})()
	_, _ = removeTestSetup(t, noToken)
	stubDereg(t, func(_ context.Context, _ *secrets.Store, _, _, _, _ string) githubcheck.DeregisterResult {
		return githubcheck.DeregisterResult{
			Done:        false,
			MessageKey:  "github.dereg.no_pat",
			MessageArgs: []any{".github_check_token", "alpha"},
		}
	})

	code, body := doRemove(t)
	if code != http.StatusOK {
		t.Fatalf("a failed deregistration should not fail the whole deletion, status code %d", code)
	}
	if body["github_deregistered"] != false {
		t.Fatalf("response should say not deregistered, got %v", body)
	}
	msg, _ := body["message"].(string)
	// Assert both layers by key: the outer layer is api.removed, and the embedded half-sentence is the key returned by githubcheck,
	// rendered by the handler in the same request language; cross-package messages now follow the language too.
	for _, want := range []string{"REMOVED-FROM-CONFIG", "NOT-DELETED", "Settings"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Response body %q should contain %q", msg, want)
		}
	}
}

// A nonexistent runner should not trigger deregistration
func TestRemoveRunner_UnknownNameDoesNotDeregister(t *testing.T) {
	_, _ = removeTestSetup(t, tokenInStore)
	stubDereg(t, func(_ context.Context, _ *secrets.Store, _, _, _, _ string) githubcheck.DeregisterResult {
		t.Error("Unfound runners should not attempt deregistration")
		return githubcheck.DeregisterResult{}
	})
	e := echo.New()
	e.DELETE("/api/runners/:name", RemoveRunnerByName)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/runners/不存在", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Should return 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// When main injects no Secrets, derive from ConfigPath so the credentials directory sits next to the config file
func TestSecretStore_FallsBackToConfigPath(t *testing.T) {
	origStore := Secrets
	Secrets = nil
	t.Cleanup(func() { Secrets = origStore })
	origPath := ConfigPath
	ConfigPath = filepath.Join("some", "where", "config.yaml")
	t.Cleanup(func() { ConfigPath = origPath })

	if got, want := secretStore().Dir, filepath.Join("some", "where", "tokens"); got != want {
		t.Fatalf("Credentials directory is %q, expected %q", got, want)
	}

	// Use the injected one if provided
	injected := &secrets.Store{Dir: filepath.Join("injected", "tokens")}
	Secrets = injected
	if got := secretStore(); got != injected {
		t.Fatalf("Should return the store injected by main, got %+v", got)
	}
}
