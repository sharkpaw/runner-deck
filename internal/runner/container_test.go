package runner

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
)

func TestGetAgentStatus_ErrorBodyIncluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "agent unhealthy", http.StatusBadGateway)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()
	port := 80
	if u.Port() != "" {
		// httptest must always have a port; this is just defensive handling
		if p, convErr := strconv.Atoi(u.Port()); convErr == nil && p > 0 {
			port = p
		}
	}

	_, err = GetAgentStatus(context.Background(), host, port, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "agent unhealthy") {
		t.Fatalf("expected response body in error, got: %v", err)
	}
}

func TestCallAgentStart_ErrorBodyIncluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/start" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "start failed in agent", http.StatusConflict)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()
	port := 80
	if u.Port() != "" {
		if p, convErr := strconv.Atoi(u.Port()); convErr == nil && p > 0 {
			port = p
		}
	}

	err = CallAgentStart(context.Background(), host, port, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "start failed in agent") {
		t.Fatalf("expected response body in error, got: %v", err)
	}
}

func TestGetAgentStatus_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(AgentStatus{Status: "installed", Running: true})
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()
	port := 80
	if u.Port() != "" {
		if p, convErr := strconv.Atoi(u.Port()); convErr == nil && p > 0 {
			port = p
		}
	}

	st, err := GetAgentStatus(context.Background(), host, port, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Status != "installed" || !st.Running {
		t.Fatalf("unexpected status: %+v", st)
	}
}

// joinArgs makes it easy to assert whether a consecutive fragment appears in the argument sequence
func joinArgs(args []string) string {
	return strings.Join(args, "\x00")
}

func containsSeq(args []string, seq ...string) bool {
	return strings.Contains(joinArgs(args), joinArgs(seq))
}

func TestRunnerCreateArgs_HostSocketAddsDockerGroup(t *testing.T) {
	// With host-socket, the app inside the container (UID 1001) must join the socket's owning group, or docker in the Job fails with permission denied
	args, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "host-socket", "runner-dind", 999, config.ResourceLimits{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsSeq(args, "--group-add", "999") {
		t.Errorf("expected --group-add 999, got %v", args)
	}
	if !containsSeq(args, "-v", HostDockerSocket+":"+HostDockerSocket) {
		t.Errorf("expected docker.sock mount, got %v", args)
	}
	if !containsSeq(args, "-e", "DOCKER_HOST=unix://"+HostDockerSocket) {
		t.Errorf("expected unix DOCKER_HOST, got %v", args)
	}
	if args[len(args)-1] != "img:tag" {
		t.Errorf("image must be the last arg, got %v", args)
	}
}

func TestRunnerCreateArgs_HostSocketUnknownGIDSkipsGroupAdd(t *testing.T) {
	// If the socket GID cannot be detected, do not append --group-add; fall back to the docker group preset in the image
	args, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "host-socket", "runner-dind", unknownDockerGID, config.ResourceLimits{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range args {
		if a == "--group-add" {
			t.Fatalf("unexpected --group-add with unknown gid: %v", args)
		}
	}
}

func TestRunnerCreateArgs_DindAndNone(t *testing.T) {
	dind, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "dind", "my-dind", 999, config.ResourceLimits{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsSeq(dind, "-e", "DOCKER_HOST=tcp://my-dind:2375") {
		t.Errorf("expected dind DOCKER_HOST, got %v", dind)
	}
	if strings.Contains(joinArgs(dind), "--group-add") || strings.Contains(joinArgs(dind), HostDockerSocket) {
		t.Errorf("dind must not mount socket nor add docker group, got %v", dind)
	}

	none, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "none", "runner-dind", 999, config.ResourceLimits{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(joinArgs(none), "DOCKER_HOST") || strings.Contains(joinArgs(none), "--group-add") {
		t.Errorf("none backend must not inject docker env/group, got %v", none)
	}
}

func TestRunnerCreateArgs_UnsupportedBackend(t *testing.T) {
	if _, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "podman", "runner-dind", 999, config.ResourceLimits{}, ""); err == nil {
		t.Fatal("expected error for unsupported backend")
	} else if !strings.Contains(err.Error(), "job_docker_backend") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestSocketGID(t *testing.T) {
	// Returns the owning group of an existing file; returns unknownDockerGID if it does not exist
	f := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(f, []byte(""), 0660); err != nil {
		t.Fatal(err)
	}
	if got, want := socketGID(f), os.Getgid(); got != want {
		t.Errorf("socketGID = %d, want %d", got, want)
	}
	if got := socketGID(filepath.Join(t.TempDir(), "missing.sock")); got != unknownDockerGID {
		t.Errorf("socketGID(missing) = %d, want %d", got, unknownDockerGID)
	}
}

func TestRunnerDockerGID_ConfigWinsOverDetection(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runners.DockerGID = 1234
	if got := runnerDockerGID(cfg); got != 1234 {
		t.Errorf("runnerDockerGID = %d, want 1234 from config", got)
	}
	// If not configured, fall back to probing docker.sock (may not exist on the host, in which case it is unknownDockerGID)
	cfg.Runners.DockerGID = 0
	if got, want := runnerDockerGID(cfg), socketGID(HostDockerSocket); got != want {
		t.Errorf("runnerDockerGID = %d, want detected %d", got, want)
	}
}

func TestRunnerCreateArgs_ResourceLimits(t *testing.T) {
	limits := config.ResourceLimits{CPUs: "2", Memory: "4g", MemorySwap: "4g", PidsLimit: 512}
	args, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "none", "runner-dind", unknownDockerGID, limits, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range [][]string{
		{"--cpus", "2"}, {"--memory", "4g"}, {"--memory-swap", "4g"}, {"--pids-limit", "512"},
	} {
		if !containsSeq(args, want...) {
			t.Errorf("missing argument %v, actual: %v", want, args)
		}
	}
	if args[len(args)-1] != "img:tag" {
		t.Errorf("image must be at the end of the arguments: %v", args)
	}
}

func TestRunnerCreateArgs_NoLimitsKeepsOldBehaviour(t *testing.T) {
	// When no resource limits are configured, no limit arguments should be produced; keep consistent with the old version
	args, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "dind", "runner-dind", unknownDockerGID, config.ResourceLimits{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, flag := range []string{"--cpus", "--memory", "--memory-swap", "--pids-limit"} {
		if strings.Contains(joinArgs(args), flag) {
			t.Errorf("%s should not appear: %v", flag, args)
		}
	}
}

func TestResourceUpdateArgs(t *testing.T) {
	// When no limits are configured, docker update must not run; keep the old behavior
	if got := resourceUpdateArgs("github-runner-a", config.ResourceLimits{}); got != nil {
		t.Errorf("should return nil when no limits are configured, actual: %v", got)
	}
	got := resourceUpdateArgs("github-runner-a", config.ResourceLimits{CPUs: "2", Memory: "4g", MemorySwap: "4g", PidsLimit: 512})
	want := "update --cpus 2 --memory 4g --memory-swap 4g --pids-limit 512 github-runner-a"
	if strings.Join(got, " ") != want {
		t.Errorf("resourceUpdateArgs = %q\nwant %q", strings.Join(got, " "), want)
	}
	if got[len(got)-1] != "github-runner-a" {
		t.Errorf("container name must be at the end of the arguments: %v", got)
	}
}

func TestEnsureAgentToken_GeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureAgentToken(dir)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	if len(first) != agentTokenBytes*2 {
		t.Errorf("token length should be %d hex characters, actual %d: %q", agentTokenBytes*2, len(first), first)
	}
	// Permissions must be 0600: this file shares a mount directory with the Job
	info, err := os.Stat(filepath.Join(dir, AgentTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("token file permissions should be 0600, actual %v", perm)
	}
	// Repeated calls must reuse the same token, otherwise after a Manager restart it will no longer match the running Agent
	second, err := EnsureAgentToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("repeated calls should reuse the token: %q vs %q", first, second)
	}
	if got := ReadAgentToken(dir); got != first {
		t.Errorf("ReadAgentToken = %q, want %q", got, first)
	}
}

func TestEnsureAgentToken_DistinctPerRunner(t *testing.T) {
	a, err := EnsureAgentToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureAgentToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("different Runners should have different tokens, otherwise a token leaked from one Job could control other Runners")
	}
}

func TestReadAgentToken_MissingIsEmpty(t *testing.T) {
	// Old containers have no token file; must return an empty string to degrade to no authentication, not an error
	if got := ReadAgentToken(t.TempDir()); got != "" {
		t.Errorf("should return an empty string when there is no token file, actual %q", got)
	}
}

func TestSetAgentAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	setAgentAuth(req, "")
	if _, ok := req.Header["Authorization"]; ok {
		t.Error("Authorization header should not be set when the token is empty")
	}
	setAgentAuth(req, "abc123")
	if got := req.Header.Get("Authorization"); got != "Bearer abc123" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestAgentCalls_SendBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"installed","running":true}`))
	}))
	defer srv.Close()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := GetAgentStatus(context.Background(), host, port, "tok-status"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-status" {
		t.Errorf("/status sent without token: %q", gotAuth)
	}
	if err := CallAgentStart(context.Background(), host, port, "tok-start"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-start" {
		t.Errorf("/start sent without token: %q", gotAuth)
	}
}

func TestRunnerCreateArgs_InjectsAgentToken(t *testing.T) {
	// The token must be injected via an environment variable: relying only on the 0600 file mounted, if the UIDs of the Manager and Agent
	// differ, the Agent cannot read it and silently degrades to no authentication
	args, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "none", "runner-dind", unknownDockerGID, config.ResourceLimits{}, "tok-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsSeq(args, "-e", "AGENT_TOKEN=tok-abc") {
		t.Errorf("AGENT_TOKEN was not injected: %v", args)
	}
	if args[len(args)-1] != "img:tag" {
		t.Errorf("image must be at the end of the arguments: %v", args)
	}
}

func TestRunnerCreateArgs_NoTokenNoEnv(t *testing.T) {
	// When no token can be obtained, do not inject an empty value, to keep the Agent from treating an empty string as a valid token
	args, err := runnerCreateArgs("github-runner-a", "/host/runners/a", "runner-net", "img:tag", "none", "runner-dind", unknownDockerGID, config.ResourceLimits{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(joinArgs(args), "AGENT_TOKEN") {
		t.Errorf("environment variable should not be injected when the token is empty: %v", args)
	}
}
