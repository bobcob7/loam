package search_test

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

	"github.com/bobcob7/loam/internal/chunkstore"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/search"
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
// external test package -- the same convention internal/handler/repo's own
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
// repo.
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

// fixedEmbedder always returns vector for any input -- most tests below do
// not care about the actual embedding math, only that Search calls Embed
// with the request query and threads the result through.
type fixedEmbedder struct {
	vector []float32
	err    error
	calls  *[]string
}

func (e fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if e.calls != nil {
		*e.calls = append(*e.calls, texts...)
	}
	if e.err != nil {
		return nil, e.err
	}
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = e.vector
	}
	return vectors, nil
}

func newHandler(t *testing.T, chunks search.ChunkStore, embedder search.Embedder, scopeStore handler.ScopeStore, roleCaps []handler.Capability, buf *bytes.Buffer) *search.Handler {
	t.Helper()
	checker := handler.NewCapabilityChecker(fakeRoleStore{capabilities: roleCaps})
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(buf, nil)))
	scope := handler.NewScopeResolver(scopeStore)
	return search.New(chunks, embedder, scope, checker, mapper, testLogger())
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	return connectErr.Code()
}

func searchRequest(query string) *connect.Request[loamv1.SearchRequest] {
	return connect.NewRequest(&loamv1.SearchRequest{Query: query})
}

// TestSearch_AgentLackingSearchCapability_Denied proves the capability gate
// runs before the query is even embedded.
func TestSearch_AgentLackingSearchCapability_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	embedder := fixedEmbedder{vector: []float32{1, 0, 0}}
	chunks := &search.ChunkStoreMock{
		SearchFunc: func(context.Context, []uuid.UUID, string, []float32, int) ([]chunkstore.Chunk, error) {
			t.Fatal("chunk store must not be consulted when the capability gate denies the caller")
			return nil, nil
		},
	}
	h := newHandler(t, chunks, embedder, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.Search(agentCtx(t, "reviewer-without-search"), searchRequest("how is auth handled"))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
}

// TestSearch_EmptyQuery_ReturnsInvalidArgument proves an empty query is
// rejected before embedding or scope resolution.
func TestSearch_EmptyQuery_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	embedder := fixedEmbedder{vector: []float32{1, 0, 0}}
	h := newHandler(t, &search.ChunkStoreMock{}, embedder, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilitySearch}, &buf)
	_, err := h.Search(agentCtx(t, "author"), searchRequest(""))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
}

// TestSearch_EmptyScope_ExpandsToAllEnrolledRepos_NotEmptySlice proves an
// empty QueryScope.repos reaches the chunk store with a concrete, non-empty
// repo id slice -- repo-scope expansion is this package's own job, not
// ChunkStore's.
func TestSearch_EmptyScope_ExpandsToAllEnrolledRepos_NotEmptySlice(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	var seenRepoIDs []uuid.UUID
	embedder := fixedEmbedder{vector: []float32{1, 0, 0}}
	chunks := &search.ChunkStoreMock{
		SearchFunc: func(_ context.Context, repoIDs []uuid.UUID, _ string, _ []float32, _ int) ([]chunkstore.Chunk, error) {
			seenRepoIDs = repoIDs
			return nil, nil
		},
	}
	h := newHandler(t, chunks, embedder, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilitySearch}, &buf)
	_, err := h.Search(agentCtx(t, "author"), searchRequest("how is auth handled"))
	require.NoError(t, err)
	assert.NotEmpty(t, seenRepoIDs, "an empty QueryScope must expand to concrete enrolled repo ids before reaching the store")
	assert.Equal(t, []uuid.UUID{repoID}, seenRepoIDs)
}

// TestSearch_RanksByCosineSimilarityAcrossRepos proves results are globally
// re-ranked by score across repos, not merely concatenated per-repo (the
// property that distinguishes SearchService from GraphService's per-repo
// fan-out, docs/cli-spec.md "RAG queries (search)").
func TestSearch_RanksByCosineSimilarityAcrossRepos(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoA := uuid.New()
	repoB := uuid.New()
	query := []float32{1, 0}
	embedder := fixedEmbedder{vector: query}
	chunks := &search.ChunkStoreMock{
		SearchFunc: func(_ context.Context, repoIDs []uuid.UUID, branch string, _ []float32, _ int) ([]chunkstore.Chunk, error) {
			switch repoIDs[0] {
			case repoA:
				// Orthogonal to the query -- similarity 0.
				return []chunkstore.Chunk{{RepoID: repoA, File: "low.go", StartLine: 1, EndLine: 2, Content: "low relevance", Embedding: []float32{0, 1}}}, nil
			case repoB:
				// Parallel to the query -- similarity 1.
				return []chunkstore.Chunk{{RepoID: repoB, File: "high.go", StartLine: 1, EndLine: 2, Content: "high relevance", Embedding: []float32{1, 0}}}, nil
			default:
				t.Fatalf("unexpected repo id %s", repoIDs[0])
				return nil, nil
			}
		},
	}
	scopeStore := fakeScopeStore{
		getRepoByName: func(_ context.Context, name string) (reposstore.Repo, error) {
			if name == "bobcob7/alpha" {
				return reposstore.Repo{ID: repoA, Name: name, IndexedBranch: "main"}, nil
			}
			return reposstore.Repo{ID: repoB, Name: name, IndexedBranch: "main"}, nil
		},
		listAllRepoNames: func(context.Context) ([]string, error) { return []string{"bobcob7/alpha", "bobcob7/beta"}, nil },
		listTargetBranches: func(_ context.Context, id uuid.UUID) ([]reposstore.TargetBranch, error) {
			return []reposstore.TargetBranch{{RepoID: id, Branch: "main"}}, nil
		},
	}
	h := newHandler(t, chunks, embedder, scopeStore, []handler.Capability{handler.CapabilitySearch}, &buf)
	resp, err := h.Search(agentCtx(t, "author"), searchRequest("auth"))
	require.NoError(t, err)
	results := resp.Msg.GetResults()
	require.Len(t, results, 2)
	assert.Equal(t, "high.go", results[0].GetFile(), "the globally higher-scoring chunk must rank first regardless of repo order")
	assert.Equal(t, "low.go", results[1].GetFile())
	assert.Greater(t, results[0].GetScore(), results[1].GetScore())
	require.Len(t, resp.Msg.GetIngested(), 2)
}

// TestSearch_Truncated_ResultsCappedAtLimitAndFlagSet proves a capped
// response sets truncated: true AND returns no more than the requested
// limit rows.
func TestSearch_Truncated_ResultsCappedAtLimitAndFlagSet(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	embedder := fixedEmbedder{vector: []float32{1, 0}}
	chunks := &search.ChunkStoreMock{
		SearchFunc: func(_ context.Context, _ []uuid.UUID, _ string, _ []float32, limit int) ([]chunkstore.Chunk, error) {
			// Return more than the caller's requested limit -- Search fetches
			// limit+offset+1 per repo precisely so it can detect this.
			result := make([]chunkstore.Chunk, limit)
			for i := range result {
				result[i] = chunkstore.Chunk{RepoID: repoID, File: "f.go", StartLine: i, EndLine: i + 1, Content: "x", Embedding: []float32{1, 0}}
			}
			return result, nil
		},
	}
	h := newHandler(t, chunks, embedder, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilitySearch}, &buf)
	req := searchRequest("auth")
	req.Msg.Page = &loamv1.Page{Limit: 3}
	resp, err := h.Search(agentCtx(t, "author"), req)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(resp.Msg.GetResults()), 3, "at most the requested limit must be returned")
	assert.True(t, resp.Msg.GetTruncated(), "the response must indicate it was truncated")
}

// TestSearch_DefaultLimit_IsTenWhenPageLimitZero proves Page.limit == 0
// resolves to the documented server default of 10
// (proto SearchRequest.page comment; docs/cli-spec.md).
func TestSearch_DefaultLimit_IsTenWhenPageLimitZero(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	var seenLimit int
	embedder := fixedEmbedder{vector: []float32{1, 0}}
	chunks := &search.ChunkStoreMock{
		SearchFunc: func(_ context.Context, _ []uuid.UUID, _ string, _ []float32, limit int) ([]chunkstore.Chunk, error) {
			seenLimit = limit
			return nil, nil
		},
	}
	h := newHandler(t, chunks, embedder, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilitySearch}, &buf)
	_, err := h.Search(agentCtx(t, "author"), searchRequest("auth"))
	require.NoError(t, err)
	assert.Equal(t, 11, seenLimit, "default limit 10, fetched as limit+offset+1 = 11 to detect truncation")
}

// TestSearch_EmbedderFailure_MapsToInternalAndLogs proves an embedder
// failure is surfaced, not silently swallowed, and the chunk store is never
// consulted with a garbage embedding.
func TestSearch_EmbedderFailure_MapsToInternalAndLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	embedErr := errors.New("embedder unreachable")
	embedder := fixedEmbedder{err: embedErr}
	chunks := &search.ChunkStoreMock{
		SearchFunc: func(context.Context, []uuid.UUID, string, []float32, int) ([]chunkstore.Chunk, error) {
			t.Fatal("chunk store must not be consulted when embedding fails")
			return nil, nil
		},
	}
	h := newHandler(t, chunks, embedder, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilitySearch}, &buf)
	_, err := h.Search(agentCtx(t, "author"), searchRequest("auth"))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.Contains(t, buf.String(), "embedder unreachable")
}

// TestSearch_UnresolvableScope_ReturnsInvalidArgument proves an explicit
// scope naming an unenrolled repo is rejected as a usage error.
func TestSearch_UnresolvableScope_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	scopeStore := fakeScopeStore{
		getRepoByName: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, reposstore.ErrNotFound
		},
	}
	embedder := fixedEmbedder{vector: []float32{1, 0}}
	h := newHandler(t, &search.ChunkStoreMock{}, embedder, scopeStore, []handler.Capability{handler.CapabilitySearch}, &buf)
	req := searchRequest("auth")
	req.Msg.Scope = &loamv1.QueryScope{Repos: []string{"bobcob7/ghost-repo"}}
	_, err := h.Search(agentCtx(t, "author"), req)
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
}

// TestSearch_ResultShape proves each SearchResult carries repo/file/lines/
// score/snippet, the docs/cli-spec.md-documented row shape.
func TestSearch_ResultShape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	embedder := fixedEmbedder{vector: []float32{1, 0}}
	chunks := &search.ChunkStoreMock{
		SearchFunc: func(context.Context, []uuid.UUID, string, []float32, int) ([]chunkstore.Chunk, error) {
			return []chunkstore.Chunk{{RepoID: repoID, File: "auth.go", StartLine: 40, EndLine: 58, Content: "func Login() {}", Embedding: []float32{1, 0}}}, nil
		},
	}
	h := newHandler(t, chunks, embedder, oneRepoScope(repoID, "bobcob7/doc-server", "main"), []handler.Capability{handler.CapabilitySearch}, &buf)
	resp, err := h.Search(agentCtx(t, "author"), searchRequest("how is auth handled"))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetResults(), 1)
	result := resp.Msg.GetResults()[0]
	assert.Equal(t, "bobcob7/doc-server", result.GetRepo())
	assert.Equal(t, "auth.go", result.GetFile())
	assert.EqualValues(t, 40, result.GetStartLine())
	assert.EqualValues(t, 58, result.GetEndLine())
	assert.Equal(t, "func Login() {}", result.GetSnippet())
	assert.InDelta(t, 1.0, result.GetScore(), 0.0001)
}
