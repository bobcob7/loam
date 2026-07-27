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
// file). Doing all the network work up front means an embedder failure
// costs zero writes rather than aborting halfway through the batch.
//
// It does NOT, on its own, keep the caller's transaction short. This
// method is Prepare followed by Persist, so a caller that invokes it with
// a transaction-bound store holds that transaction open for the embedding
// run too -- the exact thing loam-c94.12's DESIGN says not to do ("prefer
// computing outside the tx and keeping the write phase short"). A caller
// that owns a transaction should call Prepare BEFORE opening it and
// Persist inside it; this method is the convenience composition for a
// caller with no such constraint, and it is what this package's own
// single-call tests exercise.
//
// Anything that goes wrong -- ctx cancellation, an embed failure, a vector
// count or width that disagrees with the Embedder's own Dimension(), or a
// store write failing -- returns immediately, wrapped. There is no
// per-file skip-and-continue here (see the package doc comment for why):
// the enclosing transaction is going to roll back either way.
func (ix *Indexer) IngestFileChunks(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (Stats, error) {
	prepared, stats, err := ix.Prepare(ctx, repoID, targetBranch, files)
	if err != nil {
		return stats, err
	}
	writeStats, err := ix.Persist(ctx, st, repoID, targetBranch, prepared)
	stats.merge(writeStats)
	return stats, err
}

// Prepared is one batch's embedding work, already done: every file's chunk
// units paired with the vector that represents it, ready to be written
// with no further network calls. It is deliberately opaque -- a caller
// obtains one only from Prepare and can only spend it on Persist, so there
// is no way to construct a Prepared whose vectors were never validated
// against the Embedder's own Dimension().
type Prepared struct {
	files []preparedFile
}

// preparedFile is one file's share of a Prepared: its path, and the
// ChunkInputs (possibly none) that must replace whatever chunks that file
// currently has.
type preparedFile struct {
	path   string
	inputs []chunkstore.ChunkInput
}

// Files reports how many files this Prepared will replace the chunks of --
// including any that prepared to zero units. Exposed so the swap
// orchestrator (loam-c94.12) can log the size of the write phase it is
// about to enter without Prepared having to leak its contents.
func (p Prepared) Files() int { return len(p.files) }

// Prepare does the whole NETWORK half of IngestFileChunks and none of the
// database half: it embeds every chunk unit in files and pairs each unit
// with its vector, returning a Prepared that Persist can write with no
// further Embed calls.
//
// It exists as its own entry point because the swap orchestrator
// (loam-c94.12) must not hold its transaction open across slow Ollama
// calls, and it cannot avoid that by calling IngestFileChunks -- that
// method embeds INSIDE the call, so a caller that invokes it with a
// transaction-bound store necessarily has the transaction open for the
// whole embedding run. Splitting the phases is what actually delivers the
// property IngestFileChunks's own doc comment claims: the orchestrator
// calls Prepare before it begins its transaction (concurrently with the
// parse->graph track's own compute), then Persist inside it. It is also
// what makes the two tracks' compute genuinely concurrent, since a pgx.Tx
// is not goroutine-safe and nothing that touches one can run in parallel
// with anything else that does.
//
// repoID and targetBranch are used only to identify the batch in error
// messages and logs; Prepare touches no database.
func (ix *Indexer) Prepare(ctx context.Context, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (Prepared, Stats, error) {
	var stats Stats
	if err := ctx.Err(); err != nil {
		return Prepared{}, stats, fmt.Errorf("embedding chunks for %s@%s: %w", repoID, targetBranch, err)
	}
	dimension := ix.embedder.Dimension()
	if dimension <= 0 {
		return Prepared{}, stats, fmt.Errorf("embedding chunks for %s@%s: %w: %d", repoID, targetBranch, errBadDimension, dimension)
	}
	texts := flattenTexts(files)
	embeddings, calls, err := ix.embedAll(ctx, texts, dimension)
	stats.EmbedCalls = calls
	if err != nil {
		return Prepared{}, stats, fmt.Errorf("embedding chunks for %s@%s: %w", repoID, targetBranch, err)
	}
	perFile, err := chunkInputsFor(files, embeddings)
	if err != nil {
		return Prepared{}, stats, fmt.Errorf("pairing chunks with vectors for %s@%s: %w", repoID, targetBranch, err)
	}
	prepared := Prepared{files: make([]preparedFile, len(files))}
	for i, f := range files {
		prepared.files[i] = preparedFile{path: f.Path, inputs: perFile[i]}
	}
	return prepared, stats, nil
}

// Persist does the whole DATABASE half of IngestFileChunks and none of the
// network half: one st.ReplaceFileChunks call per file in p, in the order
// Prepare received them, making no Embed call at all. This is the only
// part of this package that belongs inside the swap orchestrator's
// transaction (see Prepare's doc comment).
//
// st MUST be bound to a transaction the CALLER owns and will commit --
// chunkstore.NewInTx over the orchestrator's transaction. Persist begins
// no transaction, commits nothing, and rolls nothing back: on any error it
// returns with whatever it had already staged still staged, for the caller
// to discard by rolling back.
//
// A file whose Prepared entry has zero inputs still gets its
// ReplaceFileChunks call, with an empty slice -- ReplaceFileChunks's
// documented delete-without-inserting case. That is what drops the stale
// chunks of a file that now chunks to nothing, or that chunker.ChunkFiles
// could only emit a zero-unit entry for because it turned binary or
// stopped parsing (loam-8uo).
func (ix *Indexer) Persist(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, p Prepared) (Stats, error) {
	var stats Stats
	for _, f := range p.files {
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("replacing chunks for %s: %w", f.path, err)
		}
		if _, err := st.ReplaceFileChunks(ctx, repoID, targetBranch, f.path, f.inputs); err != nil {
			return stats, fmt.Errorf("replacing chunks for %s: %w", f.path, err)
		}
		stats.FilesReplaced++
		stats.ChunksWritten += len(f.inputs)
		if len(f.inputs) == 0 {
			stats.FilesWithoutChunks++
			ix.logger.InfoContext(ctx, "file chunked to zero units; dropping its prior chunks", "file", f.path, "repo_id", repoID, "target_branch", targetBranch)
		}
	}
	return stats, nil
}

// merge folds other's write-phase counters into s, which already carries
// the embed-phase ones. Prepare and Persist each populate a disjoint set
// of Stats fields, so this is an addition, never an overwrite.
func (s *Stats) merge(other Stats) {
	s.FilesReplaced += other.FilesReplaced
	s.FilesWithoutChunks += other.FilesWithoutChunks
	s.ChunksWritten += other.ChunksWritten
	s.EmbedCalls += other.EmbedCalls
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
