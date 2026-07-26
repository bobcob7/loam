// Package workbranchstore implements the work_branches aggregate over
// Postgres via the sqlc-generated queries in internal/db/gen
// (docs/persistence-spec.md "work_branches"). Store owns creating,
// reading, listing, and transitioning work branches: both the ordinary,
// agent-facing review lifecycle (docs/cli-spec.md "Its lifecycle": draft
// -> reviewable -> reviewed, plus the complete/closed terminal states) and
// the conflict state machine docs/git-spec.md "Target Advances &
// Catch-Up" describes -- a target-branch advance flags or demotes an
// affected work branch, and a catch-up push clears the flag (restoring a
// demoted branch directly to reviewable, no request-review needed).
//
// Every transition method maps to exactly one guarded
// UPDATE ... WHERE ... RETURNING * in internal/db/queries/work_branches.sql:
// the legal-from-state check and the write commit as a single atomic
// statement, so a concurrent racer never observes a transition applied
// from a state it was no longer valid in. An illegal call always surfaces
// as errIllegalTransition (or errNotFound, if the id itself does not
// exist) -- it is never a silent no-op success.
package workbranchstore

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . querier

// querier is the subset of *gen.Queries this package calls, defined here
// at the consumer per repo convention so Store can be unit-tested against
// a moq mock instead of a live database; *gen.Queries satisfies it in
// production without modification, whether constructed over a
// *pgxpool.Pool (New) or a pgx.Tx (NewInTx).
type querier interface {
	CreateWorkBranch(ctx context.Context, arg gen.CreateWorkBranchParams) (gen.WorkBranch, error)
	GetWorkBranchByID(ctx context.Context, id pgtype.UUID) (gen.WorkBranch, error)
	GetWorkBranchByName(ctx context.Context, arg gen.GetWorkBranchByNameParams) (gen.WorkBranch, error)
	ListWorkBranches(ctx context.Context, arg gen.ListWorkBranchesParams) ([]gen.WorkBranch, error)
	CountWorkBranches(ctx context.Context, arg gen.CountWorkBranchesParams) (int64, error)
	SetWorkBranchTitleDescription(ctx context.Context, arg gen.SetWorkBranchTitleDescriptionParams) (gen.WorkBranch, error)
	UpdateWorkBranchState(ctx context.Context, arg gen.UpdateWorkBranchStateParams) (gen.WorkBranch, error)
	CloseWorkBranch(ctx context.Context, arg gen.CloseWorkBranchParams) (gen.WorkBranch, error)
	CompleteWorkBranch(ctx context.Context, id pgtype.UUID) (gen.WorkBranch, error)
	FlagWorkBranchConflict(ctx context.Context, id pgtype.UUID) (gen.WorkBranch, error)
	DemoteWorkBranchOnConflict(ctx context.Context, id pgtype.UUID) (gen.WorkBranch, error)
	ClearWorkBranchConflict(ctx context.Context, id pgtype.UUID) (gen.WorkBranch, error)
}
