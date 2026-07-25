package cli

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseCommandArgs_FlagsAfterPositionals proves flags registered after
// positional arguments still parse, matching synopses like
// "work set [repo] [work-branch] [--title <title>]" from docs/cli-spec.md.
func TestParseCommandArgs_FlagsAfterPositionals(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("work set")
	title := fs.String("title", "", "")
	positional, err := parseCommandArgs(fs, []string{"acme/repo", "wb-1", "--title", "New Title"})
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/repo", "wb-1"}, positional)
	assert.Equal(t, "New Title", *title)
}

// TestParseCommandArgs_BoolFlagAfterPositionals proves a bool flag (which
// takes no separate value token) is handled correctly when mixed with
// positional arguments, per "work comments [repo] [work-branch] [--staged]".
func TestParseCommandArgs_BoolFlagAfterPositionals(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("work comments")
	staged := fs.Bool("staged", false, "")
	positional, err := parseCommandArgs(fs, []string{"acme/repo", "wb-1", "--staged"})
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/repo", "wb-1"}, positional)
	assert.True(t, *staged)
}

// TestParseCommandArgs_EqualsForm proves the "--flag=value" form works
// whether it precedes or follows positionals.
func TestParseCommandArgs_EqualsForm(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("search")
	limit := fs.Int("limit", 10, "")
	positional, err := parseCommandArgs(fs, []string{"my query", "--limit=3"})
	require.NoError(t, err)
	assert.Equal(t, []string{"my query"}, positional)
	assert.Equal(t, 3, *limit)
}

// TestParseCommandArgs_UnknownFlag_ReturnsError proves an unrecognized flag
// surfaces as a parse error, which command handlers turn into a usageError.
func TestParseCommandArgs_UnknownFlag_ReturnsError(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("whoami")
	_, err := parseCommandArgs(fs, []string{"--nonexistent"})
	assert.Error(t, err)
}

// TestParseCommandArgs_NoFlags_LeavesPositionalsInOrder proves a command
// with no registered flags still returns its positional args untouched.
func TestParseCommandArgs_NoFlags_LeavesPositionalsInOrder(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("clone")
	positional, err := parseCommandArgs(fs, []string{"acme/repo", "wb-1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/repo", "wb-1"}, positional)
}

// TestParseCommandArgs_DoubleDashBeforePositionals proves a leading "--"
// takes every following token as a literal positional, even one that
// starts with "-" and would otherwise be hoisted as a flag.
func TestParseCommandArgs_DoubleDashBeforePositionals(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("search")
	fs.Int("limit", 10, "")
	positional, err := parseCommandArgs(fs, []string{"--", "a", "b"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, positional)
}

// TestParseCommandArgs_DoubleDashAfterPositionals proves "--" appearing
// after an ordinary positional still terminates flag scanning for
// everything that follows, rather than letting a look-alike flag steal a
// positional's place.
func TestParseCommandArgs_DoubleDashAfterPositionals(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("search")
	limit := fs.Int("limit", 10, "")
	positional, err := parseCommandArgs(fs, []string{"q", "--", "--limit"})
	require.NoError(t, err)
	assert.Equal(t, []string{"q", "--limit"}, positional)
	assert.Equal(t, 10, *limit, "--limit after -- must stay a literal positional, not set the flag")
}

// TestParseCommandArgs_DoubleDashFollowedByDashLiteral proves the
// terminator protects a dash-prefixed literal that isn't a flag at all
// (e.g. a query string that happens to start with "-").
func TestParseCommandArgs_DoubleDashFollowedByDashLiteral(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("search")
	positional, err := parseCommandArgs(fs, []string{"--", "-weird-query"})
	require.NoError(t, err)
	assert.Equal(t, []string{"-weird-query"}, positional)
}

// TestParseCommandArgs_ValuelessTrailingFlag_ReturnsError proves a
// recognized non-bool flag with nothing after it is a usage error, never a
// silent reinterpretation of a preceding positional as its value (the
// dangerous failure mode: "work set acme/repo --title" must not turn
// "acme/repo" into the title).
func TestParseCommandArgs_ValuelessTrailingFlag_ReturnsError(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("work set")
	title := fs.String("title", "", "")
	positional, err := parseCommandArgs(fs, []string{"acme/repo", "--title"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flag needs an argument: -title")
	assert.Nil(t, positional)
	assert.Empty(t, *title)
}

// TestParseCommandArgs_FlagValueLooksLikeFlag proves a flag's value is
// always the literal next token, even if it starts with "-".
func TestParseCommandArgs_FlagValueLooksLikeFlag(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("work set")
	title := fs.String("title", "", "")
	positional, err := parseCommandArgs(fs, []string{"--title", "--foo"})
	require.NoError(t, err)
	assert.Empty(t, positional)
	assert.Equal(t, "--foo", *title)
}

// TestParseCommandArgs_RepeatedFlags_LastWins proves repeating a flag
// keeps the stdlib "last occurrence wins" contract.
func TestParseCommandArgs_RepeatedFlags_LastWins(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("work set")
	title := fs.String("title", "", "")
	_, err := parseCommandArgs(fs, []string{"--title", "A", "--title", "B"})
	require.NoError(t, err)
	assert.Equal(t, "B", *title)
}

// flagExpectation is one flag's expected name and default value, compared
// against flag.Flag.DefValue (the string form flag.FlagSet stores).
type flagExpectation struct {
	name   string
	defVal string
}

// TestCommandFlagSets_NamesAndDefaults pins every flagged command's
// registered flag names and default values against a fresh flag.FlagSet
// built by production code — not a hand-maintained mirror — so a drift in
// either direction (renamed flag, changed default) fails here. This is the
// highest-value place to catch it given this bead's stale-spec history.
func TestCommandFlagSets_NamesAndDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func() *flag.FlagSet
		want  []flagExpectation
	}{
		{"work list", newWorkListFlags, []flagExpectation{
			{"repo", ""}, {"author", ""}, {"target", ""},
			{"awaiting-review", "false"}, {"state", "reviewable"}, {"limit", "100"},
		}},
		{"work set", newWorkSetFlags, []flagExpectation{{"title", ""}}},
		{"work comments", newWorkCommentsFlags, []flagExpectation{{"staged", "false"}}},
		{"work comment", newWorkCommentFlags, []flagExpectation{
			{"file", ""}, {"line", "0"}, {"resolve", ""}, {"edit", ""}, {"discard", ""},
		}},
		{"work reply", func() *flag.FlagSet { fs, _ := newWorkReplyFlags(); return fs }, []flagExpectation{{"thread", ""}}},
		{"work verdict", func() *flag.FlagSet { fs, _ := newWorkVerdictFlags(); return fs }, []flagExpectation{{"outcome", ""}}},
		{"graph def", func() *flag.FlagSet { fs, _, _ := newGraphQueryFlags("graph def"); return fs }, []flagExpectation{
			{"repo", ""}, {"all", "false"}, {"file", ""}, {"limit", "50"},
		}},
		{"graph refs", func() *flag.FlagSet { fs, _, _ := newGraphQueryFlags("graph refs"); return fs }, []flagExpectation{
			{"repo", ""}, {"all", "false"}, {"file", ""}, {"limit", "50"},
		}},
		{"search", func() *flag.FlagSet { fs, _, _ := newSearchFlags(); return fs }, []flagExpectation{
			{"repo", ""}, {"all", "false"}, {"limit", "10"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := tt.build()
			got := map[string]string{}
			fs.VisitAll(func(f *flag.Flag) { got[f.Name] = f.DefValue })
			want := map[string]string{}
			for _, w := range tt.want {
				want[w.name] = w.defVal
			}
			assert.Equal(t, want, got)
		})
	}
}
