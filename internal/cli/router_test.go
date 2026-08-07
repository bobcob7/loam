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
	return NewDeps(testLogger(), &ConfigMock{}, &OutputEncoderMock{}, &ErrorMapperMock{}, &WorkspaceResolverMock{}, fakeConnect{}, nil, nil, nil)
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

// commandImplementationProofs names, for every dispatchable leaf command
// (see leafCommandKeys), the test that proves dispatching it reaches that
// command's real handler rather than a routing usageError.
//
// This replaces the pair of lists that used to live here -- a
// "stillStubbedExemptions" set and a reachability table of still-stubbed
// commands -- which had to be kept complementary by hand and which a merge
// had already once broken by unioning them wrongly. With loam-0pj.7 no leaf
// is stubbed any more (errNotImplemented is gone), so the table had no rows
// left to hold and the invariant collapses to a single exhaustive map: one
// entry per leaf, no more and no fewer, checked in both directions by
// TestCommandTree_EveryLeafHasAnImplementationProof. There is nothing left
// for two lists to disagree about.
//
// The values are documentation, not assertions -- Go cannot check that a
// named test exists -- but the KEYS are enforced exactly, so a new command
// added to commandTree() fails this file until someone writes its proof,
// and a command deleted from the tree fails it until its stale entry goes.
var commandImplementationProofs = map[string]string{
	"instructions": "TestRouterDispatch_Instructions_ReachesRealHandler (commands_root_test.go, loam-0pj.7)",
	"whoami":       "TestRouterDispatch_Whoami_ReachesRealHandler (commands_root_test.go, loam-0pj.7)",
	"clone":        "TestRouterDispatch_Clone_ReachesRealHandler (clone_test.go, loam-0pj.8)",

	"work start":          "TestRouterDispatch_WorkStartSetRequestReview_ReachRealHandlers (commands_work_test.go, loam-0pj.11)",
	"work set":            "TestRouterDispatch_WorkStartSetRequestReview_ReachRealHandlers (commands_work_test.go, loam-0pj.11)",
	"work request-review": "TestRouterDispatch_WorkStartSetRequestReview_ReachRealHandlers (commands_work_test.go, loam-0pj.11)",

	"work list":     "TestRouterDispatch_WorkReadCommands_ReachRealHandlers (commands_work_read_test.go, loam-0pj.10)",
	"work show":     "TestRouterDispatch_WorkReadCommands_ReachRealHandlers (commands_work_read_test.go, loam-0pj.10)",
	"work diff":     "TestRouterDispatch_WorkReadCommands_ReachRealHandlers (commands_work_read_test.go, loam-0pj.10)",
	"work comments": "TestRouterDispatch_WorkReadCommands_ReachRealHandlers (commands_work_read_test.go, loam-0pj.10)",
	"work verdicts": "TestRouterDispatch_WorkReadCommands_ReachRealHandlers (commands_work_read_test.go, loam-0pj.10)",

	"work comment": "TestRouterDispatch_WorkComment_ReachesRealHandler (commands_work_comment_test.go, loam-0pj.12)",
	"work reply":   "TestRouterDispatch_WorkReply_ReachesRealHandler (commands_work_reply_test.go, loam-0pj.13)",
	"work verdict": "TestRouterDispatch_WorkVerdict_ReachesRealHandler (commands_work_verdict_test.go, loam-0pj.13)",

	"graph def":        "TestRouterDispatch_GraphSubqueries_ReachRealHandlers (commands_graph_test.go, loam-0pj.14)",
	"graph refs":       "TestRouterDispatch_GraphSubqueries_ReachRealHandlers (commands_graph_test.go, loam-0pj.14)",
	"graph deps":       "TestRouterDispatch_GraphSubqueries_ReachRealHandlers (commands_graph_test.go, loam-0pj.14)",
	"graph dependents": "TestRouterDispatch_GraphSubqueries_ReachRealHandlers (commands_graph_test.go, loam-0pj.14)",
	"graph history":    "TestRouterDispatch_GraphSubqueries_ReachRealHandlers (commands_graph_test.go, loam-0pj.14)",

	"search": "TestRouterDispatch_Search_ReachesRealHandler (commands_search_test.go, loam-0pj.15)",
}

// leafCommandKeys walks tree and returns the set of dispatchable leaf
// command keys: a top-level leaf's own name, or "<group> <sub>" for a
// subcommand -- the same shape commandImplementationProofs is keyed by, so
// the two sets can be compared directly.
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

// TestCommandTree_EveryLeafHasAnImplementationProof is the drift check on
// commandImplementationProofs: it walks commandTree() itself and asserts
// the two sets are equal in BOTH directions, so neither adding a command
// without a proof nor removing one and leaving its entry behind can pass
// silently.
func TestCommandTree_EveryLeafHasAnImplementationProof(t *testing.T) {
	t.Parallel()
	leaves := leafCommandKeys(commandTree())
	for leaf := range leaves {
		assert.Contains(t, commandImplementationProofs, leaf,
			"command %q is dispatchable but has no entry in commandImplementationProofs naming the test that proves it reaches a real handler", leaf)
	}
	for leaf := range commandImplementationProofs {
		assert.Contains(t, leaves, leaf,
			"commandImplementationProofs names %q, which is no longer a dispatchable leaf in commandTree()", leaf)
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
