package search

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/chunkstore"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/handler"
)

// defaultLimit is the server default result cap when Page.limit is 0
// (proto's own SearchRequest.page comment: "the server default (10)
// applies"; docs/cli-spec.md: "--limit <n> ... defaults to 10").
const defaultLimit = 10

// Handler implements loamv1connect.SearchServiceHandler.
type Handler struct {
	chunks       ChunkStore
	embedder     Embedder
	scope        *handler.ScopeResolver
	capabilities *handler.CapabilityChecker
	errors       *handler.ErrorMapper
	logger       *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ loamv1connect.SearchServiceHandler = (*Handler)(nil)

// New builds a Handler over chunks and embedder, gating Search with
// capabilities (the search capability, per docs/web-spec.md ->
// RoleService), resolving QueryScope through scope, and mapping domain
// errors through errors.
func New(chunks ChunkStore, embedder Embedder, scope *handler.ScopeResolver, capabilities *handler.CapabilityChecker, errors *handler.ErrorMapper, logger *slog.Logger) *Handler {
	return &Handler{chunks: chunks, embedder: embedder, scope: scope, capabilities: capabilities, errors: errors, logger: logger}
}

// Search embeds the request query and returns the globally top-ranked
// chunks across every repo in scope (docs/cli-spec.md "RAG queries
// (search)": "search --all genuinely spans repos: semantic matches surface
// relevant chunks from any enrolled repo"). Unlike GraphService.Query, which
// fans out per repo and unions the results with no cross-repo ranking,
// results here are merged and re-ranked by score across the whole scope
// before the limit/offset page is cut. One internal/chunkstore.Store.Search
// call per repo is still required -- that store takes one targetBranch per
// call, and distinct repos can have distinct indexed_branch values
// (docs/persistence-spec.md "repos") -- but the RESULT ORDER is global, not
// per-repo.
//
// chunkstore.Store.Search reports no score of its own (it returns bare
// Chunk rows), so Score is computed here from the query embedding and each
// returned Chunk's own stored Embedding via cosine similarity -- the same
// metric the chunks_embedding HNSW index itself ranks by (vector_cosine_ops)
// -- rather than left unset. Recomputing it client-side, instead of adding a
// distance column to the store's return shape, keeps chunkstore's seam
// (already consumed by loam-c94's ingest pipeline) unchanged for this bead.
func (h *Handler) Search(ctx context.Context, req *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilitySearch); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	query := req.Msg.GetQuery()
	if query == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("search: empty query: %w", handler.ErrInvalidArgument))
	}
	scoped, err := h.scope.Resolve(ctx, req.Msg.GetScope().GetRepos())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	limit, offset := resolvePage(req.Msg.GetPage())
	vectors, err := h.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("embedding search query: %w", err))
	}
	if len(vectors) != 1 {
		return nil, h.errors.ToConnectErr(fmt.Errorf("embedding search query: got %d vectors, want 1: %w", len(vectors), handler.ErrInvalidArgument))
	}
	embedding := vectors[0]
	fetchLimit := int(limit + offset + 1)
	var all []scoredChunk
	for _, repo := range scoped {
		chunks, searchErr := h.chunks.Search(ctx, []uuid.UUID{repo.ID}, repo.IndexedBranch, embedding, fetchLimit)
		if searchErr != nil {
			return nil, h.errors.ToConnectErr(fmt.Errorf("searching repo %s: %w", repo.Name, searchErr))
		}
		for _, c := range chunks {
			all = append(all, scoredChunk{repoName: repo.Name, chunk: c, score: cosineSimilarity(embedding, c.Embedding)})
		}
	}
	sort.Slice(all, func(i, j int) bool { return lessScoredChunk(all[i], all[j]) })
	page, truncated, total := paginateScored(all, limit, offset)
	results := make([]*loamv1.SearchResult, len(page))
	for i, sc := range page {
		results[i] = toSearchResult(sc)
	}
	ingested, err := h.scope.Ingested(ctx, scoped)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("building ingested provenance: %w", err))
	}
	return connect.NewResponse(&loamv1.SearchResponse{
		Results:   results,
		PageInfo:  &loamv1.PageInfo{Total: uint32(total)},
		Ingested:  toProtoIngested(ingested),
		Truncated: truncated,
	}), nil
}

// scoredChunk pairs a returned chunkstore.Chunk with the repo name that
// produced it and its cosine-similarity score against the query embedding,
// so Search can merge per-repo result sets into one globally ranked list.
type scoredChunk struct {
	repoName string
	chunk    chunkstore.Chunk
	score    float32
}

// lessScoredChunk orders scoredChunk descending by score (highest
// similarity first), breaking ties deterministically by repo name, then
// file, then start line -- otherwise-unordered ties would make trimming to
// a limit nondeterministic across identical calls.
func lessScoredChunk(a, b scoredChunk) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if a.repoName != b.repoName {
		return a.repoName < b.repoName
	}
	if a.chunk.File != b.chunk.File {
		return a.chunk.File < b.chunk.File
	}
	return a.chunk.StartLine < b.chunk.StartLine
}

// cosineSimilarity computes the cosine similarity between a and b -- the
// same metric the chunks_embedding HNSW index ranks by (vector_cosine_ops).
// Returns 0 if either vector has zero magnitude (should not occur for a
// real embedding, but this avoids a division by zero rather than
// propagating a NaN into a response) or the vectors have mismatched
// dimensions (defensively; a real embedder/store pairing never produces
// this).
func cosineSimilarity(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, normA, normB float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

// paginateScored slices the globally sorted all to the [offset, offset+
// limit) window, reporting whether more rows exist beyond it and the size
// of the merged set the window was cut from (see graph.paginate's identical
// caveat: this is a lower bound, not a true corpus count, whenever
// truncated is true). all was fetched with limit+offset+1 rows per repo
// (see Search), so len(all) > offset+limit proves at least one more match
// exists globally, regardless of how many repos contributed rows.
func paginateScored(all []scoredChunk, limit, offset int32) ([]scoredChunk, bool, int32) {
	total := int32(len(all))
	truncated := total > offset+limit
	if offset >= total {
		return []scoredChunk{}, truncated, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], truncated, total
}

// toSearchResult converts sc to the wire shape.
func toSearchResult(sc scoredChunk) *loamv1.SearchResult {
	return &loamv1.SearchResult{Repo: sc.repoName, File: sc.chunk.File, StartLine: uint32(sc.chunk.StartLine), EndLine: uint32(sc.chunk.EndLine), Score: sc.score, Snippet: sc.chunk.Content}
}

// resolvePage extracts limit/offset from page, applying defaultLimit when
// page is nil or its limit is zero.
func resolvePage(page *loamv1.Page) (limit, offset int32) {
	limit = int32(page.GetLimit())
	if limit <= 0 {
		limit = defaultLimit
	}
	return limit, int32(page.GetOffset())
}

// toProtoIngested converts handler.Ingested entries to the wire shape.
func toProtoIngested(entries []handler.Ingested) []*loamv1.Ingested {
	result := make([]*loamv1.Ingested, len(entries))
	for i, e := range entries {
		result[i] = &loamv1.Ingested{Repo: e.Repo, Target: e.Target, Ref: e.Ref, At: e.At}
	}
	return result
}
