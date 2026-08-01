package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTryHelp_NeverReadsEnvironment proves TryHelp needs no LOAM_*
// configuration at all (loam-dc2v, loam-q0ek): every documented help route
// resolves purely from args, so it must behave identically regardless of
// what is (or is not) set in the environment. Unsetting every LOAM_*
// variable here and still getting a full, correct help response is the
// point -- this is deliberately NOT using t.Setenv to inject valid values,
// the way nearly every other test in this package does.
func TestTryHelp_NeverReadsEnvironment(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	for _, name := range []string{envServerURL, envAgentName, envAgentID, envAgentRole, envOutputFormat} {
		t.Setenv(name, "")
	}
	text, ok := TryHelp([]string{"whoami", "--help"})
	require.True(t, ok)
	assert.Contains(t, text, "whoami")
}

func TestTryHelp_EmptyArgs_IsNotAHelpRoute(t *testing.T) {
	t.Parallel()
	_, ok := TryHelp(nil)
	assert.False(t, ok)
}

func TestTryHelp_BareHelp_ListsEveryTopLevelCommand(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"help"})
	require.True(t, ok)
	for _, name := range []string{"instructions", "whoami", "clone", "work", "graph", "search"} {
		assert.Contains(t, text, name)
	}
	assert.Contains(t, text, "loam instructions", "the top-level listing must name instructions as the role-filtered authority")
}

func TestTryHelp_DoubleDashHelp_TopLevel_ListsEveryCommand(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"--help"})
	require.True(t, ok)
	assert.Contains(t, text, "whoami")
	assert.Contains(t, text, "work")
}

func TestTryHelp_ShortDashH_TopLevel_ListsEveryCommand(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"-h"})
	require.True(t, ok)
	assert.Contains(t, text, "whoami")
}

// TestTryHelp_LeafCommand_Help_RendersFlagsFromRealFlagSet proves a leaf's
// --help renders actual flag usage (docs/cli-spec.md option (c): "flag
// usage help from the pflag FlagSet"), not just a bare summary line --
// e.g. `loam work set --title <title>`'s registered --title flag must
// appear, with its help text, exactly as the real command would parse it.
func TestTryHelp_LeafCommand_Help_RendersFlagsFromRealFlagSet(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"work", "set", "--help"})
	require.True(t, ok)
	assert.Contains(t, text, "work set")
	assert.Contains(t, text, "--title")
	assert.Contains(t, text, "new title for the work branch")
}

// TestTryHelp_LeafCommand_ShortDashH_AnywhereAmongArgs proves -h is
// recognized wherever it appears, matching pflag's own interspersed
// flag/positional convention (docs/cli-spec.md -> Argument Ordering) --
// not just as the very next token after the command name.
func TestTryHelp_LeafCommand_ShortDashH_AnywhereAmongArgs(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"work", "verdict", "acme/repo", "wb-1", "-h"})
	require.True(t, ok)
	assert.Contains(t, text, "work verdict")
	assert.Contains(t, text, "--outcome")
}

// TestTryHelp_TopLevelLeaf_Help_NoFlags_StillRendersUsage covers a leaf
// with no flags of its own (clone): help must still succeed, just without
// a "Flags:" section.
func TestTryHelp_TopLevelLeaf_Help_NoFlags_StillRendersUsage(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"clone", "--help"})
	require.True(t, ok)
	assert.Contains(t, text, "loam clone")
	assert.NotContains(t, text, "Flags:")
}

// TestTryHelp_GroupHelp_ListsSubcommands proves `loam work --help` (the
// group itself, no subcommand chosen yet) lists work's subcommands rather
// than falling through to Dispatch's "work requires a subcommand" usage
// error.
func TestTryHelp_GroupHelp_ListsSubcommands(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"work", "--help"})
	require.True(t, ok)
	for _, sub := range []string{"start", "set", "request-review", "list", "show", "diff", "comments", "verdicts", "comment", "reply", "verdict"} {
		assert.Contains(t, text, sub)
	}
}

func TestTryHelp_GroupHelp_BareHelpWord(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"graph", "help"})
	require.True(t, ok)
	assert.Contains(t, text, "def")
	assert.Contains(t, text, "refs")
}

// TestTryHelp_GroupWithNoSubcommandAndNoHelpToken_IsNotAHelpRoute proves a
// bare `loam work` (no subcommand, no help flag) is left alone: it must
// stay Dispatch's existing "work requires a subcommand" usage error
// (router_test.go's TestRouterDispatch_GroupWithNoSubcommand_ReturnsUsageError),
// not silently become a help listing.
func TestTryHelp_GroupWithNoSubcommandAndNoHelpToken_IsNotAHelpRoute(t *testing.T) {
	t.Parallel()
	_, ok := TryHelp([]string{"work"})
	assert.False(t, ok, "bare `work` with no subcommand must not be treated as a help request")
}

// TestTryHelp_UnknownTopLevelCommand_IsNotAHelpRoute proves an unrecognized
// command name (even with --help trailing) is left to normal dispatch,
// which will report "unknown command" the usual way.
func TestTryHelp_UnknownTopLevelCommand_IsNotAHelpRoute(t *testing.T) {
	t.Parallel()
	_, ok := TryHelp([]string{"bogus", "--help"})
	assert.False(t, ok)
}

// TestTryHelp_UnknownSubcommand_IsNotAHelpRoute mirrors the top-level case
// one level down.
func TestTryHelp_UnknownSubcommand_IsNotAHelpRoute(t *testing.T) {
	t.Parallel()
	_, ok := TryHelp([]string{"work", "bogus", "--help"})
	assert.False(t, ok)
}

// TestTryHelp_EveryLeaf_RendersWithoutPanicking walks every leaf in
// commandTree() (the same exhaustive set router_test.go's
// TestCommandTree_EveryLeafHasAnImplementationProof pins) and asserts its
// --help renders successfully. This is the drift check for router.go's
// newFlags wiring: a leaf added to commandTree() without a newFlags
// constructor would nil-panic here instead of failing a plain assertion.
func TestTryHelp_EveryLeaf_RendersWithoutPanicking(t *testing.T) {
	t.Parallel()
	tree := commandTree()
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			text, ok := TryHelp([]string{name, "--help"})
			require.True(t, ok, "command %q", name)
			assert.Contains(t, text, name)
			continue
		}
		for sub := range cmd.subcommands {
			text, ok := TryHelp([]string{name, sub, "--help"})
			require.True(t, ok, "command %q %q", name, sub)
			assert.Contains(t, text, sub)
		}
	}
}
