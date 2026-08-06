// Package vectors implements loam-c94.11, the tail of the chunk -> embed
// track (docs/ingestion-spec.md "Chunk -> Embed -> Vectors"): it takes the
// chunk units internal/ingest/chunker already produced for the changed
// files, embeds them through the internal/ingest/embed.Embedder seam, and
// persists them into the pgvector-backed chunks table via
// internal/chunkstore.Store's per-file delete-and-replace
// (ReplaceFileChunks).
//
// It is the exact mirror of internal/ingest/graph on the other track: one
// call the swap orchestrator (loam-c94.12) makes inside the single
// transaction it owns, immediately before or after the parse -> graph
// track's own writes, then COMMIT. Like that package, this one opens no
// transaction and commits nothing -- st is expected to be a
// chunkstore.Store built with NewInTx over the orchestrator's transaction,
// so every delete and insert IngestFileChunks makes is staged in that one
// commit.
//
// Scope, per this bead's own DESIGN: this package owns the per-file
// drop-then-insert for REPARSED files only. The deleted/renamed-file drops
// (diffplan.Plan.DropFiles) belong to the orchestrator, as does the commit.
//
// Embedding (Prepare) is still all-or-nothing: an embed failure, or a
// vector count/width that disagrees with the Embedder's own Dimension(),
// returns immediately rather than skipping the file, because Prepare has
// no partial result to skip past -- a file with no vector has nothing
// Persist could write for it anyway.
//
// Persist's per-file WRITES are not (loam-c94.21): a file the store
// rejects -- a malformed row, a constraint, a size limit -- is counted in
// Stats.FilesRejected, logged, and skipped, exactly the "skipped and
// counted, never silently" treatment chunker.ChunkFiles already gives a
// binary or unparseable file one step earlier. A cancelled context or an
// infrastructure-class failure (a dead pool, a closed transaction) is not
// survivable and still aborts the whole batch immediately -- see Persist's
// own doc comment for the exact line this bead draws and why.
//
// That per-file tolerance was necessary but NOT sufficient on its own to
// keep one bad file from costing a whole repo when st is bound to a shared
// transaction (chunkstore.NewInTx, which is how the swap orchestrator,
// loam-c94.12, actually constructs it): Postgres aborts the ENTIRE
// transaction, not just the offending statement, the moment any statement
// in it errors, so a rejection anywhere in the batch used to doom that
// transaction's COMMIT along with every file that had already landed.
// loam-c94.24 supplied the other half in chunkstore: ReplaceFileChunks
// wraps each call in a SAVEPOINT and unwinds to it on failure, so the
// rejection costs that one file's statements and leaves the transaction
// usable. Both halves together are what make "one bad file costs one file,
// not a repo" true end to end -- confirmed against a real server in this
// package's own integration tests, which commit a batch containing a
// rejected file and read back the other files' chunks afterward. See
// Persist's doc comment and
// TestIngestFileChunks_RejectionInASharedTransactionSparesTheRestOfTheBatch.
package vectors

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/chunkstore"
)

//go:generate go tool moq -out moq_test.go . embedder store

// embedder is the subset of internal/ingest/embed.Embedder this package
// depends on, declared here at the consumer (go-standards: "define
// interfaces where consumed") so its tests mock exactly the two facts it
// uses and nothing more. Both the production Ollama embedder
// (internal/ingest/embed/ollama) and the deterministic test double
// (internal/testembed) satisfy it unmodified.
//
// Embedder's other two methods are deliberately absent: ContextWindow is
// the chunker's business (it reaches EnforceBudget as chunk.Budgeter,
// before this package runs), and ModelID belongs to whatever decides a
// model change forces a full rebuild -- by the time chunk units arrive
// here that decision has already been made.
type embedder interface {
	// Embed returns one vector per input text, in the same order as texts.
	// It is a batch API, and IngestFileChunks uses it as one: chunk texts
	// are flattened across the whole file batch and sent in requests of up
	// to maxEmbedBatch texts each, not one request per chunk.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimension reports the fixed width every vector Embed returns must
	// have -- the width chunks.embedding's vector(N) is pinned to
	// (docs/persistence-spec.md). IngestFileChunks checks every returned
	// vector against it rather than trusting the embedder; see
	// errDimensionMismatch.
	Dimension() int
}

// store is the subset of *chunkstore.Store this package writes through:
// the per-file delete-and-replace that makes a re-embed idempotent
// (docs/persistence-spec.md "chunks"). Passing an empty inputs slice
// deletes the file's chunks without inserting any -- the path a reparsed
// file that now chunks to nothing takes, so its stale chunks still go
// away.
//
// Like internal/ingest/graph's own store seam, this is expected to be
// constructed over the transaction the swap orchestrator (loam-c94.12)
// owns -- chunkstore.NewInTx -- so nothing here auto-commits.
type store interface {
	ReplaceFileChunks(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error)
	// CountFileChunks reports how many chunks rows one file currently has
	// ON THIS TRANSACTION, which Persist asks only after a rejection: a
	// file that still has chunks is stale, one that has none is absent
	// (loam-qj21). It is part of this seam rather than a separate one
	// because it must be answered by the same store, on the same
	// transaction, as the write that was just unwound -- a count taken
	// anywhere else could not see a full rebuild's own uncommitted drop.
	CountFileChunks(ctx context.Context, repoID uuid.UUID, targetBranch, file string) (int, error)
}
