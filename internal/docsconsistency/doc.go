// Package docsconsistency holds only documentation consistency tests, with no runtime code.
//
// Why Go tests instead of a script: the repo just moved two shell checks into soulteary/ci-recipes
// (see docs/ci-recipes-migration.md). Adding shell back under scripts/ would undo that split.
// These checks don't belong in that project, though. They verify this repo's own documentation facts,
// not reusable recipes. Placing them as tests under internal/ means CI's
// `go test -race ./cmd/runner-manager/... ./internal/...` already runs them,
// with no new workflow and no drift between local and CI runs.
//
// They complement the two ci-recipes checks without overlapping:
//   - check-version-consistency: version numbers are consistent within the repo
//   - check-docs-structure: translated heading level sequences match the English
//   - this package's translations_test.go: content volume under headings (table rows, code blocks, list items, link targets)
//   - this package's pathrefs_test.go: repo paths referenced in non-Markdown files actually exist
//   - this package's examples_policy_test.go: the language policy for examples/ is a deliberate decision, not an oversight
package docsconsistency
