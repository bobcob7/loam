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
// Failure is all-or-nothing here, unlike chunker.ChunkFiles's per-file
// resilience: an embed failure or a store failure returns immediately
// rather than skipping the file and continuing. That is not a stylistic
// difference, it is docs/ingestion-spec.md "Consistency & Failure" --
// "stale-but-consistent is the rule: on any failure, including an
// unreachable embedder, nothing commits" -- so there is nothing to gain
// from pressing on into a transaction that is going to roll back. The
// per-file tolerance that does exist lives one step earlier, in
// ChunkFiles, which drops binary and unparseable files from the batch
// before this package ever sees them.
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
}
