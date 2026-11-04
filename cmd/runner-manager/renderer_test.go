package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/sharkpaw/runner-deck/internal/config"
	"github.com/sharkpaw/runner-deck/internal/runner"
)

func TestNewTemplateRenderer_ParsesEmbeddedTemplates(t *testing.T) {
	r := newTemplateRenderer()
	if r == nil || r.templates == nil {
		t.Fatal("renderer or template set is empty")
	}
	// Both names must be findable: ParseFS registers the name differently across Go versions,
	// Render's fallback exists for exactly this
	if r.templates.Lookup("index.html") == nil && r.templates.Lookup("templates/index.html") == nil {
		t.Fatal("index.html can't be found under either name")
	}
}

func renderTo(t *testing.T, name string, data any) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := newTemplateRenderer().Render(&buf, name, data, echo.New().NewContext(nil, nil))
	return buf.String(), err
}

// Render's contract: whether ParseFS registers the template as index.html or templates/index.html,
// rendering by basename must work (the handler passes the basename).
// The reverse does not hold: the current Go version registers only the basename, so calling with a path-qualified name should error.
func TestRender_IndexRendersByBaseName(t *testing.T) {
	data := map[string]any{
		"Runners": []runner.RunnerInfo{{Name: "alpha", Status: runner.StatusInstalled}},
		"Config":  &config.Config{},
		"T":       map[string]string{},
		"Lang":    "zh",
		"TJSON":   "{}",
		"Version": "test",
	}
	html, err := renderTo(t, "index.html", data)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(html, "alpha") {
		t.Fatal("rendered page does not contain the runner name")
	}

	// The fallback logic itself: register the template under a different name, basename must still render
	tpl := template.Must(template.New("templates/x.html").Parse(`hello {{.}}`))
	r := &templateRenderer{templates: tpl}
	var buf bytes.Buffer
	if err := r.Render(&buf, "x.html", "world", echo.New().NewContext(nil, nil)); err != nil {
		t.Fatalf("when the template is registered with a path, rendering by basename should succeed: %v", err)
	}
	if buf.String() != "hello world" {
		t.Fatalf("render result is %q", buf.String())
	}
}

// An unknown template name must return an error, not silently render an empty page
func TestRender_UnknownTemplateReturnsError(t *testing.T) {
	if _, err := renderTo(t, "does-not-exist.html", nil); err == nil {
		t.Fatal("unknown template name should return an error")
	}
}

// The page still renders when there are no runners
func TestRender_EmptyRunnerListStillRenders(t *testing.T) {
	html, err := renderTo(t, "index.html", map[string]any{
		"Runners": []runner.RunnerInfo{},
		"Config":  &config.Config{},
		"T":       map[string]string{},
		"Lang":    "en",
		"TJSON":   "{}",
		"Version": "test",
	})
	if err != nil {
		t.Fatalf("rendering empty list failed: %v", err)
	}
	if !strings.Contains(html, "<html") {
		t.Fatal("render result does not look like an HTML page")
	}
}
