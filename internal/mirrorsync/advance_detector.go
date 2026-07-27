package mirrorsync

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// errMissingTargetBranch is StoreAdvanceDetector's whole-repo failure
// signal: a listed target branch (repo_target_branches, set (a)) has no
// ref in the post-fetch mirror any more (docs/sync-spec.md -> Mirror
// Sync: "If a target branch disappears upstream, the repo goes to
// sync_state = error naming the branch"). The scheduler's runSteps aborts
// the remaining steps for this tick on any DetectAdvances error, which is
// exactly "skip mergeability/ingest/PR-poll for this repo, and leave work
// branches exactly as they are" -- StoreAdvanceDetector need not touch
// work branches itself for that guarantee to hold.
var errMissingTargetBranch = errors.New("mirrorsync: listed target branch missing upstream")

// StoreAdvanceDetector is the production AdvanceDetector (docs/sync-spec.md
// -> Mirror Sync step 2; owned by bead giq.4). It compares SHAs before and
// after the fetch for the union of two branch sets:
//
//   - (a) repo_target_branches.branch -- every listed target for the repo.
//   - (b) DISTINCT work_branches.target for every non-terminal
//     (draft/reviewable/reviewed) work branch of the repo.
//
// Set (b) exists independently of set (a) so conflict detection (giq.5)
// keeps running for a target de-listed from repo_target_branches while
// work was still in flight -- an open work branch's recorded target is
// what the mergeability check needs, not whatever repo_target_branches
// currently says (DESIGN: "conflict detection must survive de-listing").
//
// The comparison itself is not StoreAdvanceDetector's own work: fetched
// (produced by Fetcher/parsePorcelainFetch, git-fetch(1) --porcelain) is
// already the delta -- a ref present in fetched.Refs is, by construction,
// a ref that changed on this fetch, whether by fast-forward or by force
// (MirrorFetcher's refspecs are upstream-wins forced -- loam-giq.2 -- so a
// force-push rewriting history is exactly as much an advance as a
// fast-forward; StoreAdvanceDetector never walks ancestry to tell them
// apart). A union branch absent from fetched.Refs did not change this
// tick and is silently skipped -- not re-reported, not re-enqueued -- so a
// scheduler ticking repeatedly over an unchanged ref never produces a
// second Advance for it.
type StoreAdvanceDetector struct {
	repos    repoByNameLookup
	targets  targetBranchLister
	branches workBranchNameLister
}

// NewStoreAdvanceDetector builds a StoreAdvanceDetector resolving repo.ID
// through repos (typically *reposstore.Store), set (a) through targets
// (typically *reposstore.Store), and set (b) through branches (typically
// *workbranchstore.Store).
func NewStoreAdvanceDetector(repos repoByNameLookup, targets targetBranchLister, branches workBranchNameLister) *StoreAdvanceDetector {
	return &StoreAdvanceDetector{repos: repos, targets: targets, branches: branches}
}

// DetectAdvances satisfies AdvanceDetector. See the type doc comment for
// the two branch sets it unions and how an advance is decided; a missing
// listed target branch (fetched shows its ref pruned to an all-zero new
// SHA) aborts with errMissingTargetBranch rather than returning any
// Advance, so the caller's cycle stops before mergeability/ingest/PR-poll
// -- and before touching any work branch -- for this repo this tick.
func (d *StoreAdvanceDetector) DetectAdvances(ctx context.Context, repo RepoID, fetched FetchResult) ([]Advance, error) {
	row, err := d.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		return nil, fmt.Errorf("resolving repo %s for advance detection: %w", repo, err)
	}
	listed, err := d.targets.ListTargetBranches(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("listing target branches for repo %s: %w", repo, err)
	}
	listedSet := make(map[string]struct{}, len(listed))
	for _, tb := range listed {
		listedSet[tb.Branch] = struct{}{}
	}
	wbTargets, err := d.openWorkBranchTargets(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("listing open work-branch targets for repo %s: %w", repo, err)
	}
	union := make(map[string]struct{}, len(listedSet)+len(wbTargets))
	for branch := range listedSet {
		union[branch] = struct{}{}
	}
	for branch := range wbTargets {
		union[branch] = struct{}{}
	}
	branchNames := make([]string, 0, len(union))
	for branch := range union {
		branchNames = append(branchNames, branch)
	}
	sort.Strings(branchNames)
	refsByName := make(map[string]RefUpdate, len(fetched.Refs))
	for _, ref := range fetched.Refs {
		refsByName[ref.Ref] = ref
	}
	var advanced []Advance
	for _, branch := range branchNames {
		ref, changed := refsByName["refs/heads/"+branch]
		if !changed {
			continue
		}
		if ref.NewSHA == "" {
			if _, isListed := listedSet[branch]; isListed {
				return nil, fmt.Errorf("target branch %s for repo %s: %w", branch, repo, errMissingTargetBranch)
			}
		}
		advanced = append(advanced, Advance{Branch: branch, OldSHA: ref.OldSHA, NewSHA: ref.NewSHA})
	}
	return advanced, nil
}

// openWorkBranchTargets returns the distinct Target of every non-terminal
// work branch of repoID -- set (b), docs/sync-spec.md -> Mirror Sync step
// 2. The paging and the terminal-state exclusion are
// listOpenWorkBranches' (mergeability_checker.go), shared with
// StoreMergeabilityChecker so the two consumers of "the repo's open work
// branches" can never drift apart on what open means; this method is only
// the Target projection over it.
func (d *StoreAdvanceDetector) openWorkBranchTargets(ctx context.Context, repoID uuid.UUID) (map[string]struct{}, error) {
	open, err := listOpenWorkBranches(ctx, d.branches, repoID)
	if err != nil {
		return nil, err
	}
	targets := make(map[string]struct{}, len(open))
	for _, wb := range open {
		targets[wb.Target] = struct{}{}
	}
	return targets, nil
}
