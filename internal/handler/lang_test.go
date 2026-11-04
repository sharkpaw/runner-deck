package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v4"
	i18n "github.com/soulteary/i18n-kit/v4"
)

func langCtx(t *testing.T, query, cookie, acceptLang, xLang string) echo.Context {
	t.Helper()
	target := "/"
	if query != "" {
		target += "?lang=" + url.QueryEscape(query)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "lang", Value: cookie})
	}
	if acceptLang != "" {
		req.Header.Set("Accept-Language", acceptLang)
	}
	if xLang != "" {
		req.Header.Set("X-Language", xLang)
	}
	return echo.New().NewContext(req, httptest.NewRecorder())
}

// i18n-kit knows 10 languages, but we only have 6 translations. init() narrows kit's SupportedLanguages
// to supportedLangs. This case guards exactly that narrowing: without it, ?lang=es would be accepted as a valid value,
// and the UI language would become es, which has no translation at all.
func TestSupportedLanguagesIsNarrowedToOurTranslations(t *testing.T) {
	if len(i18n.SupportedLanguages) != len(supportedLangs) {
		t.Fatalf("i18n.SupportedLanguages = %v, expected same length as supportedLangs %v",
			i18n.SupportedLanguages, supportedLangs)
	}
	for _, want := range supportedLangs {
		if !i18n.Language(want).IsValid() {
			t.Errorf("%q has a translation but IsValid rejects it", want)
		}
	}
	// Reverse: languages kit ships with but we have no translation for must be rejected
	for _, unsupported := range []string{"it", "es", "pt", "ru"} {
		if i18n.Language(unsupported).IsValid() {
			t.Errorf("%q has no translation and should not pass IsValid", unsupported)
		}
	}
}

// This is the reason for switching to i18n-kit: the old hand-written parser took the first language it recognized in order of appearance, ignoring q values entirely.
// Tested: origin/main's implementation returns "en" for the first line below, but the client clearly prefers de (q=0.9 > q=0.1).
func TestResolveLang_AcceptLanguageHonorsQuality(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"en;q=0.1,de;q=0.9", "de"}, // The old implementation returns "en", which is a bug
		{"de;q=0.9,en;q=0.1", "de"},
		{"ja;q=0.2,ko;q=0.8", "ko"},
		{"de-DE,de;q=0.9,en;q=0.8", "de"}, // No q is treated as 1.0, so it is still de
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			if got := resolveLang(langCtx(t, "", "", tc.header, "")); got != tc.want {
				t.Fatalf("resolveLang = %q, expected %q", got, tc.want)
			}
		})
	}
}

// The reason supportedAcceptLanguage exists, and the easiest one to lose during a refactor.
//
// Without filtering: kit's Accept-Language parsing sorts by q and returns the first language that ParseLanguage recognizes,
// but ParseLanguage looks up kit's internal alias table of 10 languages, not the narrowed SupportedLanguages.
// So "es,ja" parses to es, which IsValid rejects in Detect, and the whole chain falls back to en.
// Tested: origin/main returns "ja" for "es,ja", so this would be a real regression.
func TestResolveLang_UnsupportedAcceptLanguageFallsThroughToNext(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"首选无译文，取下一个有译文的", "es,ja", "ja"},
		{"多个无译文的都跳过", "es,it,pt,ko", "ko"},
		{"无译文者 q 更高也要跳过", "es;q=0.9,ja;q=0.1", "ja"},
		{"全都没有译文时回落英文", "es,it,pt,ru", "en"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLang(langCtx(t, "", "", tc.header, "")); got != tc.want {
				t.Fatalf("resolveLang(%q) = %q, expected %q", tc.header, got, tc.want)
			}
		})
	}
}

// Newly gained with kit: region variants in query / cookie are also normalized.
// The old implementation only split on "-" for values in Accept-Language, so ?lang=zh-CN was simply not recognized (tested: returns empty and keeps searching).
func TestResolveLang_NormalizesRegionalVariants(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		cookie       string
		want         string
		wasBrokenOld bool
	}{
		{name: "query 带地区", query: "zh-CN", want: "zh", wasBrokenOld: true},
		{name: "query 下划线写法", query: "zh_TW", want: "zh", wasBrokenOld: true},
		{name: "cookie 带地区", cookie: "ja-JP", want: "ja", wasBrokenOld: true},
		{name: "大小写不敏感", query: "ZH", want: "zh"},
		{name: "前后空白", query: "  ja  ", want: "ja"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLang(langCtx(t, tc.query, tc.cookie, "", "")); got != tc.want {
				t.Fatalf("resolveLang = %q, expected %q", got, tc.want)
			}
		})
	}
}

// Values without translations must be skipped like invalid values, not used as valid languages.
func TestResolveLang_UnsupportedValuesSkippedInEveryStep(t *testing.T) {
	if got := resolveLang(langCtx(t, "es", "ja", "", "")); got != "ja" {
		t.Fatalf("?lang=es should be skipped and cookie checked next, got %q", got)
	}
	if got := resolveLang(langCtx(t, "", "es", "ko", "")); got != "ko" {
		t.Fatalf("cookie=es should be skipped and Accept-Language checked next, got %q", got)
	}
	if got := resolveLang(langCtx(t, "es", "it", "pt", "")); got != "en" {
		t.Fatalf("when none of the three have translations it should fall back to en, got %q", got)
	}
}

// X-Language ranks after cookie and before Accept-Language. It is an extra tier that came along with the kit switch,
// so pin the order here to keep it from silently shifting when Priority changes later.
func TestResolveLang_XLanguageSitsBetweenCookieAndAcceptLanguage(t *testing.T) {
	if got := resolveLang(langCtx(t, "", "ja", "", "ko")); got != "ja" {
		t.Fatalf("cookie should take precedence over X-Language, got %q", got)
	}
	if got := resolveLang(langCtx(t, "", "", "de", "ko")); got != "ko" {
		t.Fatalf("X-Language should take precedence over Accept-Language, got %q", got)
	}
}

// resolveLang is read-only: a one-off ?lang= should not overwrite the browser's long-term preference.
// i18n-kit's httpadapter.Middleware (StdMiddleware in the root package before v4) is the one that writes based on config
// cookie-related. This case guards that we never use it.
func TestResolveLang_DoesNotWriteCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?lang=ja", nil)
	req.AddCookie(&http.Cookie{Name: "lang", Value: "zh"})
	c := echo.New().NewContext(req, rec)

	if got := resolveLang(c); got != "ja" {
		t.Fatalf("resolveLang = %q, expected ja", got)
	}
	if sc := rec.Result().Header.Values("Set-Cookie"); len(sc) != 0 {
		t.Fatalf("resolveLang should not write a cookie, but emitted %v", sc)
	}
}

func TestSupportedAcceptLanguage(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"ja", "ja"},
		{"es", ""},
		{"es,ja", "ja"},
		{"ja,es", "ja"},
		{"es;q=0.9,ja;q=0.1", "ja;q=0.1"}, // Keep the q values as-is and leave sorting to kit
		{"de-DE,de;q=0.9,en;q=0.8", "de-DE,de;q=0.9,en;q=0.8"},
		{"es,it,pt,ru", ""},
		{"  ja  ,  es  ", "ja"}, // Trim whitespace on both ends of the entry
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := supportedAcceptLanguage(tc.in); got != tc.want {
				t.Fatalf("supportedAcceptLanguage(%q) = %q, expected %q", tc.in, got, tc.want)
			}
		})
	}
}
