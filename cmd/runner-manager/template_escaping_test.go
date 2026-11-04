package main

import (
	"regexp"
	"strings"
	"testing"
)

// dataRef matches references to interface return values, such as data.message, data.install_dir
var dataRef = regexp.MustCompile(`data\.[A-Za-z_][A-Za-z0-9_]*`)

// escapedRef matches references that have already been escaped: escapeHtml(data.x ...
var escapedRef = regexp.MustCompile(`escapeHtml\(\s*data\.[A-Za-z_][A-Za-z0-9_]*`)

// boolRef matches references used only for truthiness checks and not concatenated into strings: data.x ? / data.x &&
var boolRef = regexp.MustCompile(`data\.[A-Za-z_][A-Za-z0-9_]*\s*(\?|&&|\|\||\))`)

// Interface return values embed the user-entered Runner name, but name validation only blocks .. / \,
// names like `<img src=x onerror=...>` are accepted (they get created as directories verbatim, written into the config,
// and then echoed back by the interface). Anywhere concatenated into innerHTML must be escaped.
//
// Scan instead of asserting a single line: this bug is that 'the neighboring line escaped correctly, but this line slipped through',
// pinning one specific line would not stop a new unescaped spot added elsewhere later.
func TestIndexTemplate_NoUnescapedDataInInnerHTML(t *testing.T) {
	b, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "innerHTML") || !strings.Contains(line, "data.") {
			continue
		}
		stripped := escapedRef.ReplaceAllString(line, "")
		stripped = boolRef.ReplaceAllString(stripped, "")
		if m := dataRef.FindAllString(stripped, -1); len(m) > 0 {
			bad = append(bad, strings.TrimSpace(line)+"  ← "+strings.Join(m, ", "))
		}
	}
	if len(bad) > 0 {
		t.Fatalf("The following innerHTML assignments contain interface fields not passed through escapeHtml, through which a Runner name can inject script:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// Guard the three regexes above themselves: if they are written wrong, they become empty checks that always pass
func TestIndexTemplate_EscapingScannerCatchesUnescaped(t *testing.T) {
	cases := []struct {
		line string
		want bool // true = should be flagged as 'has unescaped fields'
	}{
		{`msgEl.innerHTML = data.message + '<br>';`, true},
		{`el.innerHTML = '<span>' + data.install_dir + '</span>';`, true},
		{`msgEl.innerHTML = escapeHtml(data.message || '');`, false},
		{`el.innerHTML = data.running ? '<b>x</b>' : '';`, false},
		{`el.innerHTML = '<s>' + escapeHtml(data.status || '') + '</s>';`, false},
	}
	for _, tc := range cases {
		stripped := escapedRef.ReplaceAllString(tc.line, "")
		stripped = boolRef.ReplaceAllString(stripped, "")
		got := len(dataRef.FindAllString(stripped, -1)) > 0
		if got != tc.want {
			t.Errorf("scanner classified %q as %v, expected %v", tc.line, got, tc.want)
		}
	}
}
