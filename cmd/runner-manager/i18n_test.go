package main

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func i18nLangs(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(i18nFS, "i18n")
	if err != nil {
		t.Fatal(err)
	}
	var langs []string
	for _, e := range entries {
		langs = append(langs, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(langs)
	if len(langs) < 2 {
		t.Fatalf("Embedded language file only has %v, so the embed declaration may not have taken effect", langs)
	}
	return langs
}

func TestLoadI18n_AllEmbeddedFilesParse(t *testing.T) {
	for _, lang := range i18nLangs(t) {
		m, err := loadI18n(lang)
		if err != nil {
			t.Errorf("%s.json failed to parse: %v", lang, err)
			continue
		}
		if len(m) == 0 {
			t.Errorf("%s.json has no keys at all", lang)
		}
	}
}

func TestLoadI18n_UnknownLangErrors(t *testing.T) {
	if _, err := loadI18n("does-not-exist"); err == nil {
		t.Fatal("Unknown language should error so the caller falls back to en")
	}
	// Path joining must not let .. escape the i18n directory
	if _, err := loadI18n("../go"); err == nil {
		t.Fatal("A language name containing .. must not read files outside i18n")
	}
}

// A missing key shows up as a blank spot in the UI without any error, so only the test cases can catch it.
// Forgetting a language when adding new copy is the most likely slip.
func TestLoadI18n_AllLanguagesHaveSameKeys(t *testing.T) {
	en, err := loadI18n("en")
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range i18nLangs(t) {
		if lang == "en" {
			continue
		}
		m, err := loadI18n(lang)
		if err != nil {
			t.Fatal(err)
		}
		var missing, extra []string
		for k := range en {
			if _, ok := m[k]; !ok {
				missing = append(missing, k)
			}
		}
		for k := range m {
			if _, ok := en[k]; !ok {
				extra = append(extra, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 {
			t.Errorf("%s.json is missing these keys relative to en.json: %v", lang, missing)
		}
		if len(extra) > 0 {
			t.Errorf("%s.json has keys that en.json does not: %v", lang, extra)
		}
	}
}

// An empty value is a blank spot, same as a missing one
func TestLoadI18n_NoBlankValues(t *testing.T) {
	for _, lang := range i18nLangs(t) {
		m, err := loadI18n(lang)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range m {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s.json has an empty value for %q", lang, k)
			}
		}
	}
}

var (
	tplKeyRe = regexp.MustCompile(`\{\{index \$?\.T "([^"]+)"\}\}`)
	// JS-side t('xxx.yyy'): require a dot so that things like createElement('div') are not mistaken for keys
	jsKeyRe = regexp.MustCompile(`\bt\('([a-z][a-z_]*\.[a-zA-Z_0-9]+)'\)`)
)

// A key referenced in the template but not defined renders as a blank spot, with no error at all.
//
// The script has been split out of index.html into static/app.js, so both must be scanned. Scanning only the template,
// A wrong key in app.js likewise produces no error, just a blank spot in the UI.
func TestTemplateKeysExistInI18n(t *testing.T) {
	tpl, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	en, err := loadI18n("en")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, m := range tplKeyRe.FindAllStringSubmatch(string(tpl), -1) {
		used[m[1]] = true
	}
	jsUsed := 0
	for _, m := range jsKeyRe.FindAllStringSubmatch(string(js), -1) {
		used[m[1]] = true
		jsUsed++
	}
	// The easiest slip after splitting out the script is leaving only the template here and forgetting to scan app.js too
	if jsUsed == 0 {
		t.Fatal("No i18n keys were extracted from static/app.js; the scan scope or regex may have broken")
	}
	if len(used) == 0 {
		t.Fatal("No i18n keys were extracted from the template; the regex may have broken")
	}
	var missing []string
	for k := range used {
		if _, ok := en[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("Template or script references keys that do not exist in en.json: %v", missing)
	}
}
