package main

import (
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

func a11yPage(t *testing.T, infos ...runner.RunnerInfo) string {
	t.Helper()
	html, err := renderTo(t, "index.html", map[string]any{
		"Runners": infos,
		"Config":  &config.Config{},
		"T":       map[string]string{},
		"Lang":    "en",
		"TJSON":   "{}",
	})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	return html
}

// The dialog previously had neither role nor aria-modal, so screen readers would read background content mixed in with the dialog;
// The close button was written as &times;, which is read aloud as "times".
func TestTemplate_ModalIsAnAccessibleDialog(t *testing.T) {
	html := a11yPage(t)
	i := strings.Index(html, `id="runnerModal"`)
	if i < 0 {
		t.Fatal("Dialog not found")
	}
	head := html[i : i+800]
	for _, want := range []string{`role="dialog"`, `aria-modal="true"`, `aria-labelledby="modalTitle"`} {
		if !strings.Contains(head, want) {
			t.Errorf("Dialog missing %s", want)
		}
	}
	j := strings.Index(html, `id="modalClose"`)
	if j < 0 {
		t.Fatal("Close button not found")
	}
	if !strings.Contains(html[j-160:j+80], "aria-label=") {
		t.Error("&times; close button has no accessible name, so screen readers read it as \"times\"")
	}
}

// Focus management relies on these three JS segments: missing any one of them fails silently, and keyboard users just lose their place
func TestTemplate_ModalManagesFocus(t *testing.T) {
	// The script was split from index.html into static/app.js; the assertions follow the carrier, and the intent is unchanged
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		"function trapTab(",
		"lastFocused = document.activeElement",
		"document.contains(lastFocused)",
		"modal.addEventListener('keydown', (e) => trapTab(e, modal));",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Focus management missing %q", want)
		}
	}
}

// The language selector previously had no accessible name, so screen readers could only announce the currently selected language
func TestTemplate_LangSelectHasAccessibleName(t *testing.T) {
	html := a11yPage(t)
	i := strings.Index(html, `id="langSelect"`)
	if i < 0 {
		t.Fatal("Language selector not found")
	}
	if !strings.Contains(html[i:i+200], "aria-label=") {
		t.Error("Language selector missing aria-label")
	}
	if !strings.Contains(html, `for="langSelect"`) {
		t.Error("Language selector missing associated label")
	}
}

// Drift diffs and probe errors are information needed for troubleshooting, but previously were attached only to title;
// keyboard and touch users couldn't reach it. After switching to .tip, title should no longer remain, or two tooltips would appear at the same time.
func TestTemplate_DiagnosticsAreKeyboardReachable(t *testing.T) {
	html := a11yPage(t, runner.RunnerInfo{
		Name: "alpha", Status: runner.StatusUnknown,
		TargetType: "repo", Target: "o/r",
		ContainerDrift:      "image: old -> new",
		Probe:               &runner.ProbeInfo{Error: "agent 无响应", Type: "agent_unreachable"},
		RegistrationMessage: "注册失败：token 过期",
		GitHubBusy:          boolPtr(true),
	})
	for _, want := range []string{
		`<span class="tip">image: old -&gt; new</span>`,
		`<span class="tip">agent 无响应</span>`,
		`<span class="tip">注册失败：token 过期</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Diagnostic info was not attached as a focusable tooltip: %s", want)
		}
	}
	// Elements with tooltips must be focusable, or keyboard users can never reach them
	if n := strings.Count(html, `has-tip" tabindex="0"`); n < 3 {
		t.Errorf("Only %d focusable tooltip elements, fewer than expected", n)
	}
	for _, gone := range []string{
		`title="image: old -&gt; new"`,
		`title="agent 无响应"`,
		`title="注册失败：token 过期"`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("After switching to .tip, title should be removed, or two tooltips would appear: %s", gone)
		}
	}
}
