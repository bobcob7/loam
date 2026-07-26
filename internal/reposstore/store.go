package reposstore

import "log/slog"

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

// NewStore builds a Store over db.
func NewStore(db querier, logger *slog.Logger) *Store {
	return &Store{db: db, logger: logger}
}
