package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

func filterPageData(runners []runner.RunnerInfo) map[string]any {
	return map[string]any{
		"Runners": runners,
		"Config":  &config.Config{},
		"T":       map[string]string{},
		"Lang":    "en",
		"TJSON":   "{}",
	}
}

// Filtering and counting read the data-* attributes on the row, not the visible text in the cells—
// that text is translated per language, so filtering in another language breaks. If one attribute goes missing,
// the matching chip silently counts 0 forever, with no error.
func TestTemplate_RowsCarryFilterAttributes(t *testing.T) {
	drift := "image: a -> b"
	html, err := renderTo(t, "index.html", filterPageData([]runner.RunnerInfo{{
		Name:           "alpha",
		Status:         runner.StatusInstalled,
		TargetType:     "repo",
		Target:         "acme/app",
		Running:        true,
		ContainerDrift: drift,
		Probe:          &runner.ProbeInfo{Error: "boom"},
	}}))
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	for _, want := range []string{
		`data-name="alpha"`,
		`data-target="repo: acme/app"`,
		`data-status="installed"`,
		`data-running="1"`,
		`data-drift="1"`,
		`data-probe="1"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("row is missing filter attribute %s", want)
		}
	}
}

// All three boolean attributes must be rendered as "0", not an empty string: the JS checks === '1',
// but a missing attribute and a value of 0 are two different forms in the template, so it's easy to fix only half.
func TestTemplate_RowBooleanAttributesAreZeroWhenFalse(t *testing.T) {
	html, err := renderTo(t, "index.html", filterPageData([]runner.RunnerInfo{{
		Name:       "beta",
		Status:     runner.StatusNew,
		TargetType: "org",
		Target:     "acme",
	}}))
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	for _, want := range []string{`data-running="0"`, `data-drift="0"`, `data-probe="0"`} {
		if !strings.Contains(html, want) {
			t.Errorf("boolean attribute not rendered as 0: %s", want)
		}
	}
}

// The empty-filter-result row must have the same column count as the header, or the table misaligns when filters hide everything
func TestTemplate_NoMatchRowSpansAllColumns(t *testing.T) {
	data := filterPageData(nil)
	cfg := &config.Config{}
	cfg.Runners.ContainerMode = true
	data["Config"] = cfg
	html, err := renderTo(t, "index.html", data)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	idx := strings.Index(html, `id="noMatchRow"`)
	if idx < 0 {
		t.Fatal("noMatchRow not found")
	}
	if !strings.Contains(html[idx:idx+200], `colspan="7"`) {
		t.Errorf("empty filter row in container mode should span 7 columns: %s", html[idx:idx+200])
	}
}

// The class names btn-view / btn-del are not just styling; they are also behavior selectors for row actions:
// the UI uses them to bind start/stop, view, and delete handlers. If you borrow one purely for its styling,
// it gets wired up as a row action—this is exactly how the filter bar's "clear" button once opened the view dialog by mistake
// (it has no data-name, so it requested /api/runners/null and the dialog showed "load failed").
// For small buttons used only for styling, use .btn-neutral.
func TestTemplate_RowActionClassesAlwaysCarryName(t *testing.T) {
	b, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	rowAction := map[string]bool{
		"btn-view": true, "btn-edit": true, "btn-del": true,
		"btn-start": true, "btn-stop": true, "btn-recreate": true,
	}
	// The data-name on the two footer buttons of the dialog is set by JS when the dialog opens,
	allowed := map[string]bool{"modalStartBtnFooter": true, "modalStopBtnFooter": true}
	classRe := regexp.MustCompile(`class="([^"]*)"`)
	idRe := regexp.MustCompile(`id="([^"]+)"`)
	for i, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "<button") {
			continue
		}
		m := classRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// so match on the full token: that's how CSS class selectors match,
		// so btn-edit-primary is not selected by .btn-edit and should not be flagged as a match
		hit := ""
		for _, cls := range strings.Fields(m[1]) {
			if rowAction[cls] {
				hit = cls
				break
			}
		}
		if hit == "" || strings.Contains(line, "data-name=") {
			continue
		}
		if id := idRe.FindStringSubmatch(line); id != nil && allowed[id[1]] {
			continue
		}
		t.Errorf("row %d has button with row-action class %s but no data-name; use .btn-neutral for styling-only buttons:\n  %s",
			i+1, hit, strings.TrimSpace(line))
	}
}
