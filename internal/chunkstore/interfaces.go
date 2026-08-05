// Package chunkstore implements the RAG vector store over the
// pgvector-backed chunks table (docs/persistence-spec.md "chunks"): per-file
// delete-and-replace on re-embed, and nearest-neighbour search over the
// chunks_embedding HNSW index, scoped to a caller-supplied set of repo ids.
// Construct with New for a standalone caller (each ReplaceFileChunks call
// commits on its own), or NewInTx to bind the Store to a transaction the
// caller already opened and will commit itself -- see New's and NewInTx's
// doc comments.
package chunkstore

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . queries transactor savepointExecer

// queries is the sqlc-generated surface Store calls, defined here at the
// consumer so Store is unit-testable against a moq mock instead of a live
// database. *gen.Queries satisfies it unmodified, whether it is bound
// directly to the pool (Search — a read that needs no transaction) or to a
// transaction via gen.New(tx) (ReplaceFileChunks's delete-then-insert).
type queries interface {
	InsertChunk(ctx context.Context, arg gen.InsertChunkParams) (gen.Chunk, error)
	DeleteChunksByFile(ctx context.Context, arg gen.DeleteChunksByFileParams) error
	DeleteChunksForRepoBranch(ctx context.Context, arg gen.DeleteChunksForRepoBranchParams) error
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

// savepointExecer is the ONE method savepointTransactor needs from the
// pgx.Tx a NewInTx caller hands over: the ability to issue a bare SQL
// statement on that transaction's connection, which is how SAVEPOINT /
// RELEASE SAVEPOINT / ROLLBACK TO SAVEPOINT are issued (loam-c94.24).
// Declared here at the consumer, one method wide, so a unit test can pin
// the exact statement sequence with a moq mock instead of needing a live
// server -- and so nothing in this package can reach Commit or Rollback on
// the caller's transaction, which remains the caller's alone to decide.
//
// pgx.Tx satisfies this unmodified. pgx also offers tx.Begin(), which
// implements a pseudo-nested transaction with its own internally-numbered
// savepoint; this package issues the statements itself instead, because
// the failure this whole mechanism exists to survive is a statement error
// mid-transaction, and being able to assert "SAVEPOINT, then ROLLBACK TO,
// then RELEASE, in that order, and nothing else" in a plain unit test is
// worth more here than reusing pgx's counter.
type savepointExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
