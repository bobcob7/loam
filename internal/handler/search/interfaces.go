// Package search implements loam.v1.SearchService: Search, the single RPC
// backing `loam search` -- natural-language semantic search over ingested
// docs/code (docs/cli-spec.md "RAG queries (search)"). Unlike GraphService,
// `--all` genuinely spans repos: Search merges and globally re-ranks
// results across every repo in scope by score, rather than fanning out
// per-repo and unioning with no cross-repo ordering.
package search

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/chunkstore"
)

//go:generate go tool moq -out moq_test.go . ChunkStore Embedder

// ChunkStore is the internal/chunkstore.Store surface Search needs, defined
// here at the consumer per repo convention. *chunkstore.Store satisfies it
// structurally in production; tests drive a moq mock instead of a live
// database.
type ChunkStore interface {
	// Search returns up to limit chunks nearest to embedding, restricted to
	// repoIDs (one repo per call -- see Handler.Search's own doc comment
	// for why) and targetBranch.
	Search(ctx context.Context, repoIDs []uuid.UUID, targetBranch string, embedding []float32, limit int) ([]chunkstore.Chunk, error)
}

// Embedder turns a search query into a vector, mirroring
// internal/ingest/embed.Embedder's Embed method -- the single method this
// package actually needs, defined here at the consumer rather than
// depending on that package's fuller interface (Dimension/ModelID/
// ContextWindow, none of which Search reads).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
