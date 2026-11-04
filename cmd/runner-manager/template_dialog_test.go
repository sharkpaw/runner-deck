package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Native alert/confirm blocks the whole page, and the browser's "prevent this page from showing more dialogs" option can disable them permanently
// Once that is ticked, confirmed actions like delete and recreate silently do nothing.
// The UI uses showToast / confirmDialog throughout; this guards against them being reverted.
func TestTemplate_NoNativeDialogs(t *testing.T) {
	// The script has moved from index.html to static/app.js, so scan both: if only the template is scanned, this case
	// would always pass, since the template no longer contains any JS.
	tpl, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	// confirmDialog( won't match: after confirm comes D, not an open parenthesis.
	re := regexp.MustCompile(`\b(alert|confirm|prompt)\s*\(`)
	var hits []string
	for _, f := range []struct {
		name string
		body []byte
	}{{"templates/index.html", tpl}, {"static/app.js", js}} {
		for i, line := range strings.Split(string(f.body), "\n") {
			if m := re.FindString(line); m != "" {
				hits = append(hits, f.name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("Native dialog found, use showToast / confirmDialog instead:\n%s", strings.Join(hits, "\n"))
	}
}

// Buttons must be disabled while a request is in flight, otherwise users will click repeatedly during the tens of seconds it takes to start a runner,
// and the same action gets sent twice. This can only verify that setRowBusy is actually called in the code.
func TestTemplate_ActionsMarkButtonsBusy(t *testing.T) {
	// Same as above; the assertion follows the carrier file.
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"function setRowBusy(",
		"async function runnerAction(name, action, btn)",
		"setRowBusy(btn, true)",
		"setRowBusy(btn, false)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("Cannot find %q in static/app.js; the button busy state may have been changed", want)
		}
	}
}
