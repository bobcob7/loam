package vectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/ingest/chunker"
)

// maxEmbedBatch caps how many chunk texts go into a single Embed call.
//
// The Embedder interface is a batch API and the production Ollama client
// sends the whole slice as ONE HTTP request (/api/embed's "input" array,
// internal/ingest/embed/ollama), so embedding a full rebuild's chunks in a
// single call would mean one request holding every chunk in the repo --
// tens of megabytes of JSON, and a server-side batch large enough to
// exhaust the model's memory, failing the whole ingest rather than one
// request. Batching in bounded groups keeps the per-request cost flat and
// independent of repo size while still amortizing the round trip the way
// one-call-per-chunk never would.
//
// 64 is a round working figure, not a measured optimum: it is small enough
// that a batch of even p99-sized chunks (~2KB, per internal/ingest/chunk's
// bytesPerTokenBudget doc comment) is a ~128KB request, and large enough
// that a 10,000-chunk repo costs ~157 requests instead of 10,000.
const maxEmbedBatch = 64

var (
	// errVectorCount means the Embedder returned a different number of
	// vectors than the texts it was given, so no chunk can be matched to
	// its vector with any confidence. Persisting anything after that would
	// mean pairing chunk content with some OTHER chunk's embedding -- a
	// corrupt index that looks healthy and misranks silently forever --
	// so this aborts the ingest instead. It is raised from two places on
	// purpose: embedAll checks each batch's response as it arrives (the
	// message names the offset), and chunkInputsFor re-checks the total
	// before pairing anything, so a mismatch can never walk off the end of
	// the vector list mid-write (see chunkInputsFor's doc comment).
	errVectorCount = errors.New("vectors: embedder returned the wrong number of vectors")
	// errDimensionMismatch means a returned vector's width is not the
	// Embedder's own reported Dimension(). It is checked here, at the
	// write boundary, rather than trusted: a vector must be stored whole
	// or not at all, never truncated to fit or zero-padded out to the
	// column width, since either would produce a vector that no longer
	// represents its chunk while still inserting cleanly (the same
	// silent-corruption failure mode docs/ingestion-spec.md's
	// truncate:false rule exists to prevent on the token side).
	//
	// This catches the embedder disagreeing with ITSELF. The other half of
	// the invariant -- the Embedder's Dimension() agreeing with the
	// vector(768) chunks.embedding is pinned to -- is enforced by Postgres
	// at INSERT ("expected 768 dimensions, not N"), which aborts the
	// orchestrator's transaction just as loudly; there is deliberately no
	// second copy of the 768 literal in Go production code to drift from
	// the migration (0002_code_intel.up.sql's own comment explains where
	// that invariant is pinned instead).
	errDimensionMismatch = errors.New("vectors: embedder returned a vector of the wrong width")
	// errBadDimension means the Embedder reports a non-positive
	// Dimension(), which no vector can ever match. Caught up front, before
	// a single embed call is spent, since every subsequent check would
	// fail anyway.
	errBadDimension = errors.New("vectors: embedder reports a non-positive dimension")
)

// Stats reports what one IngestFileChunks call did, mirroring
// internal/ingest/graph.Stats's role on the other track so the orchestrator
// (loam-c94.12) and the ingest_jobs.stats writer (loam-c94.13) can log and
// persist per-ingest counts the same way for both.
type Stats struct {
	// FilesReplaced is how many files had their chunks replaced -- every
	// file in the input batch, since each one's prior chunks are dropped
	// whether or not it produced any new ones.
	FilesReplaced int
	// FilesWithoutChunks is how many of FilesReplaced chunked to zero
	// units and therefore only dropped their prior chunks without
	// inserting replacements (a non-exclusive subset of FilesReplaced,
	// counted separately because "the file's chunks are gone on purpose"
	// is worth seeing in an operator's log rather than inferring from a
	// row count that went to zero).
	FilesWithoutChunks int
	// ChunksWritten is the total number of chunks rows inserted across
	// every file in the batch.
	ChunksWritten int
	// EmbedCalls is how many Embed requests the batch cost -- the
	// expensive, network-bound step, so it is worth reporting directly
	// rather than leaving a reader to divide ChunksWritten by
	// maxEmbedBatch. A batch whose files all chunked to zero units costs
	// zero embed calls.
	EmbedCalls int
}

// Indexer embeds chunk units and persists them. e is the embedding backend
// (constructor-injected, so tests use a mock or internal/testembed's
// deterministic double rather than a live Ollama); logger carries the
// per-file operator visibility. Construct with New.
type Indexer struct {
	embedder embedder
	logger   *slog.Logger
}

// New builds an Indexer over e. logger must be non-nil.
func New(e embedder, logger *slog.Logger) *Indexer {
	return &Indexer{embedder: e, logger: logger}
}

// IngestFileChunks embeds every chunk unit in files (typically the
// []chunker.FileChunks a chunker.ChunkFiles call just returned for the
// plan's reparse set) and persists them through st's per-file
// delete-and-replace, one ReplaceFileChunks call per file.
//
// st MUST be bound to a transaction the CALLER owns and will commit --
// chunkstore.NewInTx over the swap orchestrator's transaction
// (loam-c94.12). This method begins no transaction, commits nothing, and
// rolls nothing back: on any error it returns with whatever it had already
// staged still staged, for the caller to discard by rolling back. That is
// the whole point of the arrangement (docs/ingestion-spec.md "Consistency
// & Failure": readers keep serving the prior index until one commit swaps
// it). Handing it a chunkstore.Store built with New instead would silently
// commit each file on its own and defeat the atomic swap.
//
// Every file in files gets a ReplaceFileChunks call, INCLUDING one whose
// Units is empty: a reparsed file that now chunks to nothing (its content
// was emptied, or reduced to only whitespace/imports) must still have its
// prior chunks dropped, and an empty inputs slice is exactly
// ReplaceFileChunks's documented "delete without inserting" case. Skipping
// it would leave that file's stale chunks searchable against content that
// no longer exists.
//
// Embedding runs FIRST, for the whole batch, before any store call: chunk
// texts are flattened across all files in input order and sent in Embed
// requests of up to maxEmbedBatch texts each (a single request may
// therefore span a file boundary -- batching is by text count, not by
// file). Doing all the network work up front keeps the caller's
// transaction from being held open across slow Ollama calls, which is what
// loam-c94.12's own DESIGN asks for ("prefer computing outside the tx and
// keeping the write phase short"); it also means an embedder failure costs
// zero writes rather than aborting halfway through the batch.
//
// Anything that goes wrong -- ctx cancellation, an embed failure, a vector
// count or width that disagrees with the Embedder's own Dimension(), or a
// store write failing -- returns immediately, wrapped. There is no
// per-file skip-and-continue here (see the package doc comment for why):
// the enclosing transaction is going to roll back either way.
func (ix *Indexer) IngestFileChunks(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (Stats, error) {
	var stats Stats
	if err := ctx.Err(); err != nil {
		return stats, fmt.Errorf("embedding chunks for %s@%s: %w", repoID, targetBranch, err)
	}
	dimension := ix.embedder.Dimension()
	if dimension <= 0 {
		return stats, fmt.Errorf("embedding chunks for %s@%s: %w: %d", repoID, targetBranch, errBadDimension, dimension)
	}
	texts := flattenTexts(files)
	embeddings, calls, err := ix.embedAll(ctx, texts, dimension)
	stats.EmbedCalls = calls
	if err != nil {
		return stats, fmt.Errorf("embedding chunks for %s@%s: %w", repoID, targetBranch, err)
	}
	perFile, err := chunkInputsFor(files, embeddings)
	if err != nil {
		return stats, fmt.Errorf("pairing chunks with vectors for %s@%s: %w", repoID, targetBranch, err)
	}
	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("replacing chunks for %s: %w", f.Path, err)
		}
		inputs := perFile[i]
		if _, err := st.ReplaceFileChunks(ctx, repoID, targetBranch, f.Path, inputs); err != nil {
			return stats, fmt.Errorf("replacing chunks for %s: %w", f.Path, err)
		}
		stats.FilesReplaced++
		stats.ChunksWritten += len(inputs)
		if len(inputs) == 0 {
			stats.FilesWithoutChunks++
			ix.logger.InfoContext(ctx, "file chunked to zero units; dropping its prior chunks", "file", f.Path, "repo_id", repoID, "target_branch", targetBranch)
		}
	}
	return stats, nil
}

// chunkInputsFor pairs each file's units with the vectors embedAll produced
// for them, by position, returning one ChunkInput slice per file in files'
// own order (an empty, non-nil slice for a file with no units -- exactly
// what ReplaceFileChunks's delete-without-inserting case takes).
//
// It validates the total count BEFORE indexing anything, rather than
// walking the vector list as it writes. That ordering is the point: this
// whole call runs inside the swap orchestrator's transaction, and an
// index-out-of-range panic there unwinds past the deferred rollback
// (internal/chunkstore's transactor doc comment says so explicitly) instead
// of failing the ingest cleanly and letting the previous index stay live.
// embedAll's own per-batch check already makes a mismatch unreachable in
// practice; this is the cheap guarantee that a weakened check upstream
// degrades into an error rather than a crash.
func chunkInputsFor(files []chunker.FileChunks, embeddings [][]float32) ([][]chunkstore.ChunkInput, error) {
	total := 0
	for _, f := range files {
		total += len(f.Units)
	}
	if len(embeddings) != total {
		return nil, fmt.Errorf("%w: %d chunks to pair with %d vectors", errVectorCount, total, len(embeddings))
	}
	perFile := make([][]chunkstore.ChunkInput, len(files))
	next := 0
	for i, f := range files {
		inputs := make([]chunkstore.ChunkInput, len(f.Units))
		for j, u := range f.Units {
			inputs[j] = chunkstore.ChunkInput{
				StartLine: u.StartLine,
				EndLine:   u.EndLine,
				Content:   u.Content,
				Embedding: embeddings[next],
			}
			next++
		}
		perFile[i] = inputs
	}
	return perFile, nil
}

// flattenTexts concatenates every file's chunk contents into one slice, in
// input order, so embedAll can batch across file boundaries.
// IngestFileChunks walks files in that same order afterward to hand each
// chunk back its own vector by position.
func flattenTexts(files []chunker.FileChunks) []string {
	total := 0
	for _, f := range files {
		total += len(f.Units)
	}
	texts := make([]string, 0, total)
	for _, f := range files {
		for _, u := range f.Units {
			texts = append(texts, u.Content)
		}
	}
	return texts
}

// embedAll embeds texts in groups of at most maxEmbedBatch, returning every
// vector in the same order as texts plus the number of Embed calls it
// made. Each batch's result is validated against dimension before it is
// accepted (see errVectorCount and errDimensionMismatch); an empty texts
// makes no call at all.
func (ix *Indexer) embedAll(ctx context.Context, texts []string, dimension int) ([][]float32, int, error) {
	out := make([][]float32, 0, len(texts))
	calls := 0
	for start := 0; start < len(texts); start += maxEmbedBatch {
		if err := ctx.Err(); err != nil {
			return nil, calls, fmt.Errorf("embedding batch at offset %d: %w", start, err)
		}
		end := min(start+maxEmbedBatch, len(texts))
		batch := texts[start:end]
		vecs, err := ix.embedder.Embed(ctx, batch)
		calls++
		if err != nil {
			return nil, calls, fmt.Errorf("embedding %d chunks at offset %d: %w", len(batch), start, err)
		}
		if len(vecs) != len(batch) {
			return nil, calls, fmt.Errorf("%w: sent %d texts at offset %d, got %d vectors", errVectorCount, len(batch), start, len(vecs))
		}
		for i, vec := range vecs {
			if len(vec) != dimension {
				return nil, calls, fmt.Errorf("%w: vector %d has width %d, want %d", errDimensionMismatch, start+i, len(vec), dimension)
			}
		}
		out = append(out, vecs...)
	}
	return out, calls, nil
}
