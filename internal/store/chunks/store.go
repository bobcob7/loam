package chunks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
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

// New builds a Store backed by pool. Callers must have already run
// migrations.Migrate against pool's DSN and built pool via internal/db.NewPool
// (its own doc comment explains why: the chunks_embedding vector column and
// pgvector type registration must exist before any query runs).
func New(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	return newStore(gen.New(pool), &pgxTransactor{pool: pool}, logger)
}

// newStore is New's unexported core, taking the queries/transactor seams
// directly so unit tests can supply moq mocks instead of a live pool.
func newStore(q queries, tx transactor, logger *slog.Logger) *Store {
	return &Store{queries: q, tx: tx, logger: logger}
}

// pgxTransactor is transactor's production implementation over a real
// *pgxpool.Pool.
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
	if s.logger != nil {
		s.logger.InfoContext(ctx, "replaced file chunks",
			"repo_id", repoID, "target_branch", targetBranch, "file", file, "chunks", len(result))
	}
	return result, nil
}

// Search returns the limit nearest chunks to embedding by cosine distance
// (the chunks_embedding HNSW index's vector_cosine_ops), restricted to
// targetBranch and to repoIDs — the scope's repo ids
// (docs/persistence-spec.md "chunks": "filtered by the scope repo ids").
// An empty repoIDs matches nothing: it is treated as an empty scope, not as
// "no filter", so a caller that forgot to populate its scope gets zero
// results rather than every repo's chunks.
func (s *Store) Search(ctx context.Context, repoIDs []uuid.UUID, targetBranch string, embedding []float32, limit int) ([]Chunk, error) {
	if len(repoIDs) == 0 {
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
// type, so callers depend on internal/store/chunks's own types rather than
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
