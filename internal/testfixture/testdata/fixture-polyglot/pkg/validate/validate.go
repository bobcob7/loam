// Package validate provides input validation for the fixture repo.
package validate

import "strings"

// Validate reports whether s is a non-empty, trimmed string.
//
// It exists in this fixture specifically to collide in name with
// src/validate.ts's exported Validate: the ambiguous symbol that
// edge-resolution tests use to prove name-based matching stays intra-language
// and does not merge the Go and TypeScript hits together.
func Validate(s string) bool {
	return strings.TrimSpace(s) != ""
}
