package runner

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
)

func TestGitHubSettingsURL_TargetShapes(t *testing.T) {
	cases := []struct {
		name       string
		targetType string
		target     string
		want       string
	}{
		{"仓库", "repo", "sharkpaw/runner-deck",
			"https://github.com/sharkpaw/runner-deck/settings/actions/runners"},
		{"组织", "org", "my-org",
			"https://github.com/organizations/my-org/settings/actions/runners"},
		// Organization and repository settings pages use different paths; mixing them up gives a 404
		{"大小写与空格不影响", "  REPO  ", "  sharkpaw/runner-deck  ",
			"https://github.com/sharkpaw/runner-deck/settings/actions/runners"},

		// If a trustworthy address can't be built, omit the link: a link that 404s is harder to debug than no link
		{"空目标", "repo", "", ""},
		{"仓库缺少 owner", "repo", "runner-deck", ""},
		{"仓库 owner 为空", "repo", "/runner-deck", ""},
		{"组织名里有斜杠", "org", "a/b", ""},
		{"未知目标类型", "user", "someone", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GitHubSettingsURL(tc.targetType, tc.target); got != tc.want {
				t.Fatalf("GitHubSettingsURL(%q, %q) = %q, want %q",
					tc.targetType, tc.target, got, tc.want)
			}
		})
	}
}

// This address goes into an href. The target is user-supplied, so we must prove it cannot change the protocol,
// and cannot inject a second attribute, which would add an injection point to the list page.
//
// The real safeguard is the hardcoded https://github.com prefix: whatever the target is,
// it can only appear in path segments, escaped segment by segment.
func TestGitHubSettingsURL_TargetCannotEscapeTheURL(t *testing.T) {
	hostile := []struct {
		targetType string
		target     string
	}{
		{"org", `javascript:alert(1)`},
		{"org", `my-org" onmouseover="alert(1)`},
		{"org", `my-org?x=1`},
		{"org", `my-org#frag`},
		{"org", `my-org\..\..\etc`},
		{"repo", `o/r?x=1`},
		{"repo", `o/r" onclick="alert(1)`},
		{"repo", `o/r#x`},
	}
	for _, h := range hostile {
		got := GitHubSettingsURL(h.targetType, h.target)
		if got == "" {
			continue // Rejecting it as invalid and generating no link is also a safe outcome
		}
		if !strings.HasPrefix(got, "https://github.com/") {
			t.Fatalf("target %q produced a non-github.com address: %q", h.target, got)
		}
		// Quotes and angle brackets would let it escape the href attribute; ? and # change the meaning of the path
		for _, bad := range []string{`"`, `'`, `<`, `>`, ` `, `?`, `#`} {
			if strings.Contains(strings.TrimPrefix(got, "https://github.com/"), bad) {
				t.Fatalf("target %q produced an address with unescaped %q left in it: %q", h.target, bad, got)
			}
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("target %q produced an address that cannot be parsed: %v", h.target, err)
		}
		if u.Scheme != "https" || u.Host != "github.com" {
			t.Fatalf("target %q produced %s://%s", h.target, u.Scheme, u.Host)
		}
		if !strings.HasSuffix(u.EscapedPath(), "/settings/actions/runners") {
			t.Fatalf("target %q produced a path that is not the Runners settings page: %q", h.target, u.EscapedPath())
		}
	}
}

// List and GetByName must both fill in the address, otherwise only half the rows in the UI can navigate
func TestGitHubSettingsURL_IsSetOnListedRunners(t *testing.T) {
	cfg := &config.Config{Runners: config.RunnersConfig{
		BasePath: t.TempDir(),
		Items:    []config.RunnerItem{{Name: "alpha", TargetType: "repo", Target: "o/r"}},
	}}
	list := List(cfg)
	if len(list) != 1 {
		t.Fatalf("should list 1 runner, got %d", len(list))
	}
	want := "https://github.com/o/r/settings/actions/runners"
	if list[0].GitHubURL != want {
		t.Fatalf("GitHubURL in List = %q, want %q", list[0].GitHubURL, want)
	}
	got := GetByName(cfg, "alpha")
	if got == nil {
		t.Fatal("GetByName should find alpha")
	}
	if got.GitHubURL != want {
		t.Fatalf("GitHubURL in GetByName = %q, want %q", got.GitHubURL, want)
	}
}
