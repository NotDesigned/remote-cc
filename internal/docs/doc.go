// Package docs holds the documentation check (docs/documentation.md,
// principle 8): `go test ./internal/docs` checks the repository's Markdown
// files for broken links and anchors, files missing from README's document
// table, missing reader lines, forbidden terms, the principle 3, 4 and 6
// greps, and compares the commands and RCC_* variables they mention with
// the code.
package docs
