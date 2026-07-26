package reposstore

import (
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/bobcob7/loam/internal/db/gen"
)

// defaultListLimit is the page size Store.ListRepos uses when the caller's
// Page.Limit is non-positive, matching proto's loam.v1.Page contract that
// limit 0 means "use the server default."
const defaultListLimit = 50

// Store is the repos + repo_target_branches aggregate store
// (docs/persistence-spec.md "repos", "repo_target_branches"). Construct
// with NewStore, passing the real *gen.Queries in production (wired in
// cmd/server/main.go over a *pgxpool.Pool) or a querier mock in tests.
type Store struct {
	db     querier
	logger *slog.Logger
}

// NewStore builds a Store over db, typically a *gen.Queries constructed over
// either a *pgxpool.Pool (standalone reads/writes) or a pgx.Tx (atomic with
// other stores' writes in the same transaction, e.g. AdvanceIngestedRef
// alongside loam-c94.11/loam-c94.12's other writes -- docs/ingestion-spec.md
// "Consistency & Failure": "the ingested ref" is recorded "in the same
// transaction"). See NewStoreInTx for the latter as a named entry point
// matching this package's siblings.
func NewStore(db querier, logger *slog.Logger) *Store {
	return &Store{db: db, logger: logger}
}

// NewStoreInTx builds a Store bound to tx, an already-open transaction the
// caller owns and will commit or roll back itself: it is exactly
// NewStore(gen.New(tx), logger), given a name so callers composing several
// stores' writes into one commit have one consistent constructor to reach
// for across every store package. Store never calls
// tx.Begin/Commit/Rollback itself, so there is no nested-transaction path to
// guard against here.
func NewStoreInTx(tx pgx.Tx, logger *slog.Logger) *Store {
	return NewStore(gen.New(tx), logger)
}
