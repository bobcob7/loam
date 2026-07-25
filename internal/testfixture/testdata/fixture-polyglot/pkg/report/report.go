// Package report renders validation results for the fixture repo.
package report

import "fixture/pkg/validate"

// Summarize returns a human-readable line describing whether s passed
// validation. It calls validate.Validate, giving cross-file, cross-package
// reference resolution something concrete to find within the Go fixture.
func Summarize(s string) string {
	if validate.Validate(s) {
		return "ok: " + s
	}
	return "invalid: " + s
}
