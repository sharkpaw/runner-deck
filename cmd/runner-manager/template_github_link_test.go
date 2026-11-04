package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/runner"
)

func zhText(t *testing.T, key string) string {
	t.Helper()
	T := map[string]string{}
	b, err := i18nFS.ReadFile("i18n/zh.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &T); err != nil {
		t.Fatal(err)
	}
	if T[key] == "" {
		t.Fatalf("zh.json has no %q", key)
	}
	return T[key]
}

// When registered, both "Registered" and "GitHub ✓" must link to the GitHub Runners settings page
func TestIndexTemplate_GitHubIndicatorsLinkToSettingsPage(t *testing.T) {
	html := renderIndex(t, runner.RunnerInfo{
		Name: "alpha", Status: runner.StatusInstalled,
		TargetType: "repo", Target: "o/r",
		RegistrationMessage: "ok", GitHubCheckAt: "2026-09-20T09:00:00Z",
		RegisteredOnGitHub: boolPtr(true),
		GitHubURL:          runner.GitHubSettingsURL("repo", "o/r"),
	})
	href := `href="https://github.com/o/r/settings/actions/runners"`
	if n := strings.Count(html, href); n != 2 {
		t.Fatalf("\"Registered\" and \"GitHub ✓\" should both link to the settings page, found %d of %s", n, href)
	}
	// Open in a new tab with noopener, otherwise the new page can manipulate this one via window.opener
	if strings.Count(html, `target="_blank"`) < 2 || strings.Count(html, `rel="noopener noreferrer"`) < 2 {
		t.Fatal("Links to GitHub should carry target=_blank and rel=noopener noreferrer")
	}
}

// Once "Registered" becomes a link, the raw registration result that was attached to it must not be lost,
// since that cell is the real diagnostic info, especially visible when registration fails.
// The carrier moved from title to .tip: title is unreachable by keyboard and touch, yet this is what troubleshooting needs.
func TestIndexTemplate_RegisteredLinkKeepsRegistrationMessageTooltip(t *testing.T) {
	msg := "注册失败：A runner exists with the same name，请先删除同名 Runner"
	html := renderIndex(t, runner.RunnerInfo{
		Name: "alpha", Status: runner.StatusInstalled,
		TargetType: "repo", Target: "o/r",
		RegistrationMessage: msg,
		RegisteredOnGitHub:  boolPtr(true), GitHubCheckAt: "2026-09-20T09:00:00Z",
		GitHubURL: runner.GitHubSettingsURL("repo", "o/r"),
	})
	if !strings.Contains(html, `<span class="tip">`+msg+`</span>`) {
		t.Fatal("The \"Registered\" link should keep the raw registration result (.tip)")
	}
	// The tooltip must be attached to the link itself, otherwise it never appears on hover or focus
	i := strings.Index(html, `class="reg-ok github-link has-tip"`)
	if i < 0 {
		t.Fatal("The \"Registered\" link is missing has-tip")
	}
	if !strings.Contains(html[i:i+len(msg)+400], `<span class="tip">`+msg+`</span>`) {
		t.Fatal("The raw registration result is not attached inside the \"Registered\" link")
	}
}

// When the target can't form a trustworthy address, prefer no link: a link that 404s is harder to troubleshoot than plain text
func TestIndexTemplate_NoLinkWithoutURL(t *testing.T) {
	html := renderIndex(t, runner.RunnerInfo{
		Name: "alpha", Status: runner.StatusInstalled,
		RegistrationMessage: "ok", GitHubCheckAt: "2026-09-20T09:00:00Z",
		RegisteredOnGitHub: boolPtr(true),
		GitHubURL:          "", // target is invalid, the Go side has already refused to generate it
	})
	if strings.Contains(html, "/settings/actions/runners") {
		t.Fatal("A link should not render when there is no trustworthy address")
	}
	// The text itself should remain, just not clickable
	if !strings.Contains(html, zhText(t, "github.yes")) {
		t.Fatal("Even without a link, the \"GitHub ✓\" text should still be displayed")
	}
}

// hrefRe extracts the values of all hrefs in the rendered output
var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

// href is handed to the browser. target is user-supplied, so we must prove it cannot escape the href even after landing in it.
//
// Assert only the href values, do not search keywords across the whole page: target also appears as text in the target column,
// where it has already been escaped into inert text such as &#34;, so searching the page for onmouseover= would match it and cause a false positive.
// What really needs proving is "a quote cannot get into the href": only a bare quote can break out of the attribute.
func TestIndexTemplate_HostileTargetCannotInjectHref(t *testing.T) {
	for _, target := range []string{
		`o/r" onmouseover="alert(1)`,
		`o/r`,
	} {
		html := renderIndex(t, runner.RunnerInfo{
			Name: "alpha", Status: runner.StatusInstalled,
			TargetType: "repo", Target: target,
			RegistrationMessage: "ok", GitHubCheckAt: "2026-09-20T09:00:00Z",
			RegisteredOnGitHub: boolPtr(true),
			GitHubURL:          runner.GitHubSettingsURL("repo", target),
		})
		var checked int
		for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
			href := m[1]
			if !strings.Contains(href, "/settings/actions/runners") {
				continue
			}
			checked++
			if !strings.HasPrefix(href, "https://github.com/") {
				t.Fatalf("target %q produced a non-github.com href: %q", target, href)
			}
			// html/template rewrites dangerous protocols in URL context to #ZgotmplZ
			if strings.Contains(href, "ZgotmplZ") || strings.Contains(href, "javascript:") {
				t.Fatalf("target %q produced an href flagged by the template: %q", target, href)
			}
			for _, bad := range []string{`"`, `'`, `<`, `>`, ` `} {
				if strings.Contains(href, bad) {
					t.Fatalf("target %q has an unescaped %q in its href: %q", target, bad, href)
				}
			}
		}
		if checked == 0 {
			t.Fatalf("target %q rendered no settings page link, the assertion failed", target)
		}
	}
}

// The busy badge only appears when GitHub reports the runner as busy.
// GitHubBusy is a *bool: writing {{if .GitHubBusy}} in the template would make "idle" show as "busy",
// the same trap as the earlier RegisteredOnGitHub bug, so all three states must be pinned down.
func TestIndexTemplate_BusyBadgeOnlyWhenBusy(t *testing.T) {
	busy := zhText(t, "badge.busy")
	cases := []struct {
		name string
		v    *bool
		want bool
	}{
		{"忙碌", boolPtr(true), true},
		{"空闲", boolPtr(false), false},
		{"未知", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			html := renderIndex(t, runner.RunnerInfo{
				Name: "alpha", Status: runner.StatusInstalled,
				TargetType: "repo", Target: "o/r",
				RegistrationMessage: "ok", GitHubBusy: tc.v,
			})
			if got := strings.Contains(html, ">"+busy+"<"); got != tc.want {
				t.Fatalf("busy badge present = %v, expected %v", got, tc.want)
			}
		})
	}
}

// The detail view must really be three columns. Without this rule in the stylesheet the modal falls back to one long vertical stack,
// while the rendered HTML looks identical, so the styles themselves must be asserted directly.
// Styles and scripts were moved out of index.html into static/; the assertion follows them, intent unchanged.
func TestIndexTemplate_ViewIsThreeColumnGrid(t *testing.T) {
	cssBytes, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	for _, want := range []string{
		"#modalView { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr));",
		"#modalView .row.span-all { grid-column: 1 / -1; }",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("stylesheet should contain %q", want)
		}
	}
	jsBytes, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	// An inline display:block overrides display:grid and breaks the three columns immediately
	if strings.Contains(js, "modalView.style.display = 'block'") {
		t.Fatal("modalView cannot be shown with inline display:block, as it would override the grid in the stylesheet")
	}
	if strings.Contains(js, "modalEditForm.style.display = 'block'") {
		t.Fatal("modalEditForm cannot be shown with inline display:block, as it would override the grid in the stylesheet")
	}
}
