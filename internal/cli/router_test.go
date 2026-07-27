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

// fakeConnect is a trivial ConnectClient test double for tests that only
// need a valid collaborator to inject, never an RPC call to actually
// happen: every accessor returns nil, which is fine for WorkBranchClient
// etc. since nothing here calls through them. It is declared here (not as a
// package stub) precisely so deleting internal/cli's placeholder
// collaborators (loam-qdr) never broke these tests.
type fakeConnect struct{}

func (fakeConnect) WorkBranch() WorkBranchClient { return nil }
func (fakeConnect) Repo() RepoClient             { return nil }
func (fakeConnect) Graph() GraphClient           { return nil }
func (fakeConnect) Search() SearchClient         { return nil }
func (fakeConnect) Meta() MetaClient             { return nil }

func newTestDeps() *Deps {
	return NewDeps(testLogger(), &ConfigMock{}, &OutputEncoderMock{}, &ErrorMapperMock{}, &WorkspaceResolverMock{}, fakeConnect{}, nil, nil)
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

// stillStubbedExemptions lists dispatchable leaf commands (see
// leafCommandKeys) deliberately absent from
// TestRouterDispatch_EveryCommandIsReachable's table because they no longer
// return errNotImplemented: a real handler exists, and that command's own
// test file proves it is reachable instead. clone is entry #1 -- covered by
// TestRouterDispatch_Clone_ReachesRealHandler in clone_test.go (loam-0pj.8).
// As each future command bead lands, add its leaf key here rather than
// deleting its row from the table silently: this keeps the coverage claim
// enforced by TestRouterDispatch_EveryCommandIsReachable's drift check
// below instead of by a reviewer noticing a table shrank.
var stillStubbedExemptions = map[string]bool{
	"clone": true,
	// work start/set/request-review are covered by
	// TestRouterDispatch_WorkStartSetRequestReview_ReachRealHandlers in
	// commands_work_test.go (loam-0pj.11).
	"work start":          true,
	"work set":            true,
	"work request-review": true,
	// work comment is covered by TestRouterDispatch_WorkComment_
	// ReachesRealHandler in commands_work_comment_test.go (loam-0pj.12).
	"work comment": true,
	// work reply/verdict are covered by TestRouterDispatch_WorkReply_
	// ReachesRealHandler and TestRouterDispatch_WorkVerdict_
	// ReachesRealHandler (loam-0pj.13).
	"work reply":   true,
	"work verdict": true,
	// work list/show/diff/comments/verdicts are covered by
	// TestRouterDispatch_WorkReadCommands_ReachRealHandlers in
	// commands_work_read_test.go (loam-0pj.10).
	"work list":     true,
	"work show":     true,
	"work diff":     true,
	"work comments": true,
	"work verdicts": true,
	// graph def/refs/deps/dependents/history are covered by
	// TestRouterDispatch_GraphSubqueries_ReachRealHandlers in
	// commands_graph_test.go (loam-0pj.14).
	"graph def":        true,
	"graph refs":       true,
	"graph deps":       true,
	"graph dependents": true,
	"graph history":    true,
	// search is covered by TestRouterDispatch_Search_ReachesRealHandler in
	// commands_search_test.go (loam-0pj.15).
	"search": true,
}

// leafCommandKeys walks tree and returns the set of dispatchable leaf
// command keys: a top-level leaf's own name, or "<group> <sub>" for a
// subcommand -- the same shape leafKeyFromArgs derives from a reachability
// table row's args, so the two can be compared directly.
func leafCommandKeys(tree map[string]*command) map[string]bool {
	leaves := make(map[string]bool)
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			leaves[name] = true
			continue
		}
		for sub := range cmd.subcommands {
			leaves[name+" "+sub] = true
		}
	}
	return leaves
}

// leafKeyFromArgs maps a reachability-table row's args to the same
// "<top>"/"<top> <sub>" key leafCommandKeys produces. Only "work" and
// "graph" are groups in the current command tree; every other top-level
// command is itself a leaf.
func leafKeyFromArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	top := args[0]
	if top != "work" && top != "graph" || len(args) < 2 {
		return top
	}
	return top + " " + args[1]
}

// TestRouterDispatch_EveryCommandIsReachable proves every still-stubbed
// command named in docs/cli-spec.md is registered and dispatchable: given
// plausible args, each resolves to its stub handler and returns
// errNotImplemented rather than a routing usageError. This is
// self-enforcing, not just a fixed list: it walks commandTree() itself and
// fails if any leaf command has neither a row below nor an entry in
// stillStubbedExemptions, so the next ~24 command beads (each of which
// turns one more row real, exactly as loam-0pj.8 did for clone) cannot
// silently shrink this test's coverage by deleting a row -- the leaf must
// be exempted explicitly instead.
func TestRouterDispatch_EveryCommandIsReachable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"instructions", []string{"instructions"}},
		{"instructions with target", []string{"instructions", "work list"}},
		{"whoami", []string{"whoami"}},
	}

	covered := make(map[string]bool, len(tests))
	for _, tt := range tests {
		covered[leafKeyFromArgs(tt.args)] = true
	}
	for leaf := range leafCommandKeys(commandTree()) {
		if stillStubbedExemptions[leaf] {
			continue
		}
		assert.True(t, covered[leaf], "command %q has no row in the reachability table above and no entry in stillStubbedExemptions", leaf)
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
	// `work comment` is implemented (loam-0pj.12), so it is dispatched with
	// an extra positional: its flags are still registered and parsed, but it
	// stops on the argument count before reading stdin or opening a staging
	// area, neither of which newTestDeps provides.
	// `work reply` and `work verdict` (loam-0pj.13) and the five read
	// commands (loam-0pj.10) are implemented too, and get the same
	// treatment for the same reason. As of those three beads no `work`
	// leaf returns errNotImplemented any more, so the loop that used to
	// assert that is gone rather than left iterating an empty list.
	for _, args := range [][]string{
		{"work", "comment", "a", "b", "c", "--file", "x.go", "--line", "3"},
		{"work", "reply", "a", "b", "c", "--thread", "t1"},
		{"work", "verdict", "a", "b", "c", "--outcome", "approve"},
		{"work", "comments", "a", "b", "c", "--staged"},
		{"work", "list", "extra", "--limit", "5"},
	} {
		var usage *usageError
		assert.ErrorAs(t, router.Dispatch(t.Context(), args), &usage, "args %v", args)
	}
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		assert.True(t, strings.HasPrefix(f.Name, "test."), "flag.CommandLine must contain only go test's own flags, found %q", f.Name)
	})
}
