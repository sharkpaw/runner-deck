package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Agent's /start and /stop can stop someone else's Runner, and any container on the same docker network
// can reach it, including the service containers the Job itself starts. Authentication is the only boundary here,
// Previously there were no tests at all.

// agentDir creates an install directory and points RUNNER_INSTALL_DIR at it.
// When the token is non-empty, write it to .agent_token in the directory.
func agentDir(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	if token != "" {
		if err := os.WriteFile(filepath.Join(dir, agentTokenFile), []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RUNNER_INSTALL_DIR", dir)
	t.Setenv("AGENT_TOKEN", "")
	return dir
}

func callGuarded(t *testing.T, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	handler := requireToken(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/start", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// ---- expectedToken ----

func TestExpectedTokenPrefersEnvOverFile(t *testing.T) {
	agentDir(t, "from-file")
	t.Setenv("AGENT_TOKEN", "from-env")
	// The environment variable takes priority: when Manager runs as root or as a UID other than 1001, the 0600 token file
	// is unreadable by app (1001) inside the container, so relying on the file alone would silently fall back to no auth
	if got := expectedToken(); got != "from-env" {
		t.Fatalf("expectedToken() = %q, want from-env from the environment variable", got)
	}
}

func TestExpectedTokenFallsBackToFile(t *testing.T) {
	agentDir(t, "  from-file\n") // Leading and trailing whitespace should be stripped
	if got := expectedToken(); got != "from-file" {
		t.Fatalf("expectedToken() = %q, want from-file", got)
	}
}

// Control requests must also be rejected when an old container has no token.
func TestExpectedTokenEmptyWhenNoTokenConfigured(t *testing.T) {
	agentDir(t, "")
	if got := expectedToken(); got != "" {
		t.Fatalf("expectedToken() = %q, want empty string", got)
	}
}

// If reading the file fails, return an empty string so requireToken rejects the request.
func TestExpectedTokenUnreadableFileReturnsEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits behave differently on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("when running as root, no permission bits can block reads")
	}
	dir := agentDir(t, "secret")
	if err := os.Chmod(filepath.Join(dir, agentTokenFile), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, agentTokenFile), 0o600) })

	if got := expectedToken(); got != "" {
		t.Fatalf("expectedToken() = %q, should return empty string when unreadable", got)
	}
}

// ---- requireToken ----

func TestRequireTokenRejectsWhenNoTokenConfigured(t *testing.T) {
	agentDir(t, "")
	if rec := callGuarded(t, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("should reject when no token is configured, got %d", rec.Code)
	}
}

func TestRequireTokenRejectsBadCredentials(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"完全不带 Authorization", ""},
		{"令牌不对", "Bearer wrong-token"},
		{"只有前缀没有令牌", "Bearer "},
		{"只有 scheme 没有空格", "Bearer"},
		{"令牌是正确值的前缀", "Bearer the-secret-token"},
		{"令牌比正确值长", "Bearer the-secret-token-value-extra"},
		// A bare token is no longer accepted: the docs in all six languages specify Authorization: Bearer,
		// and Manager has always sent Bearer, so the code has no reason to be looser than the docs
		{"令牌对但没有 scheme", "the-secret-token-value"},
		{"换了别的 scheme", "Basic the-secret-token-value"},
		{"scheme 拼错", "Bearerr the-secret-token-value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentDir(t, "the-secret-token-value")
			rec := callGuarded(t, tc.header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "unauthorized") {
				t.Fatalf("response body should say unauthorized, actual: %s", rec.Body.String())
			}
		})
	}
}

func TestRequireTokenAcceptsCorrectToken(t *testing.T) {
	agentDir(t, "the-secret-token-value")
	cases := []string{
		"Bearer the-secret-token-value",
		"Bearer   the-secret-token-value  ", // Whitespace between the scheme and the token, and after the token, is stripped
		"  Bearer the-secret-token-value",   // Leading whitespace on the header itself
		// RFC 7235: the scheme name is case-insensitive
		"bearer the-secret-token-value",
		"BEARER the-secret-token-value",
	}
	for _, header := range cases {
		t.Run(header, func(t *testing.T) {
			if rec := callGuarded(t, header); rec.Code != http.StatusOK {
				t.Fatalf("correct token should be allowed, got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The token is read on every request: Manager may write the token into the mounted directory only after the container has started,
// so if it were read once at startup and cached, a token written later would never take effect
func TestRequireTokenPicksUpTokenWrittenAfterStart(t *testing.T) {
	dir := agentDir(t, "")
	if rec := callGuarded(t, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no token at first, should reject, got %d", rec.Code)
	}
	if err := os.WriteFile(filepath.Join(dir, agentTokenFile), []byte("late-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := callGuarded(t, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("should start authenticating immediately after the token is written to disk, got %d", rec.Code)
	}
	if rec := callGuarded(t, "Bearer late-token"); rec.Code != http.StatusOK {
		t.Fatalf("with the new token should be allowed, got %d", rec.Code)
	}
}

// ---- handleStatus ----

func TestHandleStatusReportsDiskState(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T) string
		wantStatus string
	}{
		{
			name:       "目录不存在",
			setup:      func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
			wantStatus: "missing",
		},
		{
			name:       "目录在但没注册过",
			setup:      func(t *testing.T) string { return t.TempDir() },
			wantStatus: "new",
		},
		{
			name: "有 .runner 即已注册",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantStatus: "installed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUNNER_INSTALL_DIR", tc.setup(t))
			rec := httptest.NewRecorder()
			handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status code %d, expected 200", rec.Code)
			}
			var got statusResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("response is not JSON: %s", rec.Body.String())
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, expected %q", got.Status, tc.wantStatus)
			}
			// None of these cases starts a real process, so Running must be false.
			// This case used to be wrong: running state was read from a nonexistent pid file, so it was always false,
			// which in turn made Manager restart every Runner every 5 minutes.
			if got.Running {
				t.Fatal("running should be false when there is no Runner process")
			}
		})
	}
}

func TestHandleStatusRejectsNonGet(t *testing.T) {
	t.Setenv("RUNNER_INSTALL_DIR", t.TempDir())
	rec := httptest.NewRecorder()
	handleStatus(rec, httptest.NewRequest(http.MethodPost, "/status", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code %d, expected 405", rec.Code)
	}
}

// ---- installDir ----

func TestInstallDirDefault(t *testing.T) {
	t.Setenv("RUNNER_INSTALL_DIR", "")
	if got := installDir(); got != defaultInstallDir {
		t.Fatalf("installDir() = %q, expected default %q", got, defaultInstallDir)
	}
}
