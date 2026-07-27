// Package chunk provides the chunk-time safety net loam-zoa adds to the
// ingest pipeline: EnforceBudget keeps any already-produced chunk unit
// under the configured embedding model's token budget, splitting an
// oversized one into embeddable pieces rather than letting it reach the
// embedder and fail (docs/ingestion-spec.md "Chunk -> Embed -> Vectors").
//
// This package does not itself decide chunk boundaries by symbol or
// section (that is loam-c94.10's chunker); EnforceBudget is the final step
// any chunk-producing source — code-by-symbol, docs-by-section, or the
// sliding-window fallback — runs its output through before chunks are
// handed to the Embedder.
package chunk

//go:generate go tool moq -out moq_test.go . Budgeter

// Budgeter is the seam EnforceBudget uses to learn the token budget it must
// keep every chunk under. embed.Embedder (internal/ingest/embed)
// implementations already satisfy this interface structurally —
// ContextWindow is one of Embedder's methods — but this package declares
// its own single-method interface rather than importing embed directly, so
// its tests mock exactly (and only) the one fact this package depends on
// (go-standards: "define interfaces where consumed").
type Budgeter interface {
	// ContextWindow reports the embedding model's token budget
	// (internal/ingest/embed.Embedder.ContextWindow).
	ContextWindow() int
}
