package gitanchor

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/reposstore"
)

//go:generate go tool moq -out moq_test.go . RepoStore

// RepoStore is the internal/reposstore.Store surface Checker needs: a work
// branch (workbranchstore.WorkBranch) carries only its repo's RepoID, not
// its name, and mirrorpath.Dir derives the bare mirror's on-disk path from
// the repo's name, not its id -- so resolving RepoID to that name is the
// one lookup Checker needs before it can find the mirror at all. Defined
// here at the consumer per repo convention, matching internal/gitdiff's own
// RepoStore; *reposstore.Store satisfies it directly in production, tests
// drive a moq mock instead.
type RepoStore interface {
	GetRepoByID(ctx context.Context, id uuid.UUID) (reposstore.Repo, error)
}
