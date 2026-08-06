package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/diffplan"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/graph"
	"github.com/bobcob7/loam/internal/ingest/vectors"
	"github.com/bobcob7/loam/internal/reposstore"
)

// New builds the production Orchestrator, wiring the real planner, the
// real git content reader, both compute tracks, and the transaction-bound
// store adapters. This is what cmd/server/main.go constructs and hands to
// ingest.NewPool.
//
// pool is the database the ingest transaction is opened on. repos is the
// pool-bound repos store used for the two reads that happen BEFORE that
// transaction (the repo's name and the target branch's diff base) --
// deliberately not transaction-bound, since reading them inside the swap
// would extend the transaction for no benefit. ex and ch are the two
// compute tracks' engines, both typically sharing one
// *parser.ParserPool. ix embeds; emb supplies the two facts about the
// embedding model this package itself needs (the chunker's token budget
// and the model id that goes into the recorded version triple), and is
// normally the same value ix was constructed over.
//
// dataDir is LOAM_DATA_DIR: bare mirrors live at
// <dataDir>/mirrors/<group>/<repo_name>.git (internal/mirrorpath).
func New(
	logger *slog.Logger,
	dataDir string,
	pool *pgxpool.Pool,
	repos *reposstore.Store,
	ex *graph.Extractor,
	ch *chunker.Chunker,
	ix *vectors.Indexer,
	emb embedderInfo,
) *Orchestrator {
	return newOrchestrator(
		logger,
		dataDir,
		diffplan.New(logger),
		repos,
		newGitReader(logger),
		graphAdapter{extractor: ex, logger: logger},
		ch,
		vectorAdapter{indexer: ix, logger: logger},
		storeDropper{logger: logger},
		refAdapter{logger: logger},
		ledgerAdapter{pool: pool, logger: logger},
		&pgxTransactor{pool: pool},
		emb,
		diffplan.Versions{Grammar: GrammarVersion, Pipeline: PipelineVersion, EmbeddingModel: emb.ModelID()},
	)
}

// pgxTransactor is transactor's production implementation: one fresh
// transaction per withinTx call, committed if and only if fn returns nil.
// The deferred Rollback after a successful Commit is a documented no-op in
// pgx (it returns pgx.ErrTxClosed, discarded here), which is what makes
// the single deferred rollback safe to register unconditionally -- and
// what makes a panic inside fn still close the transaction on the way out.
type pgxTransactor struct {
	pool *pgxpool.Pool
}

func (t *pgxTransactor) withinTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning ingest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing ingest transaction: %w", err)
	}
	return nil
}

// graphAdapter binds *graph.Extractor to the graphTrack seam. The only
// thing it adds is constructing a codegraph.Store over the caller's
// transaction for the write half -- graph.Extractor's own store parameter
// is an unexported interface in that package, so no interface declared
// here could name it.
type graphAdapter struct {
	extractor *graph.Extractor
	logger    *slog.Logger
}

func (a graphAdapter) Extract(ctx context.Context, files []graph.FileInput) (graph.Extracted, graph.Stats, error) {
	return a.extractor.ExtractFiles(ctx, files)
}

func (a graphAdapter) Persist(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, ex graph.Extracted) (graph.Stats, error) {
	return a.extractor.PersistFiles(ctx, codegraph.NewInTx(tx, a.logger), repoID, targetBranch, ex)
}

// vectorAdapter binds *vectors.Indexer to the vectorTrack seam, for the
// same reason graphAdapter exists: the write half needs a chunkstore.Store
// bound to the caller's transaction, and vectors' own store parameter is
// an unexported interface there.
type vectorAdapter struct {
	indexer *vectors.Indexer
	logger  *slog.Logger
}

func (a vectorAdapter) Prepare(ctx context.Context, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (vectors.Prepared, vectors.Stats, error) {
	return a.indexer.Prepare(ctx, repoID, targetBranch, files)
}

func (a vectorAdapter) Persist(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, p vectors.Prepared) (vectors.Stats, error) {
	return a.indexer.Persist(ctx, chunkstore.NewInTx(tx, a.logger), repoID, targetBranch, p)
}

// storeDropper is dropper's production implementation over the two stores
// that own derived rows. Every call binds a fresh Store to the caller's
// transaction (both constructors are trivial struct literals, so this
// costs nothing) rather than holding a Store built over a pool, which
// would commit outside the swap.
type storeDropper struct {
	logger *slog.Logger
}

// DropRepoBranch is the full-rebuild drop: every symbols, symbol_references
// and chunks row for the repo+branch, with graph_edges and symbol_history
// following the symbols by ON DELETE CASCADE.
func (d storeDropper) DropRepoBranch(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string) error {
	if err := codegraph.NewInTx(tx, d.logger).DropRepoBranch(ctx, repoID, targetBranch); err != nil {
		return err
	}
	return chunkstore.NewInTx(tx, d.logger).DropRepoBranch(ctx, repoID, targetBranch)
}

// DropPaths is the incremental drop, applied to diffplan.Plan.DropFiles:
// deleted files and a rename's or copy's OLD path. Each table's existing
// per-file replace call, given an empty input, IS the delete -- that is
// what their doc comments call the "delete without inserting" case -- so
// this needs no additional query.
//
// An empty paths makes no store call at all: the common incremental ingest
// deletes nothing, and issuing three no-op statements per ingest for that
// case is pure overhead inside a transaction whose whole point is to be
// short.
func (d storeDropper) DropPaths(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	cg := codegraph.NewInTx(tx, d.logger)
	cs := chunkstore.NewInTx(tx, d.logger)
	for _, path := range paths {
		if _, err := cg.ReplaceFileSymbols(ctx, repoID, targetBranch, path, nil); err != nil {
			return fmt.Errorf("dropping symbols for %s: %w", path, err)
		}
		if _, err := cg.ReplaceFileReferences(ctx, repoID, targetBranch, path, nil); err != nil {
			return fmt.Errorf("dropping symbol references for %s: %w", path, err)
		}
		if _, err := cs.ReplaceFileChunks(ctx, repoID, targetBranch, path, nil); err != nil {
			return fmt.Errorf("dropping chunks for %s: %w", path, err)
		}
	}
	return nil
}

// refAdapter is refWriter's production implementation: the ingest's own
// write-back onto repo_target_branches, bound to the swap transaction so
// the recorded diff base commits with the index it describes and never
// separately from it.
type refAdapter struct {
	logger *slog.Logger
}

func (a refAdapter) AdvanceIngestedRef(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, branch, ref string, ingestedAt time.Time, versions []byte) error {
	_, err := reposstore.NewStoreInTx(tx, a.logger).AdvanceIngestedRef(ctx, repoID, branch, ref, ingestedAt, versions)
	return err
}

// ledgerAdapter is rejectionLedger's production implementation over
// chunkstore.Rejections (loam-qj21). It is the one adapter here that binds
// to BOTH a pool and a transaction, because the seam genuinely straddles
// the transaction boundary: List runs before the swap opens, since its
// result is an input to the plan, while every write must be staged inside
// the swap so the ledger can never disagree with what committed. Holding
// the pool and taking the tx per write call is what expresses that split
// without letting a caller accidentally write through the pool.
type ledgerAdapter struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func (a ledgerAdapter) List(ctx context.Context, repoID uuid.UUID, targetBranch string) ([]chunkstore.Rejection, error) {
	return chunkstore.NewRejections(a.pool, a.logger).List(ctx, repoID, targetBranch)
}

func (a ledgerAdapter) Record(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, in chunkstore.RejectionInput) error {
	return chunkstore.NewRejectionsInTx(tx, a.logger).Record(ctx, repoID, targetBranch, in)
}

func (a ledgerAdapter) Clear(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, paths []string) error {
	return chunkstore.NewRejectionsInTx(tx, a.logger).Clear(ctx, repoID, targetBranch, paths)
}

func (a ledgerAdapter) ClearAll(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string) error {
	return chunkstore.NewRejectionsInTx(tx, a.logger).ClearAll(ctx, repoID, targetBranch)
}
