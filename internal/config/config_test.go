package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoad_MissingFile(t *testing.T) {
	// If the file does not exist, Load generates it from default config plus environment variables, writes it, and returns success (so a fully containerized deployment can rely on .env alone)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("expected success when file missing (generate from defaults): %v", err)
	}
	if cfg.Server.Port != 8080 || cfg.Server.Addr != "0.0.0.0" {
		t.Errorf("expected default server, got port=%d addr=%q", cfg.Server.Port, cfg.Server.Addr)
	}
	if cfg.Runners.BasePath != "./runners" || len(cfg.Runners.Items) != 0 {
		t.Errorf("expected default runners base_path=./runners items=0, got base_path=%q items=%d", cfg.Runners.BasePath, len(cfg.Runners.Items))
	}
	// The file should already be written, so a second Load should read it back
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("re-load after generate failed: %v", err)
	}
	if loaded.Server.Port != cfg.Server.Port {
		t.Errorf("re-loaded port %d != generated %d", loaded.Server.Port, cfg.Server.Port)
	}
}

func TestLoad_MissingFile_WithEnvGeneratesConfig(t *testing.T) {
	// When the file does not exist and common variables from .env are set, Load generates config.yaml from default config plus environment variables and writes it; the content matches env
	restore := setEnvsAndRestore(t, map[string]string{
		"CONTAINER_MODE":     "true",
		"VOLUME_HOST_PATH":   "/data/runners",
		"RUNNERS_BASE_PATH":  "/app/runners",
		"SERVER_PORT":        "9090",
		"JOB_DOCKER_BACKEND": "host-socket",
		"RUNNER_IMAGE":       "custom/runner:v1",
		"MANAGER_IMAGE":      "",
		"FLEET_IMAGE_TAG":    "",
	}, []string{"CONTAINER_MODE", "VOLUME_HOST_PATH", "RUNNERS_BASE_PATH", "SERVER_PORT", "JOB_DOCKER_BACKEND", "RUNNER_IMAGE", "MANAGER_IMAGE", "FLEET_IMAGE_TAG"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(missing file) with env should succeed: %v", err)
	}
	if !cfg.Runners.ContainerMode {
		t.Error("expected ContainerMode true from CONTAINER_MODE")
	}
	if cfg.Runners.VolumeHostPath != "/data/runners" {
		t.Errorf("expected VolumeHostPath /data/runners, got %q", cfg.Runners.VolumeHostPath)
	}
	if cfg.Runners.BasePath != "/app/runners" {
		t.Errorf("expected BasePath /app/runners, got %q", cfg.Runners.BasePath)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected Server.Port 9090 from SERVER_PORT, got %d", cfg.Server.Port)
	}
	if cfg.Runners.JobDockerBackend != "host-socket" {
		t.Errorf("expected JobDockerBackend host-socket, got %q", cfg.Runners.JobDockerBackend)
	}
	if cfg.Runners.ContainerImage != "custom/runner:v1" {
		t.Errorf("expected ContainerImage from RUNNER_IMAGE, got %q", cfg.Runners.ContainerImage)
	}
	// The generated file should exist and be readable back
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to be created at %s: %v", path, err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("re-load generated file failed: %v", err)
	}
	if reloaded.Runners.BasePath != "/app/runners" || reloaded.Server.Port != 9090 {
		t.Errorf("re-loaded config mismatch: base_path=%q port=%d", reloaded.Runners.BasePath, reloaded.Server.Port)
	}
}

func TestLoad_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// For empty or partially filled fields, defaults should apply
	content := []byte(`
server: {}
runners: {}
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("expected Server.Port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Runners.BasePath != "./runners" {
		t.Errorf("expected Runners.BasePath ./runners, got %q", cfg.Runners.BasePath)
	}
}

func TestLoad_Save_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{
		Server: ServerConfig{Port: 9000, Addr: "127.0.0.1"},
		Runners: RunnersConfig{BasePath: "/tmp/runners", Items: []RunnerItem{
			{Name: "r1", TargetType: "repo", Target: "owner/repo", Labels: []string{"a", "b"}},
		}},
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server.Port != 9000 || loaded.Server.Addr != "127.0.0.1" {
		t.Errorf("server mismatch: %+v", loaded.Server)
	}
	if loaded.Runners.BasePath != "/tmp/runners" {
		t.Errorf("base_path: got %q", loaded.Runners.BasePath)
	}
	if len(loaded.Runners.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(loaded.Runners.Items))
	}
	if loaded.Runners.Items[0].Name != "r1" || loaded.Runners.Items[0].Target != "owner/repo" {
		t.Errorf("item mismatch: %+v", loaded.Runners.Items[0])
	}
}

func TestLoadAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{
		Runners: RunnersConfig{BasePath: dir, Items: []RunnerItem{}},
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	err := LoadAndSave(path, func(c *Config) error {
		c.Runners.Items = append(c.Runners.Items, RunnerItem{Name: "added", TargetType: "org", Target: "myorg"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, _ := Load(path)
	if len(loaded.Runners.Items) != 1 || loaded.Runners.Items[0].Name != "added" {
		t.Errorf("LoadAndSave roundtrip failed: %+v", loaded.Runners.Items)
	}
}

func TestRunnerItem_InstallPath(t *testing.T) {
	base := "/base"
	tests := []struct {
		path, name string
		want       string
	}{
		{"", "r1", "/base/r1"},
		{"sub", "r1", "/base/sub"},
	}
	for _, tt := range tests {
		r := RunnerItem{Path: tt.path, Name: tt.name}
		got := r.InstallPath(base)
		if got != filepath.FromSlash(tt.want) && filepath.Clean(got) != filepath.Clean(tt.want) {
			t.Errorf("InstallPath(%q, %q) = %q, want %q", tt.path, tt.name, got, tt.want)
		}
	}
}

func TestValidate_InvalidJobBackend(t *testing.T) {
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			JobDockerBackend: "bad-backend",
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "job_docker_backend") {
		t.Fatalf("expected job_docker_backend validation error, got: %v", err)
	}
}

func TestValidate_BackendModeMismatch(t *testing.T) {
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "host-socket",
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "container_mode=false") {
		t.Fatalf("expected mode mismatch error, got: %v", err)
	}
}

func TestValidate_VolumeHostPathRules(t *testing.T) {
	cfg1 := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "dind",
			VolumeHostPath:   "/abs/path",
		},
	}
	if err := Validate(cfg1); err == nil || !strings.Contains(err.Error(), "volume_host_path") {
		t.Fatalf("expected volume_host_path mode validation error, got: %v", err)
	}

	cfg2 := &Config{
		Runners: RunnersConfig{
			BasePath:         "/app/runners",
			ContainerMode:    true,
			JobDockerBackend: "dind",
		},
	}
	if err := Validate(cfg2); err == nil || !strings.Contains(err.Error(), "runners.volume_host_path must be set") {
		t.Fatalf("expected missing volume_host_path validation error, got: %v", err)
	}

	cfg3 := &Config{
		Runners: RunnersConfig{
			BasePath:         "/app/runners",
			ContainerMode:    true,
			JobDockerBackend: "dind",
			VolumeHostPath:   "relative/path",
		},
	}
	if err := Validate(cfg3); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("expected absolute path validation error, got: %v", err)
	}
}

func TestNormalizedContainerName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"r1", "github-runner-r1"},
		{"a.b", "github-runner-a-b"},
		{"a-b", "github-runner-a-b"},
		{"x_y", "github-runner-x_y"},
		{"..", "github-runner-runner"}, // Empty after sanitize, fallback is "runner"
		{"", "github-runner-runner"},
	}
	for _, tt := range tests {
		got := NormalizedContainerName(tt.name)
		if got != tt.want {
			t.Errorf("NormalizedContainerName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestIsSafeRunnerNameOrPath(t *testing.T) {
	for _, s := range []string{"", "..", "/", "\\", "a/b", "a..b"} {
		if IsSafeRunnerNameOrPath(s) {
			t.Errorf("IsSafeRunnerNameOrPath(%q) should be false", s)
		}
	}
	for _, s := range []string{"a", "r1", "a-b", "x_y"} {
		if !IsSafeRunnerNameOrPath(s) {
			t.Errorf("IsSafeRunnerNameOrPath(%q) should be true", s)
		}
	}
}

func TestValidateTarget(t *testing.T) {
	if err := ValidateTarget("org", "myorg"); err != nil {
		t.Errorf("org myorg: %v", err)
	}
	if err := ValidateTarget("org", "owner/repo"); err == nil || !strings.Contains(err.Error(), "cannot contain /") {
		t.Errorf("org owner/repo should error: %v", err)
	}
	if err := ValidateTarget("repo", "owner/repo"); err != nil {
		t.Errorf("repo owner/repo: %v", err)
	}
	if err := ValidateTarget("repo", "owner"); err == nil || !strings.Contains(err.Error(), "owner/repo") {
		t.Errorf("repo owner should error: %v", err)
	}
	if err := ValidateTarget("invalid", "x"); err == nil {
		t.Error("invalid type should error")
	}
	if err := ValidateTarget("org", ""); err == nil {
		t.Error("empty target should error")
	}
}

func TestValidate_ContainerNameConflict(t *testing.T) {
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    true,
			JobDockerBackend: "dind",
			Items: []RunnerItem{
				{Name: "a.b", TargetType: "org", Target: "org1"},
				{Name: "a-b", TargetType: "org", Target: "org1"},
			},
		},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "container name conflict") {
		t.Fatalf("expected container name conflict error, got: %v", err)
	}
}

func TestLoad_TrimAndDefaultDindHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners:
  container_mode: true
  job_docker_backend: "  dind  "
  dind_host: "   "
  volume_host_path: "   /tmp/runners   "
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners.JobDockerBackend != "dind" {
		t.Fatalf("expected backend dind, got %q", cfg.Runners.JobDockerBackend)
	}
	if cfg.Runners.DindHost != "runner-dind" {
		t.Fatalf("expected default dind_host runner-dind, got %q", cfg.Runners.DindHost)
	}
	if cfg.Runners.VolumeHostPath != "/tmp/runners" {
		t.Fatalf("expected trimmed volume_host_path /tmp/runners, got %q", cfg.Runners.VolumeHostPath)
	}
}

func TestLoad_ContainerImageWhitespaceUsesDefault(t *testing.T) {
	// When FLEET_IMAGE_TAG is not set, v2.1.0-runner is used by default
	restore := setEnvAndRestore(t, "FLEET_IMAGE_TAG", "")
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners:
  container_mode: true
  container_image: "   "
  job_docker_backend: dind
  volume_host_path: /tmp/runners
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/sharkpaw/runner-deck:v2.1.0-runner"
	if cfg.Runners.ContainerImage != want {
		t.Fatalf("expected default container image %q, got %q", want, cfg.Runners.ContainerImage)
	}
}

// setEnvAndRestore sets the environment variable and returns a defer function to restore it; an empty value means Unsetenv
func setEnvAndRestore(t *testing.T, key, value string) func() {
	t.Helper()
	old, had := os.LookupEnv(key)
	if value == "" {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		return func() {
			if had {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
	if err := os.Setenv(key, value); err != nil {
		t.Fatal(err)
	}
	return func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	}
}

func TestLoad_ContainerImageDefaultWithFleetImageTag(t *testing.T) {
	// When FLEET_IMAGE_TAG=main is set, main-runner is used by default
	restore := setEnvAndRestore(t, "FLEET_IMAGE_TAG", "main")
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners:
  container_mode: true
  container_image: ""
  job_docker_backend: dind
  volume_host_path: /tmp/runners
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/sharkpaw/runner-deck:main-runner"
	if cfg.Runners.ContainerImage != want {
		t.Fatalf("expected container image %q when FLEET_IMAGE_TAG=main, got %q", want, cfg.Runners.ContainerImage)
	}
}

func TestSave_ValidateOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "host-socket",
			Items:            []RunnerItem{},
		},
	}
	err := cfg.Save(path)
	if err == nil || !strings.Contains(err.Error(), "container_mode=false") {
		t.Fatalf("expected save-time validation error, got: %v", err)
	}
}

func TestValidate_InvalidRunnerNameOrPathFromConfig(t *testing.T) {
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "dind",
			Items: []RunnerItem{
				{Name: "bad/name", TargetType: "org", Target: "org1"},
			},
		},
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "name contains an illegal character") {
		t.Fatalf("expected invalid name validation error, got: %v", err)
	}

	cfg2 := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "dind",
			Items: []RunnerItem{
				{Name: "ok", Path: "../escape", TargetType: "org", Target: "org1"},
			},
		},
	}
	if err := Validate(cfg2); err == nil || !strings.Contains(err.Error(), "path contains an illegal character") {
		t.Fatalf("expected invalid path validation error, got: %v", err)
	}
}

func TestValidate_TargetRulesFromConfig(t *testing.T) {
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "dind",
			Items: []RunnerItem{
				{Name: "r1", TargetType: "repo", Target: "owner"},
			},
		},
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "owner/repo") {
		t.Fatalf("expected repo target validation error, got: %v", err)
	}

	cfg2 := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "dind",
			Items: []RunnerItem{
				{Name: "r1", TargetType: "org", Target: "owner/repo"},
			},
		},
	}
	if err := Validate(cfg2); err == nil || !strings.Contains(err.Error(), "cannot contain /") {
		t.Fatalf("expected org target validation error, got: %v", err)
	}
}

func TestValidate_InstallPathConflict(t *testing.T) {
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "dind",
			Items: []RunnerItem{
				{Name: "r1", Path: "same", TargetType: "org", Target: "org1"},
				{Name: "r2", Path: "same", TargetType: "org", Target: "org1"},
			},
		},
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "install directory conflict") {
		t.Fatalf("expected install path conflict validation error, got: %v", err)
	}
}

// setEnvsAndRestore sets multiple environment variables and returns a restore function; keys is the list of keys to restore (including unset ones)
func setEnvsAndRestore(t *testing.T, envs map[string]string, keysToRestore []string) func() {
	t.Helper()
	old := make(map[string]string)
	for _, k := range keysToRestore {
		if v, had := os.LookupEnv(k); had {
			old[k] = v
		}
	}
	for k, v := range envs {
		if err := os.Setenv(k, v); err != nil {
			t.Fatal(err)
		}
		if _, in := old[k]; !in {
			old[k] = "" // Mark as unset; Unsetenv on restore
		}
	}
	return func() {
		for k, v := range old {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	}
}

func TestLoad_EnvOverrides_ServerPortAndContainerModeAndVolumeHostPath(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{
		"MANAGER_PORT":       "9000",
		"CONTAINER_MODE":     "true",
		"VOLUME_HOST_PATH":   "/host/runners",
		"JOB_DOCKER_BACKEND": "host-socket",
		"CONTAINER_NETWORK":  "mynet",
		"RUNNERS_BASE_PATH":  "/app/runners",
		"RUNNER_IMAGE":       "custom/runner:tag",
		"FLEET_IMAGE_TAG":    "", // Avoid affecting the MANAGER_IMAGE derivation test
		"MANAGER_IMAGE":      "", // This test specifies RUNNER_IMAGE explicitly
	}, []string{"MANAGER_PORT", "SERVER_PORT", "CONTAINER_MODE", "VOLUME_HOST_PATH", "JOB_DOCKER_BACKEND", "CONTAINER_NETWORK", "RUNNERS_BASE_PATH", "RUNNER_IMAGE", "FLEET_IMAGE_TAG", "MANAGER_IMAGE"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: { port: 8080 }
runners: { base_path: ./runners, items: [] }
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 9000 {
		t.Errorf("expected Server.Port 9000 from MANAGER_PORT, got %d", cfg.Server.Port)
	}
	if !cfg.Runners.ContainerMode {
		t.Error("expected ContainerMode true from CONTAINER_MODE")
	}
	if cfg.Runners.VolumeHostPath != "/host/runners" {
		t.Errorf("expected VolumeHostPath /host/runners, got %q", cfg.Runners.VolumeHostPath)
	}
	if cfg.Runners.JobDockerBackend != "host-socket" {
		t.Errorf("expected job_docker_backend host-socket, got %q", cfg.Runners.JobDockerBackend)
	}
	if cfg.Runners.ContainerNetwork != "mynet" {
		t.Errorf("expected container_network mynet, got %q", cfg.Runners.ContainerNetwork)
	}
	if cfg.Runners.BasePath != "/app/runners" {
		t.Errorf("expected base_path /app/runners, got %q", cfg.Runners.BasePath)
	}
	if cfg.Runners.ContainerImage != "custom/runner:tag" {
		t.Errorf("expected container_image from RUNNER_IMAGE, got %q", cfg.Runners.ContainerImage)
	}
}

func TestLoad_EnvOverrides_SERVER_PORT(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{"SERVER_PORT": "7000"}, []string{"SERVER_PORT", "MANAGER_PORT"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server: {}\nrunners: { items: [] }"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 7000 {
		t.Errorf("expected Server.Port 7000 from SERVER_PORT, got %d", cfg.Server.Port)
	}
}

func TestLoad_ContainerImageFromManagerImage(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{
		"MANAGER_IMAGE":   "ghcr.io/sharkpaw/runner-deck:v1.0.1", // version-check-ignore: the v1.0.1 here is an input sample for MANAGER_IMAGE, not the current version
		"CONTAINER_MODE":  "true",
		"FLEET_IMAGE_TAG": "",
		"RUNNER_IMAGE":    "",
		"CONTAINER_IMAGE": "",
	}, []string{"MANAGER_IMAGE", "CONTAINER_MODE", "FLEET_IMAGE_TAG", "RUNNER_IMAGE", "CONTAINER_IMAGE"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners:
  container_mode: false
  items: []
  job_docker_backend: host-socket
  volume_host_path: /tmp/runners
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/sharkpaw/runner-deck:v1.0.1-runner" // version-check-ignore: same as above, the assertion checks the result derived from the input above
	if cfg.Runners.ContainerImage != want {
		t.Fatalf("expected container_image %q from MANAGER_IMAGE, got %q", want, cfg.Runners.ContainerImage)
	}
}

func TestLoad_ContainerImageFromManagerImage_NoTag(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{
		"MANAGER_IMAGE":   "ghcr.io/sharkpaw/runner-deck",
		"CONTAINER_MODE":  "true",
		"FLEET_IMAGE_TAG": "",
	}, []string{"MANAGER_IMAGE", "CONTAINER_MODE", "FLEET_IMAGE_TAG"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners:
  items: []
  job_docker_backend: host-socket
  volume_host_path: /tmp/runners
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/sharkpaw/runner-deck:latest-runner"
	if cfg.Runners.ContainerImage != want {
		t.Fatalf("expected container_image %q when MANAGER_IMAGE has no tag, got %q", want, cfg.Runners.ContainerImage)
	}
}

func TestLoad_EnvOverrides_SERVER_ADDR(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{"SERVER_ADDR": "127.0.0.1"}, []string{"SERVER_ADDR"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server: {}\nrunners: { items: [] }"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != "127.0.0.1" {
		t.Errorf("expected Server.Addr 127.0.0.1 from SERVER_ADDR, got %q", cfg.Server.Addr)
	}
}

func TestLoad_EnvOverrides_RUNNERS_VOLUME_HOST_PATH(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{
		"CONTAINER_MODE":           "true",
		"VOLUME_HOST_PATH":         "/first",
		"RUNNERS_VOLUME_HOST_PATH": "/second",
		"JOB_DOCKER_BACKEND":       "host-socket",
		"RUNNER_IMAGE":             "img:runner",
	}, []string{"CONTAINER_MODE", "VOLUME_HOST_PATH", "RUNNERS_VOLUME_HOST_PATH", "JOB_DOCKER_BACKEND", "RUNNER_IMAGE"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners: { base_path: /app/runners, items: [], volume_host_path: /yaml }
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners.VolumeHostPath != "/second" {
		t.Errorf("expected VolumeHostPath /second (RUNNERS_VOLUME_HOST_PATH overrides), got %q", cfg.Runners.VolumeHostPath)
	}
}

func TestLoad_EnvOverrides_CONTAINER_IMAGE_overrides_RUNNER_IMAGE(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{
		"CONTAINER_MODE":     "true",
		"RUNNER_IMAGE":       "runner:one",
		"CONTAINER_IMAGE":    "container:two",
		"JOB_DOCKER_BACKEND": "host-socket",
		"VOLUME_HOST_PATH":   "/tmp/r",
	}, []string{"CONTAINER_MODE", "RUNNER_IMAGE", "CONTAINER_IMAGE", "JOB_DOCKER_BACKEND", "VOLUME_HOST_PATH"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server: {}\nrunners: { base_path: /app/runners, items: [] }"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners.ContainerImage != "container:two" {
		t.Errorf("expected container_image container:two (CONTAINER_IMAGE wins), got %q", cfg.Runners.ContainerImage)
	}
}

func TestLoad_EnvOverrides_InvalidPortIgnored(t *testing.T) {
	for _, envVal := range []string{"abc", "0", "-1", "  "} {
		restore := setEnvsAndRestore(t, map[string]string{"MANAGER_PORT": envVal}, []string{"MANAGER_PORT"})
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("server: { port: 8080 }\nrunners: { items: [] }"), 0644); err != nil {
			restore()
			t.Fatal(err)
		}
		cfg, err := Load(path)
		restore()
		if err != nil {
			t.Fatalf("MANAGER_PORT=%q: Load failed: %v", envVal, err)
		}
		if cfg.Server.Port != 8080 {
			t.Errorf("MANAGER_PORT=%q: expected port unchanged 8080, got %d", envVal, cfg.Server.Port)
		}
	}
}

func TestLoad_EnvOverrides_CONTAINER_MODE_1(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{"CONTAINER_MODE": "1"}, []string{"CONTAINER_MODE"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners:
  items: []
  job_docker_backend: host-socket
  volume_host_path: /tmp/runners
  container_image: custom:runner
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Runners.ContainerMode {
		t.Error("expected ContainerMode true when CONTAINER_MODE=1")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: ["), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestValidate_EmptyItemName(t *testing.T) {
	cfg := &Config{
		Runners: RunnersConfig{
			BasePath:         "./runners",
			ContainerMode:    false,
			JobDockerBackend: "dind",
			Items: []RunnerItem{
				{Name: "  ", TargetType: "org", Target: "myorg"},
			},
		},
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "name cannot be empty") {
		t.Fatalf("expected empty name validation error, got: %v", err)
	}
}

func TestLoadAndSave_LoadFails(t *testing.T) {
	// When the file does not exist, Load generates it from the default config, and LoadAndSave should succeed and apply fn
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := LoadAndSave(path, func(c *Config) error {
		c.Server.Port = 9000
		return nil
	})
	if err != nil {
		t.Fatalf("LoadAndSave with missing file should succeed (config generated): %v", err)
	}
	loaded, _ := Load(path)
	if loaded.Server.Port != 9000 {
		t.Errorf("expected fn change persisted, got port %d", loaded.Server.Port)
	}
}

func TestLoadAndSave_FnReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{Runners: RunnersConfig{BasePath: dir, Items: []RunnerItem{}}}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	err := LoadAndSave(path, func(c *Config) error {
		return fmt.Errorf("intentional failure")
	})
	if err == nil || err.Error() != "intentional failure" {
		t.Fatalf("expected fn error to propagate, got: %v", err)
	}
}

func TestLoad_ContainerImageFromManagerImage_ImageWithRegistryPort(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{
		"MANAGER_IMAGE":   "host:5000/my/repo:v1.0",
		"CONTAINER_MODE":  "true",
		"FLEET_IMAGE_TAG": "",
	}, []string{"MANAGER_IMAGE", "CONTAINER_MODE", "FLEET_IMAGE_TAG"})
	defer restore()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: {}
runners:
  items: []
  job_docker_backend: host-socket
  volume_host_path: /tmp/runners
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "host:5000/my/repo:v1.0-runner"
	if cfg.Runners.ContainerImage != want {
		t.Fatalf("expected container_image %q (last colon separates tag), got %q", want, cfg.Runners.ContainerImage)
	}
}

func TestLoad_DockerGIDFromFileAndEnv(t *testing.T) {
	// docker_gid can be set in the config file and overridden by the DOCKER_GID environment variable (the same variable used for group_add in .env)
	restore := setEnvsAndRestore(t, map[string]string{"DOCKER_GID": ""}, []string{"DOCKER_GID"})
	defer restore()
	if err := os.Unsetenv("DOCKER_GID"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte(`
server: { port: 8080 }
runners:
  base_path: ./runners
  items: []
  container_mode: true
  volume_host_path: /host/runners
  job_docker_backend: host-socket
  docker_gid: 113
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners.DockerGID != 113 {
		t.Errorf("expected docker_gid 113 from file, got %d", cfg.Runners.DockerGID)
	}

	if err := os.Setenv("DOCKER_GID", "996"); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners.DockerGID != 996 {
		t.Errorf("expected docker_gid 996 from DOCKER_GID env, got %d", cfg.Runners.DockerGID)
	}
}

func TestLoad_DockerGIDDefaultsToZero(t *testing.T) {
	// Zero when unset, meaning Manager auto-detects the group that owns docker.sock
	restore := setEnvsAndRestore(t, map[string]string{"DOCKER_GID": ""}, []string{"DOCKER_GID"})
	defer restore()
	if err := os.Unsetenv("DOCKER_GID"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server: { port: 8080 }\nrunners: { base_path: ./runners, items: [] }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runners.DockerGID != 0 {
		t.Errorf("expected docker_gid default 0, got %d", cfg.Runners.DockerGID)
	}
}

func TestValidate_NegativeDockerGID(t *testing.T) {
	c := &Config{}
	c.Runners.BasePath = "./runners"
	c.Runners.JobDockerBackend = "dind"
	c.Runners.DockerGID = -5
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "docker_gid") {
		t.Fatalf("expected docker_gid validation error, got: %v", err)
	}
}

func containerModeConfig(items ...RunnerItem) *Config {
	c := &Config{}
	c.Runners.BasePath = "/data/runners"
	c.Runners.ContainerMode = true
	c.Runners.VolumeHostPath = "/data/runners"
	c.Runners.JobDockerBackend = "dind"
	c.Runners.Items = items
	return c
}

func TestContainerImageFor_ItemOverridesGlobal(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{"FLEET_IMAGE_TAG": "", "MANAGER_IMAGE": ""}, []string{"FLEET_IMAGE_TAG", "MANAGER_IMAGE"})
	defer restore()

	c := containerModeConfig(
		RunnerItem{Name: "droiddesk", TargetType: "repo", Target: "o/r", ContainerImage: "reg/flutter-runner:1"},
		RunnerItem{Name: "plain", TargetType: "repo", Target: "o/r2"},
	)
	c.Runners.ContainerImage = "reg/global-runner:1"

	if got := c.ContainerImageFor("droiddesk"); got != "reg/flutter-runner:1" {
		t.Errorf("item override not applied: %q", got)
	}
	if got := c.ContainerImageFor("plain"); got != "reg/global-runner:1" {
		t.Errorf("did not fall back to global: %q", got)
	}
	// Unknown names also fall back to global, to avoid getting an empty image when creating a container
	if got := c.ContainerImageFor("not-configured"); got != "reg/global-runner:1" {
		t.Errorf("unknown runner did not fall back to global: %q", got)
	}

	c.Runners.ContainerImage = ""
	if got, want := c.ContainerImageFor("plain"), DefaultRunnerContainerImage(); got != want {
		t.Errorf("when global is also empty, should fall back to the default image: got %q want %q", got, want)
	}
}

func TestJobDockerBackendFor_ItemOverridesGlobal(t *testing.T) {
	c := containerModeConfig(
		RunnerItem{Name: "iso", TargetType: "repo", Target: "o/r", JobDockerBackend: "  NONE  "},
		RunnerItem{Name: "plain", TargetType: "repo", Target: "o/r2"},
	)
	c.Runners.JobDockerBackend = "host-socket"

	if got := c.JobDockerBackendFor("iso"); got != "none" {
		t.Errorf("item override not applied or case/whitespace not normalized: %q", got)
	}
	if got := c.JobDockerBackendFor("plain"); got != "host-socket" {
		t.Errorf("did not fall back to global: %q", got)
	}

	c.Runners.JobDockerBackend = ""
	if got := c.JobDockerBackendFor("plain"); got != DefaultJobDockerBackend {
		t.Errorf("when global is also empty, should fall back to %q: got %q", DefaultJobDockerBackend, got)
	}
}

func TestValidate_ItemJobDockerBackend(t *testing.T) {
	c := containerModeConfig(RunnerItem{Name: "r", TargetType: "repo", Target: "o/r", JobDockerBackend: "podman"})
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "items[0].job_docker_backend") {
		t.Fatalf("invalid backend should error, got: %v", err)
	}

	c = containerModeConfig(RunnerItem{Name: "r", TargetType: "repo", Target: "o/r", JobDockerBackend: "host-socket"})
	if err := Validate(c); err != nil {
		t.Fatalf("valid backend should not error: %v", err)
	}
}

func TestValidate_ItemContainerFieldsRequireContainerMode(t *testing.T) {
	// Consistent with runners.volume_host_path: container-mode-only fields are not allowed to be set in non-container mode
	base := func(item RunnerItem) *Config {
		c := &Config{}
		c.Runners.BasePath = "./runners"
		c.Runners.JobDockerBackend = "dind"
		c.Runners.Items = []RunnerItem{item}
		return c
	}
	err := Validate(base(RunnerItem{Name: "r", TargetType: "repo", Target: "o/r", ContainerImage: "reg/x:1"}))
	if err == nil || !strings.Contains(err.Error(), "items[0].container_image") {
		t.Fatalf("setting container_image in non-container mode should error, got: %v", err)
	}
	err = Validate(base(RunnerItem{Name: "r", TargetType: "repo", Target: "o/r", JobDockerBackend: "none"}))
	if err == nil || !strings.Contains(err.Error(), "items[0].job_docker_backend") {
		t.Fatalf("setting job_docker_backend in non-container mode should error, got: %v", err)
	}
	if err := Validate(base(RunnerItem{Name: "r", TargetType: "repo", Target: "o/r"})); err != nil {
		t.Fatalf("should not error when these two fields are not set: %v", err)
	}
}

func TestLoad_ItemOverridesRoundTrip(t *testing.T) {
	// Per-runner fields can be read from yaml, and omitempty ensures noise fields are not written back when unset
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`
server: { port: 8080 }
runners:
  base_path: /data/runners
  container_mode: true
  volume_host_path: /data/runners
  job_docker_backend: dind
  items:
    - name: droiddesk
      target_type: repo
      target: soulteary/droiddesk
      container_image: reg/flutter-runner:1
      job_docker_backend: none
    - name: plain
      target_type: repo
      target: soulteary/other
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ContainerImageFor("droiddesk"); got != "reg/flutter-runner:1" {
		t.Errorf("container_image not read in: %q", got)
	}
	if got := cfg.JobDockerBackendFor("droiddesk"); got != "none" {
		t.Errorf("item job_docker_backend not read in: %q", got)
	}
	if got := cfg.JobDockerBackendFor("plain"); got != "dind" {
		t.Errorf("runner without override should fall back to global: %q", got)
	}

	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(saved), "container_image:") != 2 {
		// One comes from runners.container_image (no omitempty), one from droiddesk; plain should not appear
		t.Errorf("omitempty not in effect, written content:\n%s", saved)
	}
}

func TestResourceLimits_Validate(t *testing.T) {
	cases := []struct {
		name    string
		limits  ResourceLimits
		wantErr string // Empty means it should pass
	}{
		{"全空不限制", ResourceLimits{}, ""},
		{"正常取值", ResourceLimits{CPUs: "1.5", Memory: "4g", MemorySwap: "8g", PidsLimit: 512}, ""},
		{"pids 不限", ResourceLimits{PidsLimit: -1}, ""},
		{"内存纯字节数", ResourceLimits{Memory: "1073741824"}, ""},
		{"swap 设为 -1", ResourceLimits{Memory: "4g", MemorySwap: "-1"}, ""},
		{"swap 等于 memory", ResourceLimits{Memory: "4g", MemorySwap: "4g"}, ""},
		{"cpus 非数字", ResourceLimits{CPUs: "two"}, "cpus"},
		{"cpus 为 0", ResourceLimits{CPUs: "0"}, "cpus"},
		{"cpus 为 0.0", ResourceLimits{CPUs: "0.0"}, "cpus"},
		{"cpus 科学计数法 docker 不接受", ResourceLimits{CPUs: "1e3"}, "cpus"},
		{"内存单位非法", ResourceLimits{Memory: "4gb"}, "memory"},
		{"pids 小于 -1", ResourceLimits{PidsLimit: -2}, "pids_limit"},
		{"只设 swap 不设 memory", ResourceLimits{MemorySwap: "4g"}, "memory_swap"},
		{"swap 小于 memory", ResourceLimits{Memory: "4g", MemorySwap: "1g"}, "memory_swap"},
		{"swap 小于 memory（跨单位）", ResourceLimits{Memory: "2g", MemorySwap: "1024m"}, "memory_swap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.limits.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("should pass validation, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("should report an error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseDockerSize(t *testing.T) {
	cases := map[string]int64{
		"1024":  1024,
		"1b":    1,
		"1k":    1024,
		"1m":    1024 * 1024,
		"1g":    1024 * 1024 * 1024,
		"1G":    1024 * 1024 * 1024,
		"1.5g":  1024 * 1024 * 1024 * 3 / 2,
		"2048m": 2 * 1024 * 1024 * 1024,
	}
	for in, want := range cases {
		got, err := parseDockerSize(in)
		if err != nil {
			t.Errorf("parseDockerSize(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseDockerSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "4gb", "abc", "-1"} {
		if _, err := parseDockerSize(bad); err == nil {
			t.Errorf("parseDockerSize(%q) should error", bad)
		}
	}
}

func TestResourceLimits_Args(t *testing.T) {
	if got := (ResourceLimits{}).Args(); len(got) != 0 {
		t.Errorf("should produce no args when unset: %v", got)
	}
	got := strings.Join(ResourceLimits{CPUs: " 2 ", Memory: "4g", PidsLimit: 512}.Args(), " ")
	want := "--cpus 2 --memory 4g --pids-limit 512"
	if got != want {
		t.Errorf("Args() = %q, want %q", got, want)
	}
}

func TestLoad_ResourceLimitsRejectedEarly(t *testing.T) {
	// Invalid values should fail at Load, not wait until docker create fails
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`
server: { port: 8080 }
runners:
  base_path: ./runners
  items: []
  resources:
    cpus: "many"
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "cpus") {
		t.Fatalf("should reject invalid cpus at load, got: %v", err)
	}
}

// TestLoad_EnvOverrides_PortTrimsSurroundingWhitespace pins that the port is trimmed before it is parsed.
//
// It's common for .env and the compose environment: block to have a trailing space. cli-kit's
// env.GetInt does not trim, so switching to it would silently break "MANAGER_PORT=9090 ",
// and keep listening on the configured port, a deploy failure with no logs. This test case catches it.
func TestLoad_EnvOverrides_PortTrimsSurroundingWhitespace(t *testing.T) {
	for _, envVal := range []string{" 9090", "9090 ", "  9090  "} {
		restore := setEnvsAndRestore(t, map[string]string{"MANAGER_PORT": envVal}, []string{"MANAGER_PORT"})
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("server: { port: 8080 }\nrunners: { items: [] }"), 0644); err != nil {
			restore()
			t.Fatal(err)
		}
		cfg, err := Load(path)
		restore()
		if err != nil {
			t.Fatalf("MANAGER_PORT=%q: Load failed: %v", envVal, err)
		}
		if cfg.Server.Port != 9090 {
			t.Errorf("MANAGER_PORT=%q: expected 9090 after trimming, got %d", envVal, cfg.Server.Port)
		}
	}
}

// TestValidate_ServerPortRange rejects the port range in Validate, not at ListenAndServe.
func TestValidate_ServerPortRange(t *testing.T) {
	newCfg := func(port int) *Config {
		return &Config{
			Server:  ServerConfig{Port: port},
			Runners: RunnersConfig{BasePath: "./runners", JobDockerBackend: "dind"},
		}
	}
	// 0 means unset: Load fills in the default, and Validate should not error on it,
	// much code builds Config directly and then calls Validate without setting Server.
	if err := Validate(newCfg(0)); err != nil {
		t.Fatalf("port=0 should be treated as unset and allowed, got: %v", err)
	}
	for _, port := range []int{8080, 1, 65535} {
		if err := Validate(newCfg(port)); err != nil {
			t.Errorf("port=%d should be valid, got: %v", port, err)
		}
	}
	for _, port := range []int{65536, 70000, -1} {
		err := Validate(newCfg(port))
		if err == nil || !strings.Contains(err.Error(), "server.port") {
			t.Errorf("port=%d should be rejected and name server.port, got: %v", port, err)
		}
	}
}

// TestLoad_EnvOverrides_PortOutOfRangeFailsLoudly requires out-of-range ports to fail at load time.
//
// Deliberately not silently ignored in applyEnvOverrides: ignoring it would start the process on the default port,
// so the UI would sit on 8080 instead of your 70000, with no hint. Failing loudly shows the offending config key at a glance.
func TestLoad_EnvOverrides_PortOutOfRangeFailsLoudly(t *testing.T) {
	restore := setEnvsAndRestore(t, map[string]string{"MANAGER_PORT": "70000"}, []string{"MANAGER_PORT"})
	defer restore()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server: { port: 8080 }\nrunners: { items: [] }"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "server.port") {
		t.Fatalf("MANAGER_PORT=70000 should fail at Load and name server.port, got: %v", err)
	}
}

// TestValidate_JobDockerBackendCaseInsensitive also accepts mixed-case values in the config file.
func TestValidate_JobDockerBackendCaseInsensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("server: { port: 8080 }\nrunners:\n  base_path: ./runners\n  job_docker_backend: DinD\n  items: []\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("DinD should be accepted, got: %v", err)
	}
	if cfg.Runners.JobDockerBackend != "dind" {
		t.Errorf("expected normalized to dind, got %q", cfg.Runners.JobDockerBackend)
	}
}

// TestSaveAndLoad_ConcurrentReadersNeverSeePartialConfig ensures concurrent reads during a save never see a half-written config.
//
// This reproduces the production symptom: os.WriteFile does O_TRUNC before writing. The frontend polls every 15 seconds,
// and each API request's getConfig, plus the background self-start and GitHub check loops, can land right in that gap.
// A read then sees either a half-written YAML (one 500), or an empty file. yaml.Unmarshal does not report an error for empty input,
// and returns the zero-value config, so the UI shows "no runners at all". mu only serializes writers,
// and Load takes no lock, so it cannot prevent this. After switching to rename-based publishing, readers can only see A or B.
//
// This test fails probabilistically on the old implementation (depending on whether a reader happens to land between truncation and finishing the write);
// the deterministic proof is TestWriteFile_TargetUntouchedUntilRename in internal/atomicfile.
func TestSaveAndLoad_ConcurrentReadersNeverSeePartialConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	// The two configs have different item counts, and reading an empty file (zero-value config) matches neither of them
	a := &Config{
		Server:  ServerConfig{Port: 9000, Addr: "127.0.0.1"},
		Runners: RunnersConfig{BasePath: "/tmp/runners", Items: []RunnerItem{{Name: "a1", TargetType: "org", Target: "myorg"}}},
	}
	b := &Config{
		Server: ServerConfig{Port: 9000, Addr: "127.0.0.1"},
		Runners: RunnersConfig{BasePath: "/tmp/runners", Items: []RunnerItem{
			{Name: "b1", TargetType: "repo", Target: "owner/repo"},
			{Name: "b2", TargetType: "repo", Target: "owner/other"},
		}},
	}
	names := func(c *Config) string {
		out := make([]string, 0, len(c.Runners.Items))
		for _, it := range c.Runners.Items {
			out = append(out, it.Name)
		}
		return strings.Join(out, ",")
	}
	wantA, wantB := "a1", "b1,b2"

	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}

	const rounds = 500
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < rounds; i++ {
			for _, c := range []*Config{a, b} {
				if err := c.Save(path); err != nil {
					t.Errorf("Save: %v", err)
					return
				}
			}
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				cfg, err := Load(path)
				if err != nil {
					t.Errorf("Load collided with the file being saved: %v", err)
					return
				}
				if got := names(cfg); got != wantA && got != wantB {
					t.Errorf("Load read neither A nor B: items=[%s] (the empty read is the 'no runners at all' seen in the UI)", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
