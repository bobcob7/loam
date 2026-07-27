package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// policyStoreAdapter composes reposstore.Store (repo name -> repo id) and
// workbranchstore.Store (repo id + branch name -> WorkBranch) into the
// single hooksocket.WorkBranchStore seam the policy socket consumes --
// exactly the read path internal/refpolicy.EvaluatePush needs for
// docs/git-spec.md "Ref Policy (push)" rules 1-3: resolve LOAM_REPO's
// trusted repo name, then look up the pushed branch name within it.
// hooksocket.Listen's own store parameter is typed as hooksocket's own
// WorkBranchStore interface (this repo's "interfaces at the consumer"
// convention: hooksocket owns its copy of this method set rather than
// importing refpolicy's), and this adapter's method set satisfies both
// structurally with no cast.
type policyStoreAdapter struct {
	repos        *reposstore.Store
	workBranches *workbranchstore.Store
}

// GetWorkBranch implements hooksocket.WorkBranchStore. A repo lookup
// failure is, in production, unreachable in practice: internal/handler/git
// already resolved and required this exact repo to be enrolled before
// receive-pack -- and therefore this hook -- ever ran (docs/git-spec.md
// "Repo not enrolled -> 404"). It is still mapped onto
// workbranchstore.ErrNotFound here, defensively, so
// refpolicy.EvaluatePush's rule-1 classification (unknown ref vs
// read-only ref, both keyed on errors.Is(err, workbranchstore.ErrNotFound))
// applies uniformly rather than this seam surfacing a second,
// undistinguished error shape its one caller does not expect.
func (a policyStoreAdapter) GetWorkBranch(ctx context.Context, repoName, branchName string) (workbranchstore.WorkBranch, error) {
	repo, err := a.repos.GetRepoByName(ctx, repoName)
	if err != nil {
		if errors.Is(err, reposstore.ErrNotFound) {
			return workbranchstore.WorkBranch{}, fmt.Errorf("resolving repo %s: %w", repoName, workbranchstore.ErrNotFound)
		}
		return workbranchstore.WorkBranch{}, fmt.Errorf("resolving repo %s: %w", repoName, err)
	}
	return a.workBranches.GetByName(ctx, repo.ID, branchName)
}
