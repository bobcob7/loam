package cli

import (
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
