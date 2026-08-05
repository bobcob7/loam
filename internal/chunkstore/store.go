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

// ErrTransactionUnusable means the failure was NOT confined to the one
// file's statements: the SAVEPOINT machinery savepointTransactor wraps
// every NewInTx write in could not be driven, so the caller's shared
// transaction (and possibly the connection under it) is gone, and every
// later write on it will fail the same way.
//
// It is exported because the classification is only decidable HERE and is
// consumed THERE: internal/ingest/vectors.Persist treats a plain
// ReplaceFileChunks error as a per-file rejection and keeps going, which
// is right for a bad row and catastrophic for a dead connection -- a dead
// connection would otherwise be re-attempted once per remaining file,
// turning one infrastructure failure into N (loam-c94.24). Matching on
// this sentinel is strictly better than inspecting the error's shape:
// a raw TCP death carries no *pgconn.PgError and matches none of pgx's
// named sentinels, but it is unmissable here, because ROLLBACK TO
// SAVEPOINT is itself a statement that has to reach the server. A
// rejection this store CAN unwind never carries it -- the fn error is
// returned bare in that case, exactly as before.
var ErrTransactionUnusable = errors.New("chunks: shared transaction is no longer usable")

// fileSavepoint is the savepoint name savepointTransactor establishes
// around each call. One fixed name is safe because the calls are strictly
// sequential and every one of them is balanced -- RELEASE on success,
// ROLLBACK TO then RELEASE on failure -- so no two are ever live at once
// and none can accumulate on the server across a long batch. It is
// prefixed rather than something like "sp" so it cannot collide with a
// savepoint pgx's own tx.Begin() (which uses sp_<n>) might establish on
// the same transaction.
const fileSavepoint = "loam_chunkstore_file"

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
// caller owns: every write runs directly against tx, and this Store
// neither begins nor commits a transaction of its own, so a caller
// composing several stores' writes into one commit -- the atomic swap
// loam-c94.12 orchestrates -- can hand each store the same tx and be
// certain none of them opens a competing one. Search also reads through
// tx, so it sees this transaction's own uncommitted writes, consistent
// with every other read inside it. The caller alone decides when tx
// commits or rolls back; nothing here can reach Commit or Rollback
// (savepointExecer is one method wide, and that method is Exec).
//
// Each write IS wrapped in its own SAVEPOINT (loam-c94.24). Postgres
// aborts the ENTIRE transaction the instant any statement in it errors --
// every later statement then fails with SQLSTATE 25P02 and the commit
// itself fails with "commit unexpectedly resulted in rollback" -- so
// without a savepoint one rejected file would discard every other file in
// the batch, and every write the caller had already staged alongside them,
// no matter how gracefully the caller's loop skipped past it. The
// savepoint confines that blast radius to the one call: on failure the
// call's own statements are unwound with ROLLBACK TO SAVEPOINT and the
// transaction is left usable for the next file. See savepointTransactor.
func NewInTx(tx pgx.Tx, logger *slog.Logger) *Store {
	q := gen.New(tx)
	return newStore(q, &savepointTransactor{tx: tx, q: q}, logger)
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

// savepointTransactor is transactor's implementation for NewInTx. q is
// already bound to the caller's open transaction, so fn runs against it
// directly; what this type adds is a SAVEPOINT around fn and a ROLLBACK TO
// SAVEPOINT if fn fails, which is the difference between "this file's
// chunks were not written" and "this transaction is dead and everything in
// it is lost" (loam-c94.24).
//
// It calls no Begin, Commit or Rollback -- it cannot, savepointExecer
// exposes only Exec -- so the caller's transaction lifecycle stays
// entirely the caller's. A savepoint is not a nested transaction: it takes
// no new snapshot, holds no separate locks, and commits nothing; it is a
// rollback marker inside the one transaction the caller will commit.
//
// The success path costs two extra round trips per call (SAVEPOINT,
// RELEASE SAVEPOINT) and the failure path three. Measured, not assumed
// (BenchmarkReplaceFileChunks_SavepointOverhead, integration_test.go): on
// a 500-file x 4-chunk batch against a real server the two statements cost
// ~450us per file, ~12% on top of the batch's own ~1.6s -- and they cost
// EXACTLY what a bare "SELECT 1" costs on the same connection, to within
// the measurement's noise. That is the number that matters: SAVEPOINT and
// RELEASE do no server-side work worth measuring, so the whole overhead is
// client-server round-trip latency, which is a property of where Postgres
// is deployed rather than of this change. There is no cheaper correct
// version -- see the benchmark's doc comment for why a narrower savepoint
// around "only the statements that can be rejected" is the same savepoint.
type savepointTransactor struct {
	tx savepointExecer
	q  queries
}

// withinTx establishes the savepoint, runs fn, and then either releases the
// savepoint (fn succeeded) or rolls back to it and releases it (fn failed).
//
// The RELEASE after a ROLLBACK TO is not redundant: ROLLBACK TO SAVEPOINT
// undoes the statements but leaves the savepoint itself established, so
// omitting the release would leave one savepoint per rejected file alive
// on the server for the rest of the batch.
//
// fn's own error is returned unwrapped whenever the unwind succeeded --
// that is the signal "only this file failed, the transaction is fine."
// Any failure of the three savepoint statements themselves is returned
// wrapped in ErrTransactionUnusable instead, carrying fn's error along in
// the message when there was one, because at that point the caller must
// stop rather than try the next file.
func (t *savepointTransactor) withinTx(ctx context.Context, fn func(q queries) error) error {
	if _, err := t.tx.Exec(ctx, "SAVEPOINT "+fileSavepoint); err != nil {
		return fmt.Errorf("%w: establishing savepoint: %w", ErrTransactionUnusable, err)
	}
	if fnErr := fn(t.q); fnErr != nil {
		if _, err := t.tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+fileSavepoint); err != nil {
			return fmt.Errorf("%w: rolling back to savepoint after %v: %w", ErrTransactionUnusable, fnErr, err)
		}
		if _, err := t.tx.Exec(ctx, "RELEASE SAVEPOINT "+fileSavepoint); err != nil {
			return fmt.Errorf("%w: releasing savepoint after rolling back %v: %w", ErrTransactionUnusable, fnErr, err)
		}
		return fnErr
	}
	if _, err := t.tx.Exec(ctx, "RELEASE SAVEPOINT "+fileSavepoint); err != nil {
		return fmt.Errorf("%w: releasing savepoint: %w", ErrTransactionUnusable, err)
	}
	return nil
}

// ReplaceFileChunks atomically deletes every existing chunk for
// (repoID, targetBranch, file) and inserts inputs in its place, so an
// incremental re-embed only touches the file that actually changed
// (docs/persistence-spec.md "chunks"; docs/ingestion-spec.md "Incremental
// Build" — "only changed files are re-embedded"). Passing an empty inputs
// deletes the file's chunks without inserting any, e.g. for a file removed
// from the tree.
//
// The delete and every insert are one atomic unit, both ways this Store
// can be built: a real transaction under New, a SAVEPOINT under NewInTx.
// A failure partway through therefore leaves the file's prior chunks
// intact rather than half-replaced -- the DELETE unwinds together with
// whatever INSERTs got as far as the server, so the file keeps the chunks
// it had before the call, stale but whole.
//
// Under NewInTx a failure ALSO leaves the caller's transaction usable, so
// a batch loop may treat one file's rejection as that file's own problem
// and carry on (internal/ingest/vectors.Persist does exactly that), with
// one exception it must respect: an error matching ErrTransactionUnusable
// means the savepoint could not be driven at all, the transaction is gone,
// and continuing is pointless.
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
