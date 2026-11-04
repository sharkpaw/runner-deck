package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

// renderIndex renders one runner row using the real index.html and returns HTML.
// Run the template directly instead of only testing the helper methods on RunnerInfo: the root of this bug is in how the template is written,
// If someone changes {{if .GitHubYes}} back to {{if .RegisteredOnGitHub}}, the bug recurs as-is,
// and tests that only cover the helper methods know nothing about it.
func renderIndex(t *testing.T, info runner.RunnerInfo) string {
	t.Helper()
	T := map[string]string{}
	b, err := i18nFS.ReadFile("i18n/zh.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &T); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	r := newTemplateRenderer()
	data := map[string]any{
		"Runners": []runner.RunnerInfo{info},
		"Config":  &config.Config{},
		"T":       T,
		"Lang":    "zh",
		"TJSON":   "{}",
		"Version": "test",
	}
	if err := r.Render(&buf, "index.html", data, echo.New().NewContext(nil, nil)); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	return buf.String()
}

func boolPtr(b bool) *bool { return &b }

// This is the core regression case for this fix.
//
// When {{if .RegisteredOnGitHub}} is applied to a *bool, html/template only checks whether the pointer is nil --
// a pointer to false is also truthy. So "not found on GitHub" renders as "GitHub ✓",
// so the list column can never show "not shown".
func TestIndexTemplate_GitHubStatusThreeStates(t *testing.T) {
	T := map[string]string{}
	b, _ := i18nFS.ReadFile("i18n/zh.json")
	_ = json.Unmarshal(b, &T)
	yes, no, failed, pending := T["github.yes"], T["github.no"], T["github.failed"], T["github.pending"]

	cases := []struct {
		name    string
		info    runner.RunnerInfo
		want    string
		notWant []string
	}{
		{
			name: "已登记",
			info: runner.RunnerInfo{Name: "a", Status: runner.StatusInstalled,
				RegistrationMessage: "ok", GitHubCheckAt: "2026-09-20T09:00:00Z",
				RegisteredOnGitHub: boolPtr(true)},
			want:    yes,
			notWant: []string{no, failed},
		},
		{
			name: "确实没登记",
			info: runner.RunnerInfo{Name: "a", Status: runner.StatusInstalled,
				RegistrationMessage: "ok", GitHubCheckAt: "2026-09-20T09:00:00Z",
				RegisteredOnGitHub: boolPtr(false)},
			want:    no,
			notWant: []string{yes, failed},
		},
		{
			name: "查过但没查出来",
			info: runner.RunnerInfo{Name: "a", Status: runner.StatusInstalled,
				RegistrationMessage: "ok", GitHubCheckAt: "2026-09-20T09:00:00Z",
				RegisteredOnGitHub: nil, GitHubCheckError: "GitHub 返回 401：令牌无效或已过期"},
			want:    failed,
			notWant: []string{yes, no},
		},
		{
			name: "从未检查",
			info: runner.RunnerInfo{Name: "a", Status: runner.StatusInstalled,
				RegistrationMessage: "ok"},
			want:    pending,
			notWant: []string{yes, no, failed},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			html := renderIndex(t, tc.info)
			if !strings.Contains(html, tc.want) {
				t.Fatalf("page should contain %q", tc.want)
			}
			for _, nw := range tc.notWant {
				if nw != "" && strings.Contains(html, nw) {
					t.Fatalf("page should not contain %q (it was actually rendered)", nw)
				}
			}
		})
	}
}

// The "public repo" badge only appears when the repo is known to be public.
//
// Same root as the case above: GitHubPublic is also a *bool, so writing {{if .GitHubPublic}} in the template makes a pointer to
// false also truthy, so every private repo gets the "public repo" badge. The whole point of this badge
// is to single out public repos; flagging everything makes it useless. nil must not trigger it either: not found is not public.
func TestIndexTemplate_PublicRepoBadgeOnlyWhenKnownPublic(t *testing.T) {
	T := map[string]string{}
	b, _ := i18nFS.ReadFile("i18n/zh.json")
	_ = json.Unmarshal(b, &T)
	label := T["badge.public_repo"]
	if label == "" {
		t.Fatal("zh.json has no badge.public_repo")
	}

	for _, tc := range []struct {
		name string
		v    *bool
		want bool
	}{
		{"公开仓库", boolPtr(true), true},
		{"私有仓库", boolPtr(false), false},
		{"还没查出来", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := renderIndex(t, runner.RunnerInfo{
				Name: "a", Status: runner.StatusInstalled, RegistrationMessage: "ok",
				TargetType: "repo", Target: "acme/app", GitHubPublic: tc.v,
			})
			got := strings.Contains(html, `class="badge public-repo`)
			if got != tc.want {
				t.Fatalf("rendered badge = %v, expected %v", got, tc.want)
			}
			// The hint text goes with the badge: a badge without .tip leaves users unable to see "why this is a problem"
			if tc.want && !strings.Contains(html, T["badge.public_repo_title"]) {
				t.Fatal("badge should have a .tip explaining why public repos are risky")
			}
			if !tc.want && strings.Contains(html, label) {
				t.Fatalf("page should not contain %q", label)
			}
		})
	}
}

// The reason for a failed lookup must be visible in the UI, otherwise "lookup failed" gives no way to act on it
func TestIndexTemplate_ShowsCheckErrorAsTooltip(t *testing.T) {
	html := renderIndex(t, runner.RunnerInfo{
		Name: "a", Status: runner.StatusInstalled, RegistrationMessage: "ok",
		GitHubCheckAt:    "2026-09-20T09:00:00Z",
		GitHubCheckError: "GitHub 返回 401：令牌无效或已过期",
	})
	if !strings.Contains(html, "令牌无效或已过期") {
		t.Fatal("failure reason should appear on the page (title tooltip)")
	}
}

// Angle brackets in runner names must be escaped; the server-side rendering must not open a hole
func TestIndexTemplate_EscapesRunnerName(t *testing.T) {
	html := renderIndex(t, runner.RunnerInfo{
		Name: `<img src=x onerror=alert(1)>`, Status: runner.StatusNew,
	})
	if strings.Contains(html, "<img src=x onerror=alert(1)>") {
		t.Fatal("runner name not escaped, list page has XSS")
	}
	if !strings.Contains(html, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Fatal("should appear in escaped form")
	}
}
