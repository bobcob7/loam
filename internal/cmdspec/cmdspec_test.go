package cmdspec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCompose_OrdersPositionalsThenFlagsThenStdinNote pins Compose's
// ordering DIRECTLY, and exists because the cross-package check that
// otherwise covers it cannot.
//
// internal/cli/synopsis_test.go's TestSynopsis_CommandTreeMatchesMetaCatalog
// compares the CLI's composed synopsis against the catalog's -- but both
// sides are built by calling THIS function, so reordering it moves both
// answers together and the comparison still passes. The assertion and its
// subject share a code path; the test is self-referential with respect to
// ordering. Its one literal assertion does not save it either, since that
// is on a command with no stdin note, where the flags/note order cannot be
// observed at all.
//
// The ordering is not cosmetic. docs/cli-spec.md writes every synopsis as
// positionals, then flags, then a trailing parenthetical -- e.g. `loam work
// set [repo] [work-branch] [--title <title>]` (optional description read
// from stdin). Putting the note ahead of the flags makes the printed line
// read as though the note were another positional argument and leaves the
// flags dangling after prose, which is the un-copyable usage line
// loam-hi5o.4 exists to eliminate.
func TestCompose_OrdersPositionalsThenFlagsThenStdinNote(t *testing.T) {
	t.Parallel()
	got := Compose("[repo] [work-branch]", "[--title <title>]", "description optional on stdin")
	assert.Equal(t, "[repo] [work-branch] [--title <title>] (description optional on stdin)", got)
}

// TestCompose_OmitsEmptyPartsWithoutStrayGaps covers every combination of
// the three inputs being absent. Each is genuinely reachable: "work list"
// is flags-only (no positionals), most commands have no stdin note, and
// several have no flags. A naive concatenation passes the full case above
// and still emits doubled or leading spaces for these.
func TestCompose_OmitsEmptyPartsWithoutStrayGaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                       string
		synopsis, flags, stdinNote string
		want                       string
	}{
		{"all three", "<repo> <branch>", "[--verify]", "body on stdin", "<repo> <branch> [--verify] (body on stdin)"},
		{"positionals only", "[repo] [work-branch]", "", "", "[repo] [work-branch]"},
		{"flags only, no positionals", "", "[--limit <n>]", "", "[--limit <n>]"},
		{"stdin note only", "", "", "body on stdin", "(body on stdin)"},
		{"positionals and flags", "[repo]", "[--staged]", "", "[repo] [--staged]"},
		{"positionals and stdin note", "[repo]", "", "body on stdin", "[repo] (body on stdin)"},
		{"flags and stdin note", "", "[--list]", "body on stdin", "[--list] (body on stdin)"},
		{"nothing at all", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, Compose(tt.synopsis, tt.flags, tt.stdinNote))
		})
	}
}

// TestFlags_RequiredFlagsAreNotBracketed pins the one property of this
// package's text that internal/cli's drift test explicitly cannot check
// (see its doc comment): bracketing means optional, and a flag whose
// handler rejects its absence must not be written as optional.
//
// It is a whitelist rather than a rule derived from the code, because
// "required" here is enforced by a handler's own check after parsing and
// is not visible in any structure this package can read. That makes the
// list a hand-maintained fact -- so it names WHERE each one is enforced,
// so the next reader can re-verify rather than trust it.
func TestFlags_RequiredFlagsAreNotBracketed(t *testing.T) {
	t.Parallel()
	required := map[string]string{
		// command      // flag, and where its absence is rejected
		"work reply":   "--thread",  // internal/cli/commands_work_reply.go, `if *thread == ""`
		"work verdict": "--outcome", // internal/cli/commands_work_verdict.go, parseVerdictOutcome
	}
	for command, flag := range required {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			shape, ok := Flags[command]
			assert.True(t, ok, "%q has a required flag %s but no Flags entry", command, flag)
			assert.NotContains(t, shape, "["+flag, "%s is required for %q, so it must not be bracketed as optional", flag, command)
			assert.Contains(t, shape, flag, "%q must still document %s", command, flag)
		})
	}
}
