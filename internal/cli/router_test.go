package cli

import (
	"flag"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newTestDeps() *Deps {
	return NewDeps(testLogger(), &ConfigMock{}, &OutputEncoderMock{}, &ErrorMapperMock{}, &WorkspaceResolverMock{}, &NoopConnectClient{})
}

func TestRouterDispatch_NoArgs_ReturnsUsageError(t *testing.T) {
	t.Parallel()
	router := NewRouter(newTestDeps())
	err := router.Dispatch(t.Context(), nil)
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
}

func TestRouterDispatch_UnknownCommand_ReturnsUsageError(t *testing.T) {
	t.Parallel()
	router := NewRouter(newTestDeps())
	err := router.Dispatch(t.Context(), []string{"bogus"})
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
}

func TestRouterDispatch_UnknownSubcommand_ReturnsUsageError(t *testing.T) {
	t.Parallel()
	router := NewRouter(newTestDeps())
	err := router.Dispatch(t.Context(), []string{"work", "bogus"})
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
}

func TestRouterDispatch_GroupWithNoSubcommand_ReturnsUsageError(t *testing.T) {
	t.Parallel()
	router := NewRouter(newTestDeps())
	err := router.Dispatch(t.Context(), []string{"graph"})
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
}

// TestRouterDispatch_EveryCommandIsReachable proves every command named in
// docs/cli-spec.md (as corrected by loam-0pj.1's NOTES) is registered and
// dispatchable: given plausible args, each resolves to its stub handler and
// returns errNotImplemented rather than a routing usageError.
func TestRouterDispatch_EveryCommandIsReachable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"instructions", []string{"instructions"}},
		{"instructions with target", []string{"instructions", "work list"}},
		{"whoami", []string{"whoami"}},
		{"clone", []string{"clone", "acme/repo"}},
		{"clone with branch", []string{"clone", "acme/repo", "wb-1"}},
		{"work start", []string{"work", "start", "acme/repo"}},
		{"work start with from", []string{"work", "start", "acme/repo", "main"}},
		{"work set", []string{"work", "set", "acme/repo", "wb-1", "--title", "T"}},
		{"work request-review", []string{"work", "request-review", "acme/repo", "wb-1"}},
		{"work list", []string{"work", "list"}},
		{"work list with filters", []string{"work", "list", "--repo", "acme/repo", "--awaiting-review", "--limit", "5"}},
		{"work show", []string{"work", "show", "acme/repo", "wb-1"}},
		{"work diff", []string{"work", "diff", "acme/repo", "wb-1"}},
		{"work comments", []string{"work", "comments", "acme/repo", "wb-1"}},
		{"work comments staged", []string{"work", "comments", "acme/repo", "wb-1", "--staged"}},
		{"work verdicts", []string{"work", "verdicts", "acme/repo", "wb-1"}},
		{"work comment", []string{"work", "comment", "acme/repo", "wb-1", "--file", "a.go", "--line", "3"}},
		{"work reply", []string{"work", "reply", "acme/repo", "wb-1", "--thread", "t1"}},
		{"work verdict", []string{"work", "verdict", "acme/repo", "wb-1", "--outcome", "approve"}},
		{"graph def", []string{"graph", "def", "Symbol"}},
		{"graph refs", []string{"graph", "refs", "Symbol"}},
		{"graph deps", []string{"graph", "deps", "file.go"}},
		{"graph dependents", []string{"graph", "dependents", "file.go"}},
		{"graph history", []string{"graph", "history", "Symbol"}},
		{"graph def with file/limit", []string{"graph", "def", "Symbol", "--file", "a.go", "--limit", "5"}},
		{"search", []string{"search", "how does auth work"}},
		{"search with flags", []string{"search", "auth", "--repo", "acme/repo", "--limit", "3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router := NewRouter(newTestDeps())
			err := router.Dispatch(t.Context(), tt.args)
			assert.ErrorIs(t, err, errNotImplemented)
			var ue *usageError
			assert.NotErrorAs(t, err, &ue)
		})
	}
}

// TestCommandTree_RemovedCommandsAreAbsent guards the NOTES spec correction:
// commit and push must not appear in the tree.
func TestCommandTree_RemovedCommandsAreAbsent(t *testing.T) {
	t.Parallel()
	tree := commandTree()
	_, hasCommit := tree["commit"]
	_, hasPush := tree["push"]
	assert.False(t, hasCommit, "commit must be removed per NOTES spec correction")
	assert.False(t, hasPush, "push must be removed per NOTES spec correction")
}

// TestRouter_RegistersZeroGlobalFlags asserts no command handler defines a
// flag on the package-level flag.CommandLine set: every flag must live on a
// per-command flag.FlagSet instead (see docs/cli-spec.md -> Conventions:
// "the CLI has no global flags"). It compares the flag.CommandLine flag
// count before and after dispatch, rather than asserting it is exactly
// zero, because `go test` itself registers flags (-test.run, -test.v, ...)
// on that same global set.
func TestRouter_RegistersZeroGlobalFlags(t *testing.T) {
	t.Parallel()
	countFlags := func() int {
		count := 0
		flag.CommandLine.VisitAll(func(*flag.Flag) { count++ })
		return count
	}
	before := countFlags()
	router := NewRouter(newTestDeps())
	for _, args := range [][]string{
		{"work", "set", "a", "b", "--title", "T"},
		{"work", "list", "--limit", "5"},
		{"graph", "def", "Symbol", "--repo", "acme/repo"},
		{"search", "q", "--limit", "3"},
	} {
		err := router.Dispatch(t.Context(), args)
		assert.ErrorIs(t, err, errNotImplemented)
	}
	after := countFlags()
	require.Equal(t, before, after, "dispatching commands with per-command flags must not add flags to flag.CommandLine")
}
