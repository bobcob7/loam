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

// listWorkBranchNames pages through every work_branches row for repoID via
// keyset (cursor) pagination and returns their bare names, in whatever
// order ListByCursor returns them (newest-created first).
//
// This used to page by LIMIT/OFFSET, counting rows from the top on every
// call -- unsafe here specifically because a concurrent insert landing in
// an already-passed page shifts every later row's offset by one, silently
// skipping exactly one row. A skipped row here is a work-branch name
// silently missing from ResolveRepo's exclusion list, which the next
// mirror fetch then does NOT exclude from its refspec -- an unrecoverable
// deletion of that work branch's ref (docs/git-spec.md's Ref Policy), not
// a mere pagination artifact (loam-coj). Keyset pagination resumes from
// the last row actually seen rather than a row count, so it cannot be
// shifted by inserts or deletes elsewhere in the result set. The loop
// terminates on an empty page, never on a separately-fetched total, which
// is exactly the kind of value that can go stale under concurrent writes.
func (r *StoreRepoResolver) listWorkBranchNames(ctx context.Context, repoID uuid.UUID) ([]string, error) {
	var names []string
	var after *workbranchstore.Cursor
	for {
		page, err := r.branches.ListByCursor(ctx, workbranchstore.ListFilter{RepoID: &repoID}, workBranchListPageSize, after)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return names, nil
		}
		for _, wb := range page {
			names = append(names, wb.Name)
		}
		last := page[len(page)-1]
		after = &workbranchstore.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}
