// Package chunkstore implements the RAG vector store over the
// pgvector-backed chunks table (docs/persistence-spec.md "chunks"): per-file
// delete-and-replace on re-embed, and nearest-neighbour search over the
// chunks_embedding HNSW index, scoped to a caller-supplied set of repo ids.
package chunkstore

import (
	"context"

	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . queries transactor

// queries is the sqlc-generated surface Store calls, defined here at the
// consumer so Store is unit-testable against a moq mock instead of a live
// database. *gen.Queries satisfies it unmodified, whether it is bound
// directly to the pool (Search — a read that needs no transaction) or to a
// transaction via gen.New(tx) (ReplaceFileChunks's delete-then-insert).
type queries interface {
	InsertChunk(ctx context.Context, arg gen.InsertChunkParams) (gen.Chunk, error)
	DeleteChunksByFile(ctx context.Context, arg gen.DeleteChunksByFileParams) error
	SearchChunksByEmbeddingScoped(ctx context.Context, arg gen.SearchChunksByEmbeddingScopedParams) ([]gen.Chunk, error)
}

// transactor runs fn inside a single database transaction, committing if fn
// returns nil and rolling back otherwise. A panic inside fn is not
// recovered here — it unwinds past the deferred rollback in the production
// implementation, so the transaction is never left dangling open. It
// exists so ReplaceFileChunks's delete-and-replace commits atomically: a
// crash or error between the delete and the inserts must never leave a
// file with no chunks, or a mix of stale and fresh ones. Routing this
// through a single method (rather than exposing Begin/Commit/Rollback on
// the interface) also keeps Store unit-testable without faking pgx.Tx's
// much larger surface (Commit, Rollback, Prepare, CopyFrom, SendBatch,
// ...): a moq mock for transactor just invokes fn against a queriesMock
// directly.
type transactor interface {
	withinTx(ctx context.Context, fn func(q queries) error) error
}
