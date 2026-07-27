// Package reposstore implements the repos aggregate store: repos.name is
// the natural key callers hold, an "<group>/<repo_name>" identifier
// (loam-54o.7 NOTES on the settled RepoID decision -- the proto surface
// never exposes repos.id to any client, docs/persistence-spec.md "repos").
// FKs across the schema still reference repos.id, so this package is where
// that resolution happens: GetRepoByName is the single indexed lookup
// (repos_name_key UNIQUE (name), 0001_init.up.sql) every other caller
// should use to turn a held name into the id it needs. It also owns
// repo_target_branches: the branches eligible as work-branch targets, and
// the incremental-ingest diff base (ingested_ref) loam-c94.2's planner
// reads before every ingest.
package reposstore

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

// querier is the subset of the sqlc-generated gen.Queries this store
// consumes, defined here at the consumer so Store's own tests can drive it
// against a moq mock instead of a real database. The production
// implementation is *gen.Queries itself (constructed over a *pgxpool.Pool
// via gen.New), which satisfies this interface structurally.
type querier interface {
	CreateRepo(ctx context.Context, arg gen.CreateRepoParams) (gen.Repo, error)
	GetRepoByID(ctx context.Context, id pgtype.UUID) (gen.Repo, error)
	GetRepoByName(ctx context.Context, name string) (gen.Repo, error)
	ListRepos(ctx context.Context, arg gen.ListReposParams) ([]gen.Repo, error)
	CountRepos(ctx context.Context) (int64, error)
	ListRepoNames(ctx context.Context) ([]string, error)
	UpdateRepo(ctx context.Context, arg gen.UpdateRepoParams) (gen.Repo, error)
	UpdateRepoSyncState(ctx context.Context, arg gen.UpdateRepoSyncStateParams) (gen.Repo, error)
	AddTargetBranch(ctx context.Context, arg gen.AddTargetBranchParams) (gen.RepoTargetBranch, error)
	ListTargetBranches(ctx context.Context, repoID pgtype.UUID) ([]gen.RepoTargetBranch, error)
	GetTargetBranch(ctx context.Context, arg gen.GetTargetBranchParams) (gen.RepoTargetBranch, error)
	RemoveTargetBranch(ctx context.Context, arg gen.RemoveTargetBranchParams) (int64, error)
	AdvanceIngestedRef(ctx context.Context, arg gen.AdvanceIngestedRefParams) (gen.RepoTargetBranch, error)
}

//go:generate go tool moq -out moq_test.go . querier
