package main

import (
	"regexp"
	"strings"
	"testing"
)

// A wrong id in PROBE_ROW_IDS raises no error; on a healthy Runner that row
// just keeps showing "—", the very thing this change removes. This checks the list in JS against the template's
// ids match up.
func TestTemplate_ProbeRowIDsMatchMarkup(t *testing.T) {
	// The list lives in static/app.js and the row ids in the template, so both are read.
	tpl, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(tpl)

	listRe := regexp.MustCompile(`const PROBE_ROW_IDS = \[([^\]]+)\]`)
	m := listRe.FindStringSubmatch(string(js))
	if m == nil {
		t.Fatal("PROBE_ROW_IDS not found")
	}
	ids := regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1)
	if len(ids) != 5 {
		t.Fatalf("PROBE_ROW_IDS should have 5 items, got %d", len(ids))
	}
	for _, id := range ids {
		if !strings.Contains(src, `id="`+id[1]+`"`) {
			t.Errorf("%s in PROBE_ROW_IDS has no matching row in the template", id[1])
		}
	}

	// The reverse also holds: every ...Row in the template with the vProbe prefix must be in the list,
	// so that a probe field row added later cannot be silently missed
	rowRe := regexp.MustCompile(`id="(vProbe\w*Row)"`)
	inList := map[string]bool{}
	for _, id := range ids {
		inList[id[1]] = true
	}
	for _, r := range rowRe.FindAllStringSubmatch(src, -1) {
		if !inList[r[1]] {
			t.Errorf("%s in the template is not in PROBE_ROW_IDS, so without a probe error it keeps showing '—'", r[1])
		}
	}
}

// Both timestamps must go through renderTimestamp; if either is missed, that column falls back to raw RFC3339
func TestTemplate_TimestampsUseRelativeRenderer(t *testing.T) {
	// The script has moved from index.html to static/app.js, so the assertion follows the file; intent unchanged
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, id := range []string{"vRegistrationCheckedAt", "vGitHubCheckAt"} {
		if !strings.Contains(src, "renderTimestamp(document.getElementById('"+id+"')") {
			t.Errorf("%s does not go through renderTimestamp", id)
		}
	}
	// Relative time is left to Intl rather than a separate six-language copy set; it must fall back when unavailable
	if !strings.Contains(src, "Intl.RelativeTimeFormat") {
		t.Error("Relative time should use Intl.RelativeTimeFormat")
	}
	if !strings.Contains(src, "if (!rel) { el.textContent = iso; return; }") {
		t.Error("When Intl is unavailable or time parsing fails, the raw value should be shown so no information is lost")
	}
}
