package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func getVersionBody(t *testing.T) (int, string) {
	t.Helper()
	e := echo.New()
	e.GET("/version", VersionInfo)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// /version is fully public when Basic Auth is not configured. Build details, especially go_version,
// could let anyone match a published Go runtime CVE to the exact runtime version, so this endpoint
// only exposes the version number. The commit and build time are available only through the -version command line, which runs locally and is not exposed externally.
//
// version-kit's default is this trimmed setting (HandlerConfig.IncludeBuildDetails defaults to false),
// this case guards against someone turning it on later without noticing.
func TestVersionInfo_DoesNotLeakBuildDetails(t *testing.T) {
	Version, Commit, BuildDate = "v9.9.9", "deadbeefdeadbeef", "2026-01-02T03:04:05Z" // version-check-ignore
	defer func() { Version, Commit, BuildDate = "", "", "" }()

	code, body := getVersionBody(t)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}
	if m["version"] != "v9.9.9" { // version-check-ignore
		t.Errorf("version = %v, expected v9.9.9", m["version"]) // version-check-ignore
	}
	for _, leaked := range []string{"commit", "build_date", "go_version", "platform", "compiler"} {
		if v, ok := m[leaked]; ok {
			t.Errorf("public endpoint leaked %s = %v", leaked, v)
		}
	}
	if len(m) != 1 {
		t.Errorf("response should contain only the version field, got %v", m)
	}
}

// The response body must stay byte-for-byte identical to before the change: this is a public HTTP endpoint that external monitoring may be reading.
func TestVersionInfo_BodyUnchangedFromHandWrittenVersion(t *testing.T) {
	cases := []struct{ version, want string }{
		{"v9.9.9", `{"version":"v9.9.9"}`}, // version-check-ignore
		{"", `{"version":"dev"}`},          // Falls back to dev when not injected, same as before the change
	}
	for _, tc := range cases {
		Version, Commit, BuildDate = tc.version, "abc123", "2026-01-02T03:04:05Z"
		code, body := getVersionBody(t)
		if code != http.StatusOK || body != tc.want {
			t.Errorf("Version=%q -> %d %s, expected 200 %s", tc.version, code, body, tc.want)
		}
	}
	Version, Commit, BuildDate = "", "", ""
}

// -version takes a different path: it **must** include build info, otherwise a binary's commit cannot be identified.
func TestBuildInfo_CarriesProvenanceForCLI(t *testing.T) {
	Version, Commit, BuildDate = "v9.9.9", "d5c0e6024af4", "2026-01-02T03:04:05Z" // version-check-ignore
	defer func() { Version, Commit, BuildDate = "", "", "" }()

	full := BuildInfo().Full()
	for _, want := range []string{"v9.9.9", "d5c0e60", "2026-01-02T03:04:05Z", "go1."} { // version-check-ignore
		if !strings.Contains(full, want) {
			t.Errorf("-version output does not contain %q:\n%s", want, full)
		}
	}
}

func TestBuildInfo_EmptyVersionFallsBackToDev(t *testing.T) {
	Version, Commit, BuildDate = "", "", ""
	if got := BuildInfo().Version; got != "dev" {
		t.Fatalf("Version should be dev when not injected, got %q", got)
	}
}
