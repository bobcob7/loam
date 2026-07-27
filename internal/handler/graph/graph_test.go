package graph_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/codegraph"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/graph"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/reposstore"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func agentCtx(t *testing.T, role string) context.Context {
	t.Helper()
	return httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: role})
}

// fakeRoleStore and fakeScopeStore are hand-written handler.RoleStore/
// handler.ScopeStore fakes, since internal/handler's moq-generated mocks
// live in its own package's moq_test.go and are unreachable from this
// external test package (graph_test only imports handler as a normal,
// non-test dependency) -- the same convention internal/handler/repo's own
// repo_test.go establishes.
type fakeRoleStore struct {
	capabilities []handler.Capability
}

func (s fakeRoleStore) RoleCapabilities(context.Context, string) ([]handler.Capability, error) {
	return s.capabilities, nil
}

type fakeScopeStore struct {
	getRepoByName      func(ctx context.Context, name string) (reposstore.Repo, error)
	listAllRepoNames   func(ctx context.Context) ([]string, error)
	listTargetBranches func(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error)
}

func (s fakeScopeStore) GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error) {
	return s.getRepoByName(ctx, name)
}

func (s fakeScopeStore) ListAllRepoNames(ctx context.Context) ([]string, error) {
	return s.listAllRepoNames(ctx)
}

func (s fakeScopeStore) ListTargetBranches(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error) {
	return s.listTargetBranches(ctx, repoID)
}

// oneRepoScope builds a fakeScopeStore resolving to exactly one enrolled
// repo, so most tests below can exercise Query's query-kind logic without
// also exercising ScopeResolver's own fan-out (already covered by
// internal/handler's own scope_test.go).
func oneRepoScope(repoID uuid.UUID, name, branch string) fakeScopeStore {
	return fakeScopeStore{
		getRepoByName: func(_ context.Context, n string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID, Name: n, IndexedBranch: branch}, nil
		},
		listAllRepoNames: func(context.Context) ([]string, error) { return []string{name}, nil },
		listTargetBranches: func(context.Context, uuid.UUID) ([]reposstore.TargetBranch, error) {
			return []reposstore.TargetBranch{{RepoID: repoID, Branch: branch}}, nil
		},
	}
}

// newHandler wires a graph.Handler over symbols with a capability checker
// granting every role roleCaps, an admin-bypassable capability gate, and an
// ErrorMapper logging to buf.
func newHandler(t *testing.T, symbols graph.SymbolStore, scopeStore handler.ScopeStore, roleCaps []handler.Capability, buf *bytes.Buffer) *graph.Handler {
	t.Helper()
	checker := handler.NewCapabilityChecker(fakeRoleStore{capabilities: roleCaps})
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(buf, nil)))
	scope := handler.NewScopeResolver(scopeStore)
	return graph.New(symbols, scope, checker, mapper, testLogger())
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	return connectErr.Code()
}

func definitionRequest(symbol string) *connect.Request[loamv1.QueryRequest] {
	return connect.NewRequest(&loamv1.QueryRequest{Query: &loamv1.QueryRequest_Definition{Definition: &loamv1.DefinitionQuery{Symbol: symbol}}})
}

func referencesRequest(symbol string) *connect.Request[loamv1.QueryRequest] {
	return connect.NewRequest(&loamv1.QueryRequest{Query: &loamv1.QueryRequest_References{References: &loamv1.ReferencesQuery{Symbol: symbol}}})
}

func dependenciesRequest(target string) *connect.Request[loamv1.QueryRequest] {
	return connect.NewRequest(&loamv1.QueryRequest{Query: &loamv1.QueryRequest_Dependencies{Dependencies: &loamv1.DependenciesQuery{Target: target}}})
}

func dependentsRequest(target string) *connect.Request[loamv1.QueryRequest] {
	return connect.NewRequest(&loamv1.QueryRequest{Query: &loamv1.QueryRequest_Dependents{Dependents: &loamv1.DependentsQuery{Target: target}}})
}

func historyRequest(symbol string) *connect.Request[loamv1.QueryRequest] {
	return connect.NewRequest(&loamv1.QueryRequest{Query: &loamv1.QueryRequest_History{History: &loamv1.HistoryQuery{Symbol: symbol}}})
}

// TestQuery_AgentLackingGraphQuery_Denied proves the capability gate runs
// before any store call, for every query kind, not just one.
func TestQuery_AgentLackingGraphQuery_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	storeCalled := false
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			storeCalled = true
			return nil, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.Query(agentCtx(t, "reviewer-without-graph"), definitionRequest("Login"))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.False(t, storeCalled, "the symbol store must not be consulted when the capability gate denies the caller")
}

// TestQuery_Definition_NotFound proves an empty LookupSymbolsByName result
// is exit 3 (handler.ErrNotFound / CodeNotFound) for `graph def` -- the
// store's own documented authoritative not-found signal.
func TestQuery_Definition_NotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return nil, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	_, err := h.Query(agentCtx(t, "author"), definitionRequest("DoesNotExist"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
}

// TestQuery_Definition_Success_Unambiguous proves a single match converts
// to one Location with no `of` disambiguation field.
func TestQuery_Definition_Success_Unambiguous(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	line := int32(42)
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(_ context.Context, repoIDs []uuid.UUID, branch, name, file string, limit int32) ([]codegraph.Symbol, bool, error) {
			assert.Equal(t, []uuid.UUID{repoID}, repoIDs)
			assert.Equal(t, "main", branch)
			assert.Equal(t, "Login", name)
			return []codegraph.Symbol{{ID: uuid.New(), RepoID: repoID, TargetBranch: "main", File: "auth.go", Line: &line, Name: "Login", Kind: "function"}}, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	resp, err := h.Query(agentCtx(t, "author"), definitionRequest("Login"))
	require.NoError(t, err)
	locations := resp.Msg.GetLocations().GetLocations()
	require.Len(t, locations, 1)
	loc := locations[0]
	assert.Equal(t, "bobcob7/doc-server", loc.GetRepo())
	assert.Equal(t, "auth.go", loc.GetFileLine().GetFile())
	assert.EqualValues(t, 42, loc.GetFileLine().GetLine())
	assert.Equal(t, "Login", loc.GetSymbol())
	assert.Equal(t, "function", loc.GetKind())
	assert.Nil(t, loc.Of, "an unambiguous match must not carry an `of` disambiguation field")
	assert.False(t, resp.Msg.GetTruncated())
	require.Len(t, resp.Msg.GetIngested(), 1)
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetIngested()[0].GetRepo())
}

// TestQuery_Definition_Ambiguous_EachRowNamesItsMatch proves several
// distinct symbols sharing a name are all returned (data, not an error,
// docs/cli-spec.md:528-533) and each carries `of`.
func TestQuery_Definition_Ambiguous_EachRowNamesItsMatch(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return []codegraph.Symbol{
				{ID: uuid.New(), RepoID: repoID, File: "auth.go", Name: "Login", Kind: "function"},
				{ID: uuid.New(), RepoID: repoID, File: "admin.go", Name: "Login", Kind: "method"},
			}, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	resp, err := h.Query(agentCtx(t, "author"), definitionRequest("Login"))
	require.NoError(t, err)
	locations := resp.Msg.GetLocations().GetLocations()
	require.Len(t, locations, 2)
	for _, loc := range locations {
		require.NotNil(t, loc.Of, "an ambiguous match must carry `of`")
		assert.Equal(t, "Login", loc.Of.GetSymbol())
	}
	assert.Equal(t, "auth.go", locations[0].Of.GetFile())
	assert.Equal(t, "admin.go", locations[1].Of.GetFile())
}

// TestQuery_References_SymbolNotFound_LookupReferencesNeverCalled is the
// two-step composition's not-found half: when LookupSymbolsByName finds
// nothing, `graph refs` is exit 3 and LookupReferencesByName must never be
// called at all.
func TestQuery_References_SymbolNotFound_LookupReferencesNeverCalled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	refsCalled := false
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return nil, false, nil
		},
		LookupReferencesByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Reference, bool, error) {
			refsCalled = true
			return nil, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	_, err := h.Query(agentCtx(t, "author"), referencesRequest("DoesNotExist"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
	assert.False(t, refsCalled, "LookupReferencesByName must not be called once existence already failed")
}

// TestQuery_References_DefinedSymbolWithZeroReferences_IsDataNotNotFound is
// THE central case this bead's NOTES require: a real, defined symbol that
// happens to have zero references is exit 0 with an empty results list, NOT
// exit 3. LookupSymbolsByName reports the symbol exists; LookupReferencesByName
// legitimately returns nothing.
func TestQuery_References_DefinedSymbolWithZeroReferences_IsDataNotNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return []codegraph.Symbol{{ID: uuid.New(), RepoID: repoID, File: "util.go", Name: "Orphan", Kind: "function"}}, false, nil
		},
		LookupReferencesByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Reference, bool, error) {
			return nil, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	resp, err := h.Query(agentCtx(t, "author"), referencesRequest("Orphan"))
	require.NoError(t, err, "a defined symbol with zero references must not be exit 3")
	assert.Empty(t, resp.Msg.GetLocations().GetLocations())
	assert.False(t, resp.Msg.GetTruncated())
}

// TestQuery_References_Success_OmitsKind proves refs rows carry the
// documented shape (docs/cli-spec.md:544: `{ repo, file, line, symbol }`,
// no kind) even though codegraph.Reference itself carries a Kind column.
func TestQuery_References_Success_OmitsKind(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return []codegraph.Symbol{{ID: uuid.New(), RepoID: repoID, File: "auth.go", Name: "Login", Kind: "function"}}, false, nil
		},
		LookupReferencesByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Reference, bool, error) {
			return []codegraph.Reference{{ID: uuid.New(), RepoID: repoID, File: "handler.go", Name: "Login", Kind: "call", Line: 10}}, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	resp, err := h.Query(agentCtx(t, "author"), referencesRequest("Login"))
	require.NoError(t, err)
	locations := resp.Msg.GetLocations().GetLocations()
	require.Len(t, locations, 1)
	assert.Equal(t, "handler.go", locations[0].GetFileLine().GetFile())
	assert.EqualValues(t, 10, locations[0].GetFileLine().GetLine())
	assert.Equal(t, "Login", locations[0].GetSymbol())
	assert.Empty(t, locations[0].GetKind(), "refs rows must not carry kind (docs/cli-spec.md:544)")
}

// TestQuery_Dependencies_TargetNotFound proves `graph deps` is exit 3 when
// the target resolves to no symbol at all.
func TestQuery_Dependencies_TargetNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	depsCalled := false
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return nil, false, nil
		},
		DepsFunc: func(context.Context, uuid.UUID, string, uuid.UUID, int32) ([]codegraph.Dependency, bool, error) {
			depsCalled = true
			return nil, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	_, err := h.Query(agentCtx(t, "author"), dependenciesRequest("ghost.go"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
	assert.False(t, depsCalled)
}

// TestQuery_Dependencies_Success_FromIsTargetToIsDependency proves the edge
// direction: From is the resolved target, To is each found dependency.
func TestQuery_Dependencies_Success_FromIsTargetToIsDependency(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	targetID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return []codegraph.Symbol{{ID: targetID, RepoID: repoID, File: "auth.go", Name: "Login", Kind: "function"}}, false, nil
		},
		DepsFunc: func(_ context.Context, repoArg uuid.UUID, branch string, symbolID uuid.UUID, _ int32) ([]codegraph.Dependency, bool, error) {
			assert.Equal(t, repoID, repoArg)
			assert.Equal(t, "main", branch)
			assert.Equal(t, targetID, symbolID)
			return []codegraph.Dependency{{Symbol: codegraph.Symbol{ID: uuid.New(), RepoID: repoID, File: "db.go", Name: "Connect", Kind: "function"}, Depth: 1}}, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	resp, err := h.Query(agentCtx(t, "author"), dependenciesRequest("Login"))
	require.NoError(t, err)
	edges := resp.Msg.GetDependencies().GetEdges()
	require.Len(t, edges, 1)
	assert.Equal(t, "Login", edges[0].GetFrom().GetSymbol())
	assert.Equal(t, "Connect", edges[0].GetTo().GetSymbol())
}

// TestQuery_Dependents_Success_FromIsDependentToIsTarget mirrors
// TestQuery_Dependencies_Success_FromIsTargetToIsDependency with the edge
// direction reversed: From is a symbol that depends on the target, To is
// the target.
func TestQuery_Dependents_Success_FromIsDependentToIsTarget(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	targetID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return []codegraph.Symbol{{ID: targetID, RepoID: repoID, File: "auth.go", Name: "Login", Kind: "function"}}, false, nil
		},
		DependentsFunc: func(_ context.Context, repoArg uuid.UUID, _ string, symbolID uuid.UUID, _ int32) ([]codegraph.Dependency, bool, error) {
			assert.Equal(t, targetID, symbolID)
			return []codegraph.Dependency{{Symbol: codegraph.Symbol{ID: uuid.New(), RepoID: repoID, File: "handler.go", Name: "Handle", Kind: "function"}, Depth: 1}}, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	resp, err := h.Query(agentCtx(t, "author"), dependentsRequest("Login"))
	require.NoError(t, err)
	edges := resp.Msg.GetDependencies().GetEdges()
	require.Len(t, edges, 1)
	assert.Equal(t, "Handle", edges[0].GetFrom().GetSymbol())
	assert.Equal(t, "Login", edges[0].GetTo().GetSymbol())
}

// TestQuery_History_Success proves history entries convert with Repo set
// from the resolved symbol's repo.
func TestQuery_History_Success(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	symbolID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return []codegraph.Symbol{{ID: symbolID, RepoID: repoID, File: "auth.go", Name: "Login", Kind: "function"}}, false, nil
		},
		HistoryFunc: func(_ context.Context, id uuid.UUID, _ int32) ([]codegraph.HistoryEntry, bool, error) {
			assert.Equal(t, symbolID, id)
			return []codegraph.HistoryEntry{{ID: uuid.New(), SymbolID: id, Commit: "a1b2c3d", Ref: "main", Message: "add login"}}, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	resp, err := h.Query(agentCtx(t, "author"), historyRequest("Login"))
	require.NoError(t, err)
	entries := resp.Msg.GetHistory().GetEntries()
	require.Len(t, entries, 1)
	assert.Equal(t, "bobcob7/doc-server", entries[0].GetRepo())
	assert.Equal(t, "a1b2c3d", entries[0].GetCommit())
	assert.Equal(t, "add login", entries[0].GetMessage())
}

// TestQuery_Truncated_ResultsCappedAtLimitAndFlagSet proves a capped
// response sets truncated: true (docs/cli-spec.md:535-537) AND returns no
// more than the requested limit rows -- both properties are required, and a
// mutation dropping either must fail this test.
func TestQuery_Truncated_ResultsCappedAtLimitAndFlagSet(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	targetID := uuid.New()
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return []codegraph.Symbol{{ID: targetID, RepoID: repoID, File: "widely_used.go", Name: "Widely", Kind: "function"}}, false, nil
		},
		DependentsFunc: func(context.Context, uuid.UUID, string, uuid.UUID, int32) ([]codegraph.Dependency, bool, error) {
			// The store itself reports truncated=true, as codegraph.Store's
			// own limit+1/fetchLimit contract does when more rows exist than
			// the caller's limit.
			deps := make([]codegraph.Dependency, 5)
			for i := range deps {
				deps[i] = codegraph.Dependency{Symbol: codegraph.Symbol{ID: uuid.New(), RepoID: repoID, Name: "Caller"}, Depth: 1}
			}
			return deps, true, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	req := dependentsRequest("Widely")
	req.Msg.Page = &loamv1.Page{Limit: 5}
	resp, err := h.Query(agentCtx(t, "author"), req)
	require.NoError(t, err)
	edges := resp.Msg.GetDependencies().GetEdges()
	assert.LessOrEqual(t, len(edges), 5, "at most the requested limit must be returned")
	assert.True(t, resp.Msg.GetTruncated(), "the response must indicate it was truncated")
}

// TestQuery_EmptyScope_ExpandsToAllEnrolledRepos_NotEmptySlice proves an
// empty QueryScope.repos reaches the symbol store with a concrete, non-empty
// repo id slice -- repo-scope expansion is this package's own job, not
// SymbolStore's (this bead's NOTES). A regression that passed the empty
// scope straight through would make repoIDs empty here, which
// codegraph.Store.LookupSymbolsByName documents as "matches nothing" -- a
// silent, wrong empty result instead of a loud test failure.
func TestQuery_EmptyScope_ExpandsToAllEnrolledRepos_NotEmptySlice(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	var seenRepoIDs []uuid.UUID
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(_ context.Context, repoIDs []uuid.UUID, _, _, _ string, _ int32) ([]codegraph.Symbol, bool, error) {
			seenRepoIDs = repoIDs
			return []codegraph.Symbol{{ID: uuid.New(), RepoID: repoID, File: "auth.go", Name: "Login", Kind: "function"}}, false, nil
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	_, err := h.Query(agentCtx(t, "author"), definitionRequest("Login"))
	require.NoError(t, err)
	assert.NotEmpty(t, seenRepoIDs, "an empty QueryScope must expand to concrete enrolled repo ids before reaching the store")
	assert.Equal(t, []uuid.UUID{repoID}, seenRepoIDs)
}

// TestQuery_UnresolvableScope_ReturnsInvalidArgument proves an explicit
// scope naming an unenrolled repo is rejected as a usage error
// (docs/cli-spec.md's exit-2 "unresolvable scope" case), not silently
// treated as not-found or ignored.
func TestQuery_UnresolvableScope_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	scopeStore := fakeScopeStore{
		getRepoByName: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, reposstore.ErrNotFound
		},
	}
	symbols := &graph.SymbolStoreMock{}
	h := newHandler(t, symbols, scopeStore, []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	req := definitionRequest("Login")
	req.Msg.Scope = &loamv1.QueryScope{Repos: []string{"bobcob7/ghost-repo"}}
	_, err := h.Query(agentCtx(t, "author"), req)
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
}

// TestQuery_NoQueryKindSet_ReturnsInvalidArgument proves an unset oneof is
// rejected rather than panicking or silently matching some default kind.
func TestQuery_NoQueryKindSet_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	h := newHandler(t, &graph.SymbolStoreMock{}, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	_, err := h.Query(agentCtx(t, "author"), connect.NewRequest(&loamv1.QueryRequest{}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
}

// TestQuery_StoreFailure_MapsToInternalAndLogs proves an unclassified store
// error becomes CodeInternal and is logged, not silently swallowed.
func TestQuery_StoreFailure_MapsToInternalAndLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	dbErr := errors.New("connection reset by peer")
	symbols := &graph.SymbolStoreMock{
		LookupSymbolsByNameFunc: func(context.Context, []uuid.UUID, string, string, string, int32) ([]codegraph.Symbol, bool, error) {
			return nil, false, dbErr
		},
	}
	h := newHandler(t, symbols, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityGraphQuery}, &buf)
	_, err := h.Query(agentCtx(t, "author"), definitionRequest("Login"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.Contains(t, buf.String(), "connection reset by peer")
}
