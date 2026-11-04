package main

import (
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

func rowsData(runners []runner.RunnerInfo, containerMode bool) map[string]any {
	cfg := &config.Config{}
	cfg.Runners.ContainerMode = containerMode
	return map[string]any{
		"Runners": runners,
		"Config":  cfg,
		"T":       map[string]string{},
		"Lang":    "en",
		"TJSON":   "{}",
	}
}

// Auto-refresh fetches this fragment, so it must render on its own, without index.html.
// Otherwise the UI would replace <tbody> with an error page or blank every 15 seconds.
func TestRender_RunnerRowsRendersStandalone(t *testing.T) {
	html, err := renderTo(t, "runnerRows", rowsData([]runner.RunnerInfo{
		{Name: "alpha", Status: runner.StatusInstalled, Target: "acme", TargetType: "org"},
	}, false))
	if err != nil {
		t.Fatalf("fragment render failed: %v", err)
	}
	if !strings.Contains(html, "alpha") {
		t.Fatalf("no runner name in fragment: %s", html)
	}
	// The fragment is only the <tbody> content and should not pull in the whole page.
	if strings.Contains(html, "<html") || strings.Contains(html, "<table") {
		t.Fatalf("fragment should not contain page skeleton: %s", html)
	}
}

// After extracting into {{define}}, the most likely first-load bug is 'page still there, rows gone':
// A wrong template name or data not passed down renders without error, just with a missing section.
func TestRender_IndexStillEmbedsRunnerRows(t *testing.T) {
	data := rowsData([]runner.RunnerInfo{
		{Name: "beta", Status: runner.StatusInstalled, Target: "acme/app", TargetType: "repo", InstallDir: "/runners/beta"},
	}, false)
	page, err := renderTo(t, "index.html", data)
	if err != nil {
		t.Fatalf("index page render failed: %v", err)
	}
	frag, err := renderTo(t, "runnerRows", data)
	if err != nil {
		t.Fatalf("fragment render failed: %v", err)
	}
	if strings.TrimSpace(frag) == "" {
		t.Fatal("fragment rendered empty")
	}
	// First load and polling must share the same row rendering logic, or the UI changes look after one refresh.
	if !strings.Contains(page, strings.TrimSpace(frag)) {
		t.Fatal("rows on index page differ from runnerRows fragment")
	}
	if !strings.Contains(page, `id="runnerRowsBody"`) {
		t.Fatal("index page is missing the runnerRowsBody container, JS has nothing to replace")
	}
}

// Container mode has one extra column, so the empty-list row's colspan changes too; when the fragment renders alone
// Config must be passed all the way down; if it is dropped, this falls back to 6 columns.
func TestRender_RunnerRowsHonorsContainerMode(t *testing.T) {
	html, err := renderTo(t, "runnerRows", rowsData(nil, true))
	if err != nil {
		t.Fatalf("fragment render failed: %v", err)
	}
	if !strings.Contains(html, `colspan="7"`) {
		t.Fatalf("empty list in container mode should span 7 columns: %s", html)
	}
}
