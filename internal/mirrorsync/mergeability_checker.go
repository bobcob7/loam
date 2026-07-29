package mirrorsync

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// StoreMergeabilityChecker is the production MergeabilityChecker
// (docs/sync-spec.md -> Mergeability Check; docs/git-spec.md -> Target
// Advances & Catch-Up; owned by bead giq.5). For every advanced target it
// is handed, it tests each open (non-terminal) work branch whose recorded
// target is that branch against the target's new tip, and flags the ones
// that no longer merge.
//
// It is a broker, not an author: the check runs through merge-tree against
// the bare mirror with no worktree and no ref writes, so a work branch's
// commits are never touched (docs/git-spec.md: "The server is a broker and
// a store: it never authors commits, and work-branch refs advance only by
// agent pushes"). Catching up is ordinary git, done by the agent.
//
// # Clean is a no-op, deliberately
//
// A branch that still merges gets NOTHING written -- not a cleared flag,
// not a state change. Being behind but mergeable is a normal state
// (docs/sync-spec.md: "Merges cleanly -> nothing happens"), and this
// checker holds no clearing seam at all (see workBranchConflictMarker).
// That is not an oversight and not a latching bug: a flagged branch
// recovers by PUSH, via catch-up detection on an accepted push
// (internal/catchup, loam-giq.6), because clearing is inseparable from
// re-opening a review round with requested_by = the server whenever the
// branch was a conflict-RESET one and therefore flips "directly back to
// reviewable". If a clean re-check cleared the flag here instead, a target
// advance the branch happens to merge with would silently restore a
// demoted branch to reviewable with no agent push, no fresh round, and
// stale verdicts suddenly counting again toward the approval bar. The
// spec's own words for the neighbouring case are the same shape: "If the
// target has advanced again since the reset, the flag simply stays until a
// push catches up."
//
// # A failed check is not a conflict
//
// Every error from mergeTreeRunner aborts the whole call, leaving every
// work branch -- including ones already flagged, and ones not yet reached
// this pass -- exactly as it was. The scheduler turns that into
// repos.sync_state = error and retries the entire cycle from step 1 on the
// next tick, which is the correct treatment for "we do not know": the
// alternative, treating an unresolvable ref or a broken mirror as a
// conflict, would demote reviewable branches and void verdicts over a
// check that never ran.
type StoreMergeabilityChecker struct {
	dataDir   string
	repos     repoByNameLookup
	branches  workBranchNameLister
	merger    mergeTreeRunner
	conflicts workBranchConflictMarker
}

// NewStoreMergeabilityChecker builds a StoreMergeabilityChecker rooted at
// dataDir (LOAM_DATA_DIR; the bare mirror path is derived through
// internal/mirrorpath exactly as MirrorFetcher's is), resolving repo.ID
// through repos (typically *reposstore.Store), enumerating work branches
// through branches (typically *workbranchstore.Store), running merge
// checks through merger (typically *gitmergetree.Checker), and writing
// conflict verdicts through conflicts (typically *workbranchstore.Store).
func NewStoreMergeabilityChecker(dataDir string, repos repoByNameLookup, branches workBranchNameLister, merger mergeTreeRunner, conflicts workBranchConflictMarker) *StoreMergeabilityChecker {
	return &StoreMergeabilityChecker{dataDir: dataDir, repos: repos, branches: branches, merger: merger, conflicts: conflicts}
}

// CheckMergeability satisfies MergeabilityChecker. See the type doc
// comment for why a clean result writes nothing and why a failed check is
// never reported as a conflict.
//
// advanced is loam-giq.4's deliberately wider union -- every listed target
// branch plus the recorded target of every open work branch -- and this
// method is the reason that extra width exists: a target de-listed from
// repo_target_branches while work was still in flight must keep getting
// conflict detection, so StoreAdvanceDetector reports it and this method
// consumes it without re-checking whether it is still listed. (Its sibling
// consumer, StoreIngestEnqueuer, narrows the same set the other way, to
// repo's indexed branch alone.)
//
// An advance whose NewSHA is empty is a deleted ref (Advance's own doc
// comment) and is dropped before anything else happens: there is no tip to
// merge against, and inventing one -- or passing the empty string to git
// -- would be worse than skipping. If that leaves nothing to check at all,
// the method returns without so much as a repo lookup, so the common tick
// where nothing advanced costs no database round trip.
func (c *StoreMergeabilityChecker) CheckMergeability(ctx context.Context, repo RepoID, advanced []Advance) error {
	tips := advancedTips(advanced)
	if len(tips) == 0 {
		return nil
	}
	row, err := c.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		return fmt.Errorf("resolving repo %s for mergeability check: %w", repo, err)
	}
	open, err := listOpenWorkBranches(ctx, c.branches, row.ID)
	if err != nil {
		return fmt.Errorf("listing open work branches for repo %s: %w", repo, err)
	}
	mirrorDir := mirrorpath.Dir(c.dataDir, string(repo))
	for _, wb := range open {
		tip, advancedTarget := tips[wb.Target]
		if !advancedTarget {
			continue
		}
		conflicted, err := c.merger.MergeTree(ctx, mirrorDir, workBranchRef(wb.Name), tip)
		if err != nil {
			return fmt.Errorf("merge-testing work branch %s of repo %s against %s tip %s: %w", wb.Name, repo, wb.Target, tip, err)
		}
		if !conflicted {
			continue
		}
		if _, err := c.conflicts.MarkConflicted(ctx, wb.ID); err != nil {
			return fmt.Errorf("flagging work branch %s of repo %s conflicted against %s tip %s: %w", wb.Name, repo, wb.Target, tip, err)
		}
	}
	return nil
}

// advancedTips indexes advanced by branch name, mapping each to its new
// tip SHA, and drops any entry whose NewSHA is empty (a deleted ref). A
// branch appearing twice keeps the last entry, which cannot happen from
// StoreAdvanceDetector (it iterates a deduplicated union) but is defined
// rather than left to chance.
func advancedTips(advanced []Advance) map[string]string {
	tips := make(map[string]string, len(advanced))
	for _, adv := range advanced {
		if adv.NewSHA == "" {
			continue
		}
		tips[adv.Branch] = adv.NewSHA
	}
	return tips
}

// workBranchRef builds the mirror ref path for a registered work branch's
// bare name (docs/git-spec.md -> Ref Policy), the same convention
// buildFetchRefspecs excludes from every mirror fetch. A full ref path is
// passed to git rather than the bare name so a same-named tag or remote-
// tracking ref in the mirror can never be resolved instead.
func workBranchRef(name string) string {
	return refnames.WorkBranch(name)
}

// listOpenWorkBranches pages through every work_branches row for repoID
// and returns the non-terminal ones -- the "open work branches" both
// docs/sync-spec.md's Mirror Sync step 2 (set (b)) and its Mergeability
// Check are defined over. Complete and closed rows are excluded: their
// recorded target no longer needs conflict detection, and MarkConflicted
// rejects them outright.
//
// Shared by StoreAdvanceDetector (which projects Target) and
// StoreMergeabilityChecker (which needs the whole row), so the paging loop
// and the terminal-state predicate have one definition rather than two
// that could drift into disagreeing about which branches are "open".
func listOpenWorkBranches(ctx context.Context, lister workBranchNameLister, repoID uuid.UUID) ([]workbranchstore.WorkBranch, error) {
	var open []workbranchstore.WorkBranch
	var offset int32
	for {
		page, total, err := lister.List(ctx, workbranchstore.ListFilter{RepoID: &repoID}, workBranchListPageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, wb := range page {
			if isTerminalWorkBranchState(wb.State) {
				continue
			}
			open = append(open, wb)
		}
		offset += int32(len(page))
		if len(page) == 0 || int64(offset) >= total {
			return open, nil
		}
	}
}

// isTerminalWorkBranchState reports whether s is one of work_branches'
// two terminal states (docs/persistence-spec.md "work_branches": "complete
// and closed are terminal"), the states listOpenWorkBranches excludes.
func isTerminalWorkBranchState(s workbranchstore.State) bool {
	return s == workbranchstore.StateComplete || s == workbranchstore.StateClosed
}
