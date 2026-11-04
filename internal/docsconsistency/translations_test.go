package docsconsistency

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// docsConf holds the three docs-related settings in scripts/ci-recipes.conf.
//
// It reads from the same file instead of repeating the language list here: the ci-recipes heading check reads it,
// and the content check here reads it too, so adding a seventh language still means editing only that one file. Pinning a copy on each side is how
// the Makefile header comment already covers this: changing one copy and forgetting the other means local and CI results disagree.
type docsConf struct {
	root  string
	langs []string
	files []string
}

func loadDocsConf(t *testing.T, repo string) docsConf {
	t.Helper()
	f, err := os.Open(filepath.Join(repo, "scripts", "ci-recipes.conf"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	conf := docsConf{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "docs_root":
			conf.root = val
		case "docs_languages":
			conf.langs = append(conf.langs, strings.Fields(val)...)
		case "docs_files":
			conf.files = append(conf.files, strings.Fields(val)...)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if conf.root == "" || len(conf.langs) == 0 || len(conf.files) == 0 {
		t.Fatalf("docs_root/docs_languages/docs_files not found in ci-recipes.conf: %+v", conf)
	}
	return conf
}

// TestTranslations_SectionContentMatchesEnglish compares the amount of content in each section of the English original and the five translations.
//
// The existing ci-recipes check-docs-structure compares the sequence of heading levels, so it cannot see what is missing under a heading.
// This is not a hypothetical risk; it already happened. 075b612 added a row for DELETE /api/runners/:name to the HTTP API table in the English
// development.md and left every translation untouched; 797959a, as a catch-up, put back
// the 'Removing Runner from GitHub' section, because it is a heading and the structure check would fail;
// but the table row is not a heading, so it went missing from all five translations together, and the check still reported everything as consistent.
//
// It compares quantities, not text, since text is supposed to be translated. The four quantities each match one of the four kinds of drift that actually happened:
// a missing table row, a missing code block, a missing command inside a code block, and a missing list item.
func TestTranslations_SectionContentMatchesEnglish(t *testing.T) {
	repo := repoRoot(t)
	conf := loadDocsConf(t, repo)

	for _, file := range conf.files {
		enPath := filepath.Join(repo, conf.root, file)
		en := parseSections(t, enPath)

		for _, lang := range conf.langs {
			trPath := filepath.Join(repo, conf.root, lang, file)
			tr := parseSections(t, trPath)

			if len(en) != len(tr) {
				// Heading count mismatches are check-docs-structure's job; this note only explains why the comparison stops there.
				t.Errorf("%s/%s: %d sections, English has %d; get check-docs-structure passing first",
					lang, file, len(tr), len(en))
				continue
			}

			for i := range en {
				where := fmt.Sprintf("%s/%s 第 %d 节（英文 %s:%d）",
					lang, file, i, filepath.Join(conf.root, file), en[i].line)

				if en[i].tableRow != tr[i].tableRow {
					t.Errorf("%s: table has %d rows, English has %d", where, tr[i].tableRow, en[i].tableRow)
				}
				if en[i].bullets != tr[i].bullets {
					t.Errorf("%s: %d list items, English has %d", where, tr[i].bullets, en[i].bullets)
				}
				if len(en[i].blocks) != len(tr[i].blocks) {
					t.Errorf("%s: %d code blocks, English has %d", where, len(tr[i].blocks), len(en[i].blocks))
					continue
				}
				for b := range en[i].blocks {
					if en[i].blocks[b].lines != tr[i].blocks[b].lines {
						t.Errorf("%s: code block %d has %d command lines, English has %d",
							where, b+1, tr[i].blocks[b].lines, en[i].blocks[b].lines)
					}
					// Language tags should not be translated. A mismatch usually means one translation dropped a code block,
					// so every block after it shifts by one, yet a line-count-only comparison can still show both sides matching.
					if en[i].blocks[b].info != tr[i].blocks[b].info {
						t.Errorf("%s: code block %d is tagged ```%s, English is ```%s",
							where, b+1, tr[i].blocks[b].info, en[i].blocks[b].info)
					}
				}

				enLinks := normalizeLinks(repo, conf, en[i].links)
				trLinks := normalizeLinks(repo, conf, tr[i].links)
				if strings.Join(enLinks, " ") != strings.Join(trLinks, " ") {
					t.Errorf("%s: link target is %v, English is %v", where, trLinks, enLinks)
				}
			}
		}
	}
}

// normalizeLinks converts link targets into a form that can be compared across languages.
//
// Two cases must be normalized, or every result is a false positive: path depth (../examples and ../../examples point to the same place,
// already resolved by filepath.Join), and links where each language points to its own copy of the document,
// the footer of docs/zh/development.md points to docs/zh/README.md, while the English one points to docs/README.md,
// which is correct, not drift. After removing the language directory segment, the two are equal.
func normalizeLinks(repo string, conf docsConf, links []string) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		rel, err := filepath.Rel(repo, l)
		if err != nil {
			rel = l
		}
		rel = filepath.ToSlash(rel)
		for _, lang := range conf.langs {
			prefix := conf.root + "/" + lang
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				rel = conf.root + strings.TrimPrefix(rel, prefix)
				break
			}
		}
		out = append(out, strings.TrimSuffix(rel, "/"))
	}
	sort.Strings(out)
	return out
}
