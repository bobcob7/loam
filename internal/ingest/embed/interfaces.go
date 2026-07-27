// Package embed defines the seam between the ingest pipeline and pluggable
// embedding backends (see docs/ingestion-spec.md, "Chunk -> Embed ->
// Vectors"). Concrete backends (the Ollama client, the deterministic test
// double in internal/testembed) live outside this package; consumers only
// ever import the Embedder interface declared here.
package embed

import "context"

//go:generate go tool moq -out moq_test.go . Embedder

// Embedder turns text chunks into vectors for RAG search and reports the
// metadata the ingest pipeline needs to manage the pgvector schema and
// full-rebuild triggers: Dimension pins chunks.embedding's vector(N)
// (docs/persistence-spec.md), and ModelID lets the pipeline detect a model
// change, which forces a full rebuild (docs/ingestion-spec.md). ContextWindow
// reports the model's token budget, so the chunker (internal/ingest/chunk,
// loam-zoa) can keep every chunk it produces under the limit that would
// otherwise make Embed fail loudly (truncate:false, loam-eg9) instead of
// discovering the problem only once embedding is attempted.
type Embedder interface {
	// Embed returns one vector per input text, in the same order as texts.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimension reports the fixed width of vectors this Embedder produces.
	Dimension() int
	// ModelID identifies the embedding model/version in use, so a change can
	// be detected and trigger a full rebuild.
	ModelID() string
	// ContextWindow reports the model's token budget: the maximum number of
	// tokens Embed can accept in a single input text before the request is
	// rejected (docs/ingestion-spec.md "Chunk -> Embed -> Vectors"). It
	// follows whichever model is configured -- a different Embedder
	// implementation or model reports its own value, so a caller must call
	// this rather than assume a constant.
	ContextWindow() int
}
