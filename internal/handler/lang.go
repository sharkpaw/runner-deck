package handler

import (
	"strings"

	"github.com/labstack/echo/v4"
	i18n "github.com/soulteary/i18n-kit/v4"
)

// langDetector chooses the UI language in this order: ?lang= > cookie > X-Language > Accept-Language.
//
// query comes before cookie on purpose. The cookie is the long-term preference on this browser (the language dropdown writes it,
// followed by a reload, and the URL carries no parameter). ?lang= is an explicit choice for this visit. If the order were reversed,
// a link with ?lang=ja sent to someone who once picked Chinese would still show Chinese. The parameter would fail for every
// person who has a saved preference, and that is exactly when it is most useful.
//
// It also deliberately does not write the cookie back. A one-off link parameter should not quietly change someone's long-term preference. Removing the parameter and refreshing
// should return them to the language they chose. So this uses only i18n-kit's Detector, not its
// httpadapter.Middleware (which writes cookies per SetCookie config and puts the language into context,
// neither of which we need). Before v4 it was StdMiddleware in the root package.
var langDetector = i18n.NewDetector(i18n.DetectorConfig{
	QueryParam: "lang",
	CookieName: "lang",
	Default:    i18n.LangEN,
})

// i18n-kit ships 10 languages (it/es/pt/ru extra), but we only have 6 translations.
// Detector decides whether a value is usable through Language.IsValid, which reads this table,
// so narrowing it to supportedLangs makes ?lang=es skipped like ?lang=xx, with the cookie checked next.
//
// This changes i18n-kit's package-level variable. This is an application, not a library, and only this place in the process uses i18n-kit,
// and the kit's own RegisterLanguage also modifies this table this way, so it is the configuration point it provides.
func init() {
	langs := make([]i18n.Language, 0, len(supportedLangs))
	for _, l := range supportedLangs {
		langs = append(langs, i18n.Language(l))
	}
	i18n.SupportedLanguages = langs
	// i18n-kit's alias table does not know Turkish, and without an alias the detector
	// treats "tr" as unrecognized even though it is in SupportedLanguages.
	i18n.AddLanguageAlias("tr", i18n.Language("tr"))
}

// echoLangSource adapts echo.Context to i18n.RequestSource (three value-getter methods).
//
// When v3 split Fiber out into fiberadapter, the root package still only knew net/http, so Echo had to plug in at this layer.
// v4 moved net/http into httpadapter too, so the root package now knows no framework and only accepts the RequestSource
// interface. The same three methods went from "working around the kit's intended usage" to the kit's official entry point
// for a third framework. No code changed; the reason did.
type echoLangSource struct{ c echo.Context }

func (s echoLangSource) Query(name string) string { return s.c.QueryParam(name) }

func (s echoLangSource) Cookie(name string) string {
	v, err := s.c.Cookie(name)
	if err != nil || v == nil {
		return ""
	}
	return v.Value
}

func (s echoLangSource) Header(name string) string {
	if name == "Accept-Language" {
		return supportedAcceptLanguage(s.c.Request().Header.Get(name))
	}
	return s.c.Request().Header.Get(name)
}

// supportedAcceptLanguage drops Accept-Language entries we have no translation for and keeps the rest, q values included.
//
// It must filter before handing off to Detector. Its parsing step sorts by q and returns the first
// **language ParseLanguage recognizes**. ParseLanguage checks the kit's internal alias table (10 languages),
// not the narrowed SupportedLanguages above. So "es,ja" would parse as es, get rejected by
// IsValid in Detect, and the whole chain would fall back to en, when the correct answer is ja.
//
// After es is filtered out, the kit's q sorting still applies. That is the reason to use it: the old hand-written parsing took
// the first recognized entry in order and ignored q, so "en;q=0.1,de;q=0.9" selected en.
func supportedAcceptLanguage(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.Split(header, ",")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		// Only the language code is used for the check. The original entry, q included, goes to the kit.
		code := strings.TrimSpace(part)
		if i := strings.Index(code, ";"); i >= 0 {
			code = strings.TrimSpace(code[:i])
		}
		if lang, ok := i18n.ParseLanguage(code); ok && lang.IsValid() {
			kept = append(kept, strings.TrimSpace(part))
		}
	}
	return strings.Join(kept, ",")
}

// resolveLang picks the UI language. Priority: ?lang= > cookie > X-Language > Accept-Language > en.
func resolveLang(c echo.Context) string {
	return langDetector.Detect(echoLangSource{c: c}).String()
}
