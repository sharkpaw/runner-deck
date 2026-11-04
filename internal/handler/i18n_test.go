package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// withI18n temporarily installs a translation table and returns a restore function.
//
// It doesn't use the real JSON: those files are embedded in cmd/runner-manager, which this package can't reach. Also, the tests need to assert
// which key this response used, not what text that key currently maps to. The latter would fail every time the copy changes.
func withI18n(m map[string]string) func() {
	prev := I18nLoader
	I18nLoader = func(string) (map[string]string, error) { return m, nil }
	ResetI18nCache()
	return func() {
		I18nLoader = prev
		ResetI18nCache()
	}
}

func trCtx(t *testing.T, lang string) echo.Context {
	t.Helper()
	req := httptest.NewRequest("GET", "/?lang="+lang, nil)
	return echo.New().NewContext(req, httptest.NewRecorder())
}

// When a key is missing, trf should not Sprintf the arguments into a string that has no verb.
//
// The first version did exactly that, and the result was "api.recreate_requires_registered%!(EXTRA string=new)"--
// It looked neither like a missing translation nor like an argument, so a reader would just assume the program was broken. Now it falls back to "key: args".
func TestTrf_MissingKeyDoesNotProduceSprintfNoise(t *testing.T) {
	defer withI18n(map[string]string{})()
	got := trf(trCtx(t, "en"), "api.nope", "new")
	if got != "api.nope: new" {
		t.Fatalf("When a key is missing, it should fall back to 'key: arg', got %q", got)
	}
	if want := "%!"; len(got) > 0 && contains(got, want) {
		t.Fatalf("When a key is missing, no Sprintf error marker should appear: %q", got)
	}
}

// Request language comes first, then English, and only last the key itself.
func TestTr_FallsBackThroughEnglishThenKey(t *testing.T) {
	defer withI18n(map[string]string{"api.only_en": "English text"})()
	if got := tr(trCtx(t, "ja"), "api.only_en"); got != "English text" {
		t.Fatalf("When a key is missing in this language, it should fall back to English, got %q", got)
	}
	if got := tr(trCtx(t, "ja"), "api.absent"); got != "api.absent" {
		t.Fatalf("When the key is missing in both languages, it should return the key itself, got %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
