package handler

import (
	"fmt"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
)

// Localization of server-side messages.
//
// The UI shell has supported six languages for a while, but the lines users actually read after an action -- the toast notices,
// and the errors on failure -- have always been Chinese string literals in the handler. Switch the UI to English and the shell is English,
// but every toast is still Chinese. The docs promise a six-language experience, but the product has so far only delivered on the shell.
//
// There is nothing new in the approach: resolveLang and I18nLoader already exist (the former picks the language for templates,
// the latter is injected by main from embed); this only wires them into the API response side.
//
// There are two reasons to translate on the **server** rather than return error codes for the browser to translate:
//   - The API's message field is still prose meant for humans. Returning "api.name_required" would leave
//     curl and CI scripts with a bare code, and those are exactly the callers with no translation table to look it up in.
//   - Detector's default language is en, so scripts without Accept-Language get English --
//     strictly better than getting Chinese today.
//
// Keys always start with api., separate from the UI's own msg.* keys: those are looked up by app.js,
// these are looked up here. The two sets have different lifecycles, and mixing them under one prefix would sooner or later lead someone to delete the wrong one.

// bundleCache caches "language -> translation table".
//
// I18nLoader re-reads the embedded file and calls json.Unmarshal on every call, and on the API side every
// request does a lookup -- the list page polls every 15 seconds, so re-parsing a JSON file each time is wasted work.
// The translation tables don't change for the life of the process (the content is compiled into the binary), so the cache never expires.
var bundleCache sync.Map // string -> map[string]string

func bundle(lang string) map[string]string {
	if v, ok := bundleCache.Load(lang); ok {
		m, _ := v.(map[string]string)
		return m
	}
	var m map[string]string
	if I18nLoader != nil {
		m, _ = I18nLoader(lang)
	}
	if m == nil {
		m = map[string]string{}
	}
	bundleCache.Store(lang, m)
	return m
}

// ResetI18nCache drops the cached translation tables; tests call it after replacing I18nLoader.
//
// It is exported because the cmd/runner-manager test cases also swap out I18nLoader: without clearing the cache,
// a table installed by an earlier case would be read by later cases, and that kind of cross-talk only shows up when the test order changes.
func ResetI18nCache() { bundleCache = sync.Map{} }

// tr returns the message for the key in the current request language.
//
// Three-level fallback: request language → English → the key itself. The last level deliberately does not fall back to Chinese: when a key is missing,
// a visible "api.start_failed" on the screen is an obvious defect, whereas a Chinese sentence would be taken as
// "this one isn't translated yet" and slip through in a UI that claims to support six languages.
func tr(c echo.Context, key string) string {
	if s, ok := lookup(c, key); ok {
		return s
	}
	return key
}

// trf is like tr, then fills the arguments with fmt.Sprintf.
//
// When the key is missing, Sprintf is **not** applied. The fallback value is the key itself, which contains no verbs, so Sprintf would append
// noise such as "api.start_failed%!(EXTRA string=new)" — which looks neither like a missing translation
// nor like an argument, so readers would think the program is broken. Using "key: args" keeps both facts,
// and makes it obvious that a translation entry is missing.
func trf(c echo.Context, key string, args ...any) string {
	if s, ok := lookup(c, key); ok {
		return fmt.Sprintf(s, args...)
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, key)
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	return strings.Join(parts, ": ")
}

// lookup: three-level fallback — request language → English → nothing.
func lookup(c echo.Context, key string) (string, bool) {
	lang := resolveLang(c)
	if s, ok := bundle(lang)[key]; ok && s != "" {
		return s, true
	}
	if lang != "en" {
		if s, ok := bundle("en")[key]; ok && s != "" {
			return s, true
		}
	}
	return "", false
}

// trEnf renders the English message for places without a request context, mainly logging.
//
// Logs are always in English by design: a log line has no request to follow, its readers are operators, and their tooling
// (grep, Loki queries, alert rules) is most hurt when the same thing appears in different languages. API responses follow the request language,
// while logs are always English; the readers and constraints of the two are simply different.
//
// When the key is missing, it falls back to "key: args" just like trf, without Sprintf.
func trEnf(key string, args ...any) string {
	if s, ok := bundle("en")[key]; ok && s != "" {
		return fmt.Sprintf(s, args...)
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, key)
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	return strings.Join(parts, ": ")
}

// Trf exposes trf to middleware outside the handler package.
//
// The CSRF guard sits in front of the routes; its code lives in cmd/runner-manager, but the 403 it returns travels the same path
// as handler errors into the UI toast. The UI is in French while this one is in Chinese, and readers will not
// assume "one string was missed in translation"; they will assume they hit a different service.
func Trf(c echo.Context, key string, args ...any) string { return trf(c, key, args...) }
