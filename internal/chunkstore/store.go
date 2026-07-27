package chunkstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/bobcob7/loam/internal/db/gen"
)

// errInvalidUUID means a row read back from Postgres carried a NULL or
// otherwise invalid uuid column where chunks' schema (0002_code_intel.up.sql)
// declares NOT NULL primary/foreign keys — a schema-contract violation the
// caller cannot recover from, distinguished from a plain decode error so it
// can be matched with errors.Is if a caller ever needs to.
var errInvalidUUID = errors.New("chunks: invalid uuid column")

// Chunk is one row of a RAG chunk: a file-scoped span of source text plus
// the embedding vector search ranks it by (docs/persistence-spec.md
// "chunks").
type Chunk struct {
	ID           uuid.UUID
	RepoID       uuid.UUID
	TargetBranch string
	File         string
	StartLine    int
	EndLine      int
	Content      string
	Embedding    []float32
	CreatedAt    time.Time
}

// ChunkInput is one chunk to persist via ReplaceFileChunks: everything
// InsertChunk needs beyond the identifiers ReplaceFileChunks itself
// supplies (a fresh id, repoID, targetBranch, file).
type ChunkInput struct {
	StartLine int
	EndLine   int
	Content   string
	Embedding []float32
}

// Store implements the RAG chunks store: per-file delete-and-replace on
// re-embed, and nearest-neighbour search over the chunks_embedding HNSW
// index scoped to a caller-supplied set of repo ids
// (docs/persistence-spec.md "chunks"). Construct with New.
type Store struct {
	queries queries
	tx      transactor
	logger  *slog.Logger
}

// New builds a Store backed by pool: every ReplaceFileChunks call opens and
// commits its own transaction (via pgxTransactor), the right shape for a
// standalone caller that is not composing this write with any other
// store's. Callers must have already run migrations.Migrate against pool's
// DSN and built pool via internal/db.NewPool (its own doc comment explains
// why: the chunks_embedding vector column and pgvector type registration
// must exist before any query runs).
func New(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	return newStore(gen.New(pool), &pgxTransactor{pool: pool}, logger)
}

// NewInTx builds a Store bound to tx, an already-open transaction the
// caller owns: every ReplaceFileChunks call runs its delete-then-insert
// directly against tx and neither begins nor commits anything itself
// (passthroughTransactor never calls tx.Begin), so a caller composing
// several stores' writes into one commit -- the atomic swap loam-c94.12
// orchestrates -- can hand each store the same tx and be certain none of
// them opens a nested transaction of its own. Search also reads through tx,
// so it sees this transaction's own uncommitted writes, consistent with
// every other read inside it. The caller alone decides when tx commits or
// rolls back.
func NewInTx(tx pgx.Tx, logger *slog.Logger) *Store {
	q := gen.New(tx)
	return newStore(q, &passthroughTransactor{q: q}, logger)
}

// newStore is New's and NewInTx's unexported core, taking the
// queries/transactor seams directly so unit tests can supply moq mocks
// instead of a live pool or transaction.
func newStore(q queries, tx transactor, logger *slog.Logger) *Store {
	return &Store{queries: q, tx: tx, logger: logger}
}

// pgxTransactor is transactor's production implementation over a real
// *pgxpool.Pool: it owns the full begin/commit/rollback lifecycle of a
// fresh transaction per withinTx call, for New's standalone-caller shape.
type pgxTransactor struct {
	pool *pgxpool.Pool
}

func (t *pgxTransactor) withinTx(ctx context.Context, fn func(q queries) error) error {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(gen.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// passthroughTransactor is transactor's implementation for NewInTx: q is
// already bound to the caller's open transaction, so withinTx just invokes
// fn against it. There is no Begin/Commit/Rollback call anywhere in this
// type -- not merely an unused one -- so a Store built via NewInTx cannot
// nest a transaction inside the caller's, whether by a savepoint or by
// mistake; the code path to do so does not exist.
type passthroughTransactor struct {
	q queries
}

func (t *passthroughTransactor) withinTx(_ context.Context, fn func(q queries) error) error {
	return fn(t.q)
}

// ReplaceFileChunks atomically deletes every existing chunk for
// (repoID, targetBranch, file) and inserts inputs in its place, so an
// incremental re-embed only touches the file that actually changed
// (docs/persistence-spec.md "chunks"; docs/ingestion-spec.md "Incremental
// Build" — "only changed files are re-embedded"). Passing an empty inputs
// deletes the file's chunks without inserting any, e.g. for a file removed
// from the tree. The delete and every insert commit as one transaction: a
// failure partway through leaves the file's prior chunks intact rather
// than half-replaced.
func (s *Store) ReplaceFileChunks(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []ChunkInput) ([]Chunk, error) {
	result := make([]Chunk, 0, len(inputs))
	err := s.tx.withinTx(ctx, func(q queries) error {
		if err := q.DeleteChunksByFile(ctx, gen.DeleteChunksByFileParams{
			RepoID:       pgUUID(repoID),
			TargetBranch: targetBranch,
			File:         file,
		}); err != nil {
			return fmt.Errorf("deleting existing chunks for %s: %w", file, err)
		}
		for _, in := range inputs {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generating chunk id: %w", err)
			}
			row, err := q.InsertChunk(ctx, gen.InsertChunkParams{
				ID:           pgUUID(id),
				RepoID:       pgUUID(repoID),
				TargetBranch: targetBranch,
				File:         file,
				StartLine:    int32(in.StartLine),
				EndLine:      int32(in.EndLine),
				Content:      in.Content,
				Embedding:    pgvector.NewVector(in.Embedding),
			})
			if err != nil {
				return fmt.Errorf("inserting chunk for %s:%d-%d: %w", file, in.StartLine, in.EndLine, err)
			}
			chunk, err := fromGenChunk(row)
			if err != nil {
				return fmt.Errorf("decoding inserted chunk: %w", err)
			}
			result = append(result, chunk)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("replacing chunks for repo %s file %s: %w", repoID, file, err)
	}
	s.logger.InfoContext(ctx, "replaced file chunks",
		"repo_id", repoID, "target_branch", targetBranch, "file", file, "chunks", len(result))
	return result, nil
}

// DropRepoBranch deletes every chunks row for (repoID, targetBranch) --
// the repo-scoped drop the full-rebuild path runs before re-embedding the
// whole tree (docs/ingestion-spec.md "Incremental Build" -> "Full
// rebuild"; loam-c94.12's own DESCRIPTION: "Full-rebuild path drops all
// repo+target derived rows first").
//
// It is deliberately NOT expressible as a loop of ReplaceFileChunks(nil)
// over the new tree's files: a full rebuild exists precisely for the cases
// where there is no usable diff (force-push, history rewrite, first
// ingest, version bump), so chunks may exist for files that the new tree
// does not contain at all and a per-file loop keyed on the new tree can
// never name them.
//
// Like ReplaceFileChunks, it runs through the Store's transactor -- so a
// Store built with NewInTx stages this delete in the caller's transaction
// (nothing is visible to readers until that one commit), while a Store
// built with New commits it on its own.
func (s *Store) DropRepoBranch(ctx context.Context, repoID uuid.UUID, targetBranch string) error {
	err := s.tx.withinTx(ctx, func(q queries) error {
		return q.DeleteChunksForRepoBranch(ctx, gen.DeleteChunksForRepoBranchParams{
			RepoID:       pgUUID(repoID),
			TargetBranch: targetBranch,
		})
	})
	if err != nil {
		return fmt.Errorf("dropping chunks for repo %s branch %s: %w", repoID, targetBranch, err)
	}
	s.logger.InfoContext(ctx, "dropped repo branch chunks", "repo_id", repoID, "target_branch", targetBranch)
	return nil
}

// Search returns the limit nearest chunks to embedding by cosine distance
// (the chunks_embedding HNSW index's vector_cosine_ops), restricted to
// targetBranch and to repoIDs — the scope's repo ids
// (docs/persistence-spec.md "chunks": "filtered by the scope repo ids").
// An empty repoIDs matches nothing: it is treated as an empty scope, not as
// "no filter", so a caller that forgot to populate its scope gets zero
// results rather than every repo's chunks. A non-positive limit is treated
// the same way (zero results) rather than reaching Postgres as `LIMIT 0`
// (a wasted round trip) or `LIMIT -1` (a syntax error): callers ask for "no
// more than limit results", and asking for at most zero (or fewer) has
// exactly one sensible answer.
//
// Whether an unforced call here actually reaches chunks_embedding depends
// on table size and is measured, not assumed -- see the DECISION comment on
// SearchChunksByEmbeddingScoped (internal/db/queries/chunks.sql) for the
// numbers, the crossover point, and a caveat band worth knowing about
// (loam-962, loam-l73).
func (s *Store) Search(ctx context.Context, repoIDs []uuid.UUID, targetBranch string, embedding []float32, limit int) ([]Chunk, error) {
	if len(repoIDs) == 0 || limit <= 0 {
		return nil, nil
	}
	ids := make([]pgtype.UUID, len(repoIDs))
	for i, id := range repoIDs {
		ids[i] = pgUUID(id)
	}
	rows, err := s.queries.SearchChunksByEmbeddingScoped(ctx, gen.SearchChunksByEmbeddingScopedParams{
		Column1:      ids,
		TargetBranch: targetBranch,
		Embedding:    pgvector.NewVector(embedding),
		Limit:        int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("searching chunks by embedding: %w", err)
	}
	result := make([]Chunk, 0, len(rows))
	for _, row := range rows {
		chunk, err := fromGenChunk(row)
		if err != nil {
			return nil, fmt.Errorf("decoding searched chunk: %w", err)
		}
		result = append(result, chunk)
	}
	return result, nil
}

// pgUUID adapts a uuid.UUID to the pgtype.UUID sqlc's generated params
// expect; uuid.UUID and pgtype.UUID's Bytes are both a plain [16]byte in
// the same byte order, so this is a direct field copy, not a reparse.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// fromGenChunk converts a sqlc-generated row into the store's domain Chunk
// type, so callers depend on internal/chunkstore's own types rather than
// reaching into internal/db/gen directly.
func fromGenChunk(row gen.Chunk) (Chunk, error) {
	id, err := uuidFromPg(row.ID)
	if err != nil {
		return Chunk{}, fmt.Errorf("chunk id: %w", err)
	}
	repoID, err := uuidFromPg(row.RepoID)
	if err != nil {
		return Chunk{}, fmt.Errorf("chunk repo_id: %w", err)
	}
	return Chunk{
		ID:           id,
		RepoID:       repoID,
		TargetBranch: row.TargetBranch,
		File:         row.File,
		StartLine:    int(row.StartLine),
		EndLine:      int(row.EndLine),
		Content:      row.Content,
		Embedding:    row.Embedding.Slice(),
		CreatedAt:    row.CreatedAt.Time,
	}, nil
}

func uuidFromPg(id pgtype.UUID) (uuid.UUID, error) {
	if !id.Valid {
		return uuid.UUID{}, errInvalidUUID
	}
	return uuid.UUID(id.Bytes), nil
}
