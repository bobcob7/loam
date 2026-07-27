package mirrorsync

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

// workBranchListPageSize bounds each page StoreRepoResolver reads while
// enumerating a repo's work-branch names. workbranchstore.Store.List has
// no unpaginated bulk query (unlike reposstore.Store.ListAllRepoNames, the
// bespoke query loam-13z added for RepoLister's exact same shape of
// problem), so ResolveRepo pages through it itself.
const workBranchListPageSize = 200

// StoreRepoResolver is the production repoResolver, joining
// reposstore.Store (upstream URL, forge host) and workbranchstore.Store
// (registered work-branch names) so MirrorFetcher never depends on either
// store directly.
type StoreRepoResolver struct {
	repos    repoByNameLookup
	branches workBranchNameLister
}

// NewStoreRepoResolver builds a StoreRepoResolver over repos (typically
// *reposstore.Store) and branches (typically *workbranchstore.Store).
func NewStoreRepoResolver(repos repoByNameLookup, branches workBranchNameLister) *StoreRepoResolver {
	return &StoreRepoResolver{repos: repos, branches: branches}
}

// ResolveRepo satisfies repoResolver: it resolves repo's row via
// GetRepoByName for ForgeHost/UpstreamURL, then pages through every
// work_branches row for that repo's id to collect their bare names --
// unfiltered by state, since a work branch in any state (including
// complete or closed) still has a live refs/heads/<name> ref in the
// mirror until something explicitly removes it (docs/git-spec.md -> Ref
// Policy: "registered work branch", not "open work branch").
func (r *StoreRepoResolver) ResolveRepo(ctx context.Context, repo RepoID) (host, upstreamURL string, workBranchNames []string, err error) {
	row, err := r.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		return "", "", nil, fmt.Errorf("resolving repo %s: %w", repo, err)
	}
	names, err := r.listWorkBranchNames(ctx, row.ID)
	if err != nil {
		return "", "", nil, fmt.Errorf("listing work branches for repo %s: %w", repo, err)
	}
	return row.ForgeHost, row.UpstreamURL, names, nil
}

// listWorkBranchNames pages through every work_branches row for repoID and
// returns their bare names, in whatever order List returns them.
func (r *StoreRepoResolver) listWorkBranchNames(ctx context.Context, repoID uuid.UUID) ([]string, error) {
	var names []string
	var offset int32
	for {
		page, total, err := r.branches.List(ctx, workbranchstore.ListFilter{RepoID: &repoID}, workBranchListPageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, wb := range page {
			names = append(names, wb.Name)
		}
		offset += int32(len(page))
		if len(page) == 0 || int64(offset) >= total {
			break
		}
	}
	return names, nil
}
