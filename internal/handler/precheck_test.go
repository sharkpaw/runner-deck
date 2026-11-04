package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
)

// noContainers no container with the same name exists on the host
func noContainers(string) (bool, string, bool) { return false, "", true }

// containerNamed the named container exists
func containerNamed(name, status string) containerLookup {
	return func(cn string) (bool, string, bool) {
		if cn == name {
			return true, status, true
		}
		return false, "", true
	}
}

func conflictTypes(conflicts []RunnerConflict) []string {
	out := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, c.Type)
	}
	return out
}

func hasType(conflicts []RunnerConflict, want string) bool {
	for _, c := range conflicts {
		if c.Type == want {
			return true
		}
	}
	return false
}

func TestCollectRunnerConflicts_NameTaken(t *testing.T) {
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath: t.TempDir(),
		Items:    []config.RunnerItem{{Name: "droiddesk", TargetType: "repo", Target: "o/r"}},
	}}
	conflicts := collectRunnerConflicts(cfg, "droiddesk", "", noContainers, false)
	if !hasType(conflicts, ConflictNameTaken) {
		t.Fatalf("same-name Runner not detected: %v", conflictTypes(conflicts))
	}
	if !hasErrorConflict(conflicts) {
		t.Fatal("same name should be error level")
	}
	// a same-name Runner's own directory and container should not be reported again
	if len(conflicts) != 1 {
		t.Fatalf("with the same name there should be only one conflict, got %v", conflictTypes(conflicts))
	}
}

func TestCollectRunnerConflicts_ContainerNameCollision(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath:      base,
		ContainerMode: true,
		Items:         []config.RunnerItem{{Name: "build.1", TargetType: "org", Target: "o"}},
	}}
	// build.1 and build-1 both normalize to github-runner-build-1
	conflicts := collectRunnerConflicts(cfg, "build-1", "", noContainers, false)
	if !hasType(conflicts, ConflictContainerName) {
		t.Fatalf("container name conflict not detected: %v", conflictTypes(conflicts))
	}
	// in non-container mode the container name does not matter and should not be reported
	cfg.Runners.ContainerMode = false
	if got := collectRunnerConflicts(cfg, "build-1", "", noContainers, false); hasType(got, ConflictContainerName) {
		t.Fatalf("non-container mode should not report a container name conflict: %v", conflictTypes(got))
	}
}

func TestCollectRunnerConflicts_InstallDirTaken(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath: base,
		Items:    []config.RunnerItem{{Name: "a", Path: "shared", TargetType: "org", Target: "o"}},
	}}
	conflicts := collectRunnerConflicts(cfg, "b", "shared", noContainers, false)
	if !hasType(conflicts, ConflictInstallDir) {
		t.Fatalf("install directory conflict not detected: %v", conflictTypes(conflicts))
	}
}

func TestCollectRunnerConflicts_DirState(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: base}}

	// a leftover directory that is already registered: registering again would fail, so treat as error
	registered := filepath.Join(base, "leftover")
	if err := os.MkdirAll(registered, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registered, ".runner"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// with a registration token: config.sh rejects duplicate configuration, a hard failure
	conflicts := collectRunnerConflicts(cfg, "leftover", "", noContainers, true)
	if !hasType(conflicts, ConflictDirRegistered) || !hasErrorConflict(conflicts) {
		t.Fatalf("leftover directory already registered was not detected as error: %v", conflictTypes(conflicts))
	}
	// without a token: this is the normal way to take over an existing Runner into configuration, must not be blocked
	adopt := collectRunnerConflicts(cfg, "leftover", "", noContainers, false)
	if !hasType(adopt, ConflictDirAdopt) {
		t.Fatalf("no hint given when taking over an already registered directory: %v", conflictTypes(adopt))
	}
	if hasErrorConflict(adopt) {
		t.Fatalf("taking over a registered directory without a token should not be blocked: %v", conflictTypes(adopt))
	}

	// non-empty but unregistered: can continue, warning only
	halfInstalled := filepath.Join(base, "half")
	if err := os.MkdirAll(halfInstalled, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(halfInstalled, "config.sh"), []byte("#!/bin/sh"), 0644); err != nil {
		t.Fatal(err)
	}
	conflicts = collectRunnerConflicts(cfg, "half", "", noContainers, false)
	if !hasType(conflicts, ConflictDirExists) {
		t.Fatalf("non-empty directory not detected: %v", conflictTypes(conflicts))
	}
	if hasErrorConflict(conflicts) {
		t.Fatal("adding should not be blocked by a non-empty but unregistered directory")
	}

	// an empty directory and a nonexistent directory are not conflicts
	if got := collectRunnerConflicts(cfg, "fresh", "", noContainers, false); len(got) != 0 {
		t.Fatalf("fresh name should have no conflicts: %v", conflictTypes(got))
	}
}

func TestCollectRunnerConflicts_ExistingContainer(t *testing.T) {
	cfg := &config.Config{Runners: config.RunnersConfig{BasePath: t.TempDir(), ContainerMode: true}}
	lookup := containerNamed("github-runner-droiddesk", "running")

	conflicts := collectRunnerConflicts(cfg, "droiddesk", "", lookup, false)
	if !hasType(conflicts, ConflictContainerExists) || !hasErrorConflict(conflicts) {
		t.Fatalf("leftover container with the same name on the host not detected as error: %v", conflictTypes(conflicts))
	}

	// when docker is unavailable (known=false), the add flow must not be blocked
	unknown := func(string) (bool, string, bool) { return false, "", false }
	if got := collectRunnerConflicts(cfg, "droiddesk", "", unknown, false); len(got) != 0 {
		t.Fatalf("should skip container check when docker is unavailable: %v", conflictTypes(got))
	}
	// same for a nil lookup
	if got := collectRunnerConflicts(cfg, "droiddesk", "", nil, false); len(got) != 0 {
		t.Fatalf("should skip container check without lookup: %v", conflictTypes(got))
	}
}

func TestSuggestRunnerName_SkipsTakenNames(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath: base,
		Items: []config.RunnerItem{
			{Name: "droiddesk", TargetType: "repo", Target: "o/r"},
			{Name: "droiddesk-2", TargetType: "repo", Target: "o/r"},
		},
	}}
	if got := suggestRunnerName(cfg, "droiddesk", "", noContainers, false); got != "droiddesk-3" {
		t.Fatalf("suggested = %q, want droiddesk-3", got)
	}
}

func TestPrecheckRunner_ReportsConflictAndSuggestion(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "config.yaml")
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{
			BasePath: base,
			Items:    []config.RunnerItem{{Name: "droiddesk", TargetType: "repo", Target: "o/r"}},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	oldConfigPath := ConfigPath
	ConfigPath = cfgPath
	defer func() { ConfigPath = oldConfigPath }()

	e := echo.New()
	e.GET("/api/runner-precheck", PrecheckRunner)
	req := httptest.NewRequest(http.MethodGet, "/api/runner-precheck?name=droiddesk", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp PrecheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Available {
		t.Fatal("a same-name Runner already exists, available should be false")
	}
	if !hasType(resp.Conflicts, ConflictNameTaken) {
		t.Fatalf("conflicts = %v", conflictTypes(resp.Conflicts))
	}
	if resp.SuggestedName != "droiddesk-2" {
		t.Fatalf("suggested_name = %q, want droiddesk-2", resp.SuggestedName)
	}
}

func TestPrecheckRunner_AvailableName(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "config.yaml")
	cfg := &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{BasePath: base, Items: []config.RunnerItem{}},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	oldConfigPath := ConfigPath
	ConfigPath = cfgPath
	defer func() { ConfigPath = oldConfigPath }()

	e := echo.New()
	e.GET("/api/runner-precheck", PrecheckRunner)
	req := httptest.NewRequest(http.MethodGet, "/api/runner-precheck?name=brand-new", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var resp PrecheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Available || len(resp.Conflicts) != 0 {
		t.Fatalf("a fresh name should be available: %+v", resp)
	}
	if resp.SuggestedName != "" {
		t.Fatalf("a suggested name should not be given when available, got %q", resp.SuggestedName)
	}
}

func TestPrecheckRunner_RejectsUnsafeName(t *testing.T) {
	e := echo.New()
	e.GET("/api/runner-precheck", PrecheckRunner)
	for _, name := range []string{"../etc", "a/b", ""} {
		req := httptest.NewRequest(http.MethodGet, "/api/runner-precheck?name="+name, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name=%q status = %d, want 400", name, rec.Code)
		}
	}
}

// TestAddRunner_ConflictInsteadOfSilentRename a same-name conflict must explicitly report the conflict and give a suggested name,
// rather than silently creating droiddesk-ab12cd as before — the user thinks they are operating on droiddesk.
func TestAddRunner_ConflictInsteadOfSilentRename(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "config.yaml")
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{
			BasePath: base,
			Items:    []config.RunnerItem{{Name: "droiddesk", TargetType: "repo", Target: "o/r"}},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	oldConfigPath := ConfigPath
	ConfigPath = cfgPath
	defer func() { ConfigPath = oldConfigPath }()

	e := echo.New()
	e.POST("/api/runners", AddRunner)
	body := `{"name":"droiddesk","target_type":"repo","target":"o/r"}`
	req := httptest.NewRequest(http.MethodPost, "/api/runners", bytes.NewBufferString(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SuggestedName string           `json:"suggested_name"`
		Conflicts     []RunnerConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SuggestedName != "droiddesk-2" {
		t.Fatalf("suggested_name = %q", resp.SuggestedName)
	}
	if !hasType(resp.Conflicts, ConflictNameTaken) {
		t.Fatalf("conflicts = %v", conflictTypes(resp.Conflicts))
	}

	// configuration must not be changed
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Runners.Items) != 1 {
		t.Fatalf("a new Runner should not be written on conflict, currently %d", len(after.Runners.Items))
	}
}

// TestAddRunner_AutoRenameKeepsOldBehavior still auto-renames when explicitly requested (compatible with existing scripts)
func TestAddRunner_AutoRenameKeepsOldBehavior(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "config.yaml")
	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8080},
		Runners: config.RunnersConfig{
			BasePath: base,
			Items:    []config.RunnerItem{{Name: "droiddesk", TargetType: "repo", Target: "o/r"}},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	oldConfigPath := ConfigPath
	ConfigPath = cfgPath
	defer func() { ConfigPath = oldConfigPath }()

	e := echo.New()
	e.POST("/api/runners", AddRunner)
	body := `{"name":"droiddesk","target_type":"repo","target":"o/r","auto_rename":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/runners", bytes.NewBufferString(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Name != "droiddesk-2" {
		t.Fatalf("name = %q, want droiddesk-2", resp.Name)
	}
}
