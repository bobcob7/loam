package cli

import (
	"flag"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeConnect is a trivial ConnectClient test double. It is declared here
// (not as a package stub) precisely so deleting internal/cli's placeholder
// collaborators later never breaks these tests.
type fakeConnect struct{}

func newTestDeps() *Deps {
	return NewDeps(testLogger(), &ConfigMock{}, &OutputEncoderMock{}, &ErrorMapperMock{}, &WorkspaceResolverMock{}, fakeConnect{})
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
// docs/cli-spec.md is registered and dispatchable: given plausible args,
// each resolves to its stub handler and returns errNotImplemented rather
// than a routing usageError.
func TestRouterDispatch_EveryCommandIsReachable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"instructions", []string{"instructions"}},
		{"instructions with target", []string{"instructions", "work list"}},
		{"whoami", []string{"whoami"}},
		{"clone", []string{"clone", "acme/repo", "wb-1"}},
		{"work start", []string{"work", "start", "acme/repo", "main"}},
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

// TestCommandTree_ExactCommandSet pins the exact command surface: every
// top-level command, every work subcommand, and every graph subquery,
// named exactly (not just "commit/push absent"). A drift in either
// direction — a missing command or a stray extra one — fails this test.
func TestCommandTree_ExactCommandSet(t *testing.T) {
	t.Parallel()
	tree := commandTree()
	assert.ElementsMatch(t, []string{"instructions", "whoami", "clone", "work", "graph", "search"}, keysOf(tree))
	require.Contains(t, tree, "work")
	require.NotNil(t, tree["work"].subcommands)
	assert.ElementsMatch(t, []string{
		"start", "set", "request-review", "list", "show", "diff",
		"comments", "verdicts", "comment", "reply", "verdict",
	}, keysOf(tree["work"].subcommands))
	require.Contains(t, tree, "graph")
	require.NotNil(t, tree["graph"].subcommands)
	assert.ElementsMatch(t, []string{"def", "refs", "deps", "dependents", "history"}, keysOf(tree["graph"].subcommands))
	require.Contains(t, tree, "search")
	assert.Nil(t, tree["search"].subcommands)
}

func keysOf(m map[string]*command) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestRouter_RegistersZeroGlobalFlags asserts no command handler defines a
// flag on the package-level flag.CommandLine set: every flag must live on a
// per-command flag.FlagSet instead (see docs/cli-spec.md -> Conventions:
// "the CLI has no global flags"). It asserts every flag on
// flag.CommandLine carries the "test." prefix go test itself registers
// (-test.run, -test.v, ...) — a before/after count would miss a
// package-level `var _ = flag.Bool(...)` registered at init time, since
// that flag would already be present in "before".
func TestRouter_RegistersZeroGlobalFlags(t *testing.T) {
	t.Parallel()
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
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		assert.True(t, strings.HasPrefix(f.Name, "test."), "flag.CommandLine must contain only go test's own flags, found %q", f.Name)
	})
}
