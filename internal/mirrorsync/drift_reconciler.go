package mirrorsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/gitref"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// driftRoundRequestedBy is the review_rounds.requested_by value an adopted
// fast-forward's round carries. It is the literal internal/catchup's
// RoundRequestedBy already established for a round the SERVER opened
// because something outside the review flow moved the branch
// (docs/sync-spec.md -> Upstream Drift: "`requested_by` is `server`, the
// same attribution a catch-up round uses").
//
// It is a second copy of that constant rather than an import, for the
// reason this package's other cross-package duplications document: taking a
// dependency on internal/catchup -- a package built around the pre-receive
// hook's post-accept seam, which nothing here touches -- to share one
// three-character string would couple two unrelated halves of the system
// for no benefit. The shared meaning lives in the spec, and both call sites
// name it.
const driftRoundRequestedBy = "server"

// StoreDriftReconciler is the production DriftReconciler (docs/sync-spec.md
// -> Upstream Drift on `loam/<work-branch>`; owned by bead loam-giq.11).
//
// Loam owns the loam/<name> branches it pushes upstream, but it does not
// control them: anyone with write access to the forge can push straight to
// one, and that has happened in production (loam-giq.11's report). The
// branch comes back on the ordinary mirror fetch -- branchesRefspec is
// "+refs/heads/*:refs/heads/*" and only the reserved namespace is excluded
// -- so the divergence is already sitting in the mirror by the time this
// step runs. This is a missing COMPARISON, not missing data, and that is
// all this type adds.
//
// # The comparison, and the three outcomes
//
// Once per cycle, for every work branch of the repo with a recorded PR and
// a recorded accepted_tip that has not reached a terminal state, it reads
// three values -- the mirrored upstream tip U (refs/heads/loam/<name>), the
// recorded accepted_tip A, and the work branch's own tip W
// (refs/heads/loam-reserved/<name>) -- and acts on the first that applies:
//
//   - U == A. Upstream is exactly where Loam left it. Nothing happened;
//     this is the overwhelmingly common case and it costs one rev-parse.
//     Any recorded drift is cleared here (see "Drift is level-triggered").
//   - W is an ancestor of U. Someone fast-forwarded the branch. Loam ADOPTS
//     it: the work-branch ref advances to U, a fresh review round opens,
//     and accepted_tip absorbs U.
//   - anything else. The two histories cannot be reconciled without a
//     decision Loam is not entitled to make, so it changes nothing and
//     records upstream_drift = diverged for the admin console.
//
// The third case is deliberately broader than docs/sync-spec.md's own
// "neither tip is an ancestor of the other" sentence, and the gap is real
// rather than pedantic: U can also be a strict ancestor of W, which is what
// a force-push that REWOUND the upstream branch leaves behind. That is not
// a fast-forward (adopting it would move the work branch backwards, losing
// reviewed commits), and it is not nothing (accepted_tip would permanently
// misdescribe upstream, and ListProposals -- which compares accepted_tip
// against W, never against upstream -- would never re-list the branch, so
// nothing would ever put the dropped commits back). It is "someone rewrote
// the branch Loam pushed", which is exactly what diverged means. The spec
// section was updated to say so.
//
// # Adopting is not blessing
//
// An adopted commit reached the mirror through the FORGE, not through
// /git/*, so it never passed the pre-receive hook and none of the push
// policy applied to it -- not the author check, not the reserved-namespace
// guard, not force-push rejection (docs/git-spec.md -> Enforcement
// Mechanics). This is the one path by which Loam takes in code it did not
// gate, and it is defensible ONLY because the approvals reset: the commit
// is now in the work branch, and it cannot reach a FURTHER upstream push
// until someone approves it again. Anything that weakens the reset breaks
// that argument.
//
// The reset is expressed the way every other reset in this system is -- by
// opening a new numbered review round, which makes every prior verdict
// stale by derivation (staleness is never a stored column) -- rather than
// by inventing a second notion of staleness. See reopenReview for which
// branches get one and why a draft branch does not.
//
// # Drift is level-triggered, unlike conflict
//
// work_branches.conflict is edge-triggered on both sides: a target advance
// flags it and only a catch-up PUSH clears it (internal/catchup). Drift has
// no such push. The operator fixes it on the forge -- by force-pushing
// loam/<name> back, or by merging the work branch's commits into it -- and
// neither action reaches Loam at all. So this step re-derives the value
// from the mirror on every cycle and writes whichever it observes,
// including 'none'; a branch that was diverged and has since been
// reconciled recovers on the next tick with no command to run. That is also
// why workbranchstore.SetUpstreamDrift is a plain setter rather than the
// guarded pair MarkConflicted/ClearConflict form.
//
// # Every step is safe to repeat, and the round goes FIRST
//
// Nothing here is transactional across git and Postgres, so the order of an
// adoption's three writes is chosen for what a crash between any two of
// them leaves behind. It is: review round, then the ref advance, then
// accepted_tip LAST as the commit of the whole reconciliation.
//
// The round is first because it is the only one of the three whose absence
// is unrecoverable. Consider the ref moving first and a crash before the
// round: the branch now holds the adopted, unreviewed commit, the
// pre-adoption approvals are still live, and upstream_drift is still none
// -- so an AcceptProposal arriving before the next tick PASSES, pushes, and
// writes accepted_tip = U itself. The next tick then sees U == A and never
// opens the round at all. The unreviewed commit is not merely un-reset for
// one tick; it is permanently blessed, and the review gate this whole
// mechanism exists to keep honest has been silently bypassed. Writing the
// round first makes that window harmless: the approvals are already stale,
// so the accept is refused, and everything after the round is retried by
// the next tick (until accepted_tip is written, U != A still holds, and
// W == U keeps the ancestry check answering "fast-forward", since a commit
// is its own ancestor -- verified against real git 2.50.1).
//
// What round-first costs, stated plainly rather than buried: the reset can
// fire for an adoption that then does not happen. If the compare-and-swap
// below loses to an agent push landing in the same window, or the ref
// advance simply fails, the branch has been sent back for re-review for
// nothing -- an ordinary agent push does NOT reset approvals, so this is a
// reset the branch would not otherwise have had. A repeatedly failing
// adoption also opens one round per tick; every one after the first is
// semantically a no-op (the verdicts it would stale are already stale) and
// the repo sits in sync_state = error throughout, which is the visible
// signal. Both costs are re-review. The cost they replace is an unreviewed
// commit reaching upstream with approvals that were never re-earned, and
// those are not comparable: a duplicated round is the cheap error, a
// skipped one is not.
//
// The ref advance is a compare-and-swap against W
// (gitref.AdvanceWorkBranchRef), so an agent push that lands between this
// step's read and its write is never overwritten: the swap is refused, this
// branch is left alone, and the next tick re-derives everything from the
// new tip.
//
// Failure isolation is per branch, exactly as StorePRPoller's is: one
// branch's git or store failure is collected and the rest are still
// reconciled, and the joined error puts the repo in sync_state = error so
// the failure is visible, with the next tick as the retry.
type StoreDriftReconciler struct {
	dataDir  string
	logger   *slog.Logger
	repos    repoByNameLookup
	branches workBranchNameLister
	tips     mirrorTipResolver
	refs     workBranchRefAdvancer
	ancestry ancestryChecker
	drift    workBranchDriftMarker
	adoption workBranchAdoptionWriter
	rounds   roundOpener
}

// NewStoreDriftReconciler builds a StoreDriftReconciler rooted at dataDir
// (LOAM_DATA_DIR; the same root every other mirror-addressing collaborator
// in this package derives bare-mirror paths from through
// internal/mirrorpath).
//
// repos resolves the repo row (typically *reposstore.Store) and branches
// enumerates its work branches (typically *workbranchstore.Store). tips
// reads the two mirror refs and refs performs the guarded advance (both
// typically *gitref.Creator); ancestry answers the fast-forward question
// (typically *gitancestry.Checker). drift and adoption write the
// work_branches row and rounds opens the review round (typically
// *workbranchstore.Store and *reviewstore.RoundStore).
func NewStoreDriftReconciler(dataDir string, logger *slog.Logger, repos repoByNameLookup, branches workBranchNameLister, tips mirrorTipResolver, refs workBranchRefAdvancer, ancestry ancestryChecker, drift workBranchDriftMarker, adoption workBranchAdoptionWriter, rounds roundOpener) *StoreDriftReconciler {
	return &StoreDriftReconciler{
		dataDir:  dataDir,
		logger:   logger,
		repos:    repos,
		branches: branches,
		tips:     tips,
		refs:     refs,
		ancestry: ancestry,
		drift:    drift,
		adoption: adoption,
		rounds:   rounds,
	}
}

// ReconcileDrift satisfies DriftReconciler. See the type doc comment for
// the comparison and what each outcome does. A failure to resolve the repo
// or to list its work branches aborts the whole step (there is nothing to
// compare without them); past that point every failure is per branch and
// collected.
func (d *StoreDriftReconciler) ReconcileDrift(ctx context.Context, repo RepoID) error {
	row, err := d.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		return fmt.Errorf("resolving repo %s for upstream drift reconciliation: %w", repo, err)
	}
	branches, err := d.driftSet(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("listing work branches with a recorded PR for repo %s: %w", repo, err)
	}
	var errs []error
	for _, wb := range branches {
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, fmt.Errorf("stopped reconciling repo %s before work branch %s: %w", repo, wb.Name, ctxErr))
			break
		}
		if err := d.reconcileOne(ctx, repo, wb); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileOne compares one work branch's upstream branch against what Loam
// last pushed or adopted, and applies whichever of the three outcomes fits.
//
// An upstream branch that is simply ABSENT from the mirror is not one of
// them: it is skipped, leaving both accepted_tip and upstream_drift exactly
// as they are. That state is reachable without anything being wrong -- a
// forge configured to delete a merged PR's head branch removes it the
// moment the PR merges, seconds before the poller flips the branch to
// complete -- and there is no third SHA to classify against, so treating it
// as drift would flag branches for a state that resolves itself, while
// treating it as "no drift" would clear a real diverged flag on the
// strength of a missing ref.
func (d *StoreDriftReconciler) reconcileOne(ctx context.Context, repo RepoID, wb workbranchstore.WorkBranch) error {
	upstream, err := d.tips.ResolveUpstreamProposalRef(ctx, string(repo), wb.Name)
	if errors.Is(err, gitref.ErrRefMissing) {
		d.logger.DebugContext(ctx, "no mirrored upstream branch to compare against", "repo", string(repo), "work_branch", wb.Name, "ref", refnames.UpstreamProposalBranch(wb.Name))
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolving the mirrored upstream branch for work branch %s in repo %s: %w", wb.Name, repo, err)
	}
	if upstream == *wb.AcceptedTip {
		return d.recordDrift(ctx, repo, wb, workbranchstore.DriftNone)
	}
	tip, err := d.tips.ResolveWorkBranchRef(ctx, string(repo), wb.Name)
	if err != nil {
		return fmt.Errorf("resolving the tip of work branch %s in repo %s for drift reconciliation: %w", wb.Name, repo, err)
	}
	fastForward, err := d.ancestry.Contains(ctx, mirrorpath.Dir(d.dataDir, string(repo)), "", tip, upstream)
	if err != nil {
		return fmt.Errorf("checking whether work branch %s in repo %s is behind its upstream branch: %w", wb.Name, repo, err)
	}
	if !fastForward {
		d.logger.WarnContext(ctx, "upstream branch diverged from the work branch Loam pushed", "repo", string(repo), "work_branch", wb.Name, "upstream_tip", upstream, "accepted_tip", *wb.AcceptedTip, "work_branch_tip", tip)
		return d.recordDrift(ctx, repo, wb, workbranchstore.DriftDiverged)
	}
	return d.adopt(ctx, repo, wb, tip, upstream)
}

// adopt takes in a commit that arrived on the upstream branch behind Loam's
// back, in the one shape that loses nothing: the work branch gains history
// it did not have and every prior verdict goes stale.
//
// The three writes are ordered round, ref, accepted_tip -- see the type doc
// comment's "Every step is safe to repeat, and the round goes FIRST" for why
// that order and not another, and for what it costs. The ref advance is
// skipped when the work branch is already AT the upstream commit, which is
// not a rare case: it is exactly what a previous tick left behind if it
// crashed after the advance (and git-merge-base --is-ancestor answers yes
// for a commit against itself, so such a branch is still classified as a
// fast-forward and lands here again).
//
// A compare-and-swap that loses to a concurrent agent push
// (gitref.ErrRefMoved) is not an error for this branch: this pass's whole
// view of it -- the tip it read, the ancestry it computed -- is stale, so
// it stops, writes nothing further, and lets the next tick re-derive all of
// it. The round opened moments earlier stands; that is the cost named
// above, paid so the crash window cannot bless an unreviewed commit.
func (d *StoreDriftReconciler) adopt(ctx context.Context, repo RepoID, wb workbranchstore.WorkBranch, tip, upstream string) error {
	if err := d.reopenReview(ctx, repo, wb); err != nil {
		return err
	}
	if tip != upstream {
		if err := d.refs.AdvanceWorkBranchRef(ctx, string(repo), wb.Name, tip, upstream); err != nil {
			if errors.Is(err, gitref.ErrRefMoved) {
				d.logger.InfoContext(ctx, "work branch moved while adopting an upstream fast-forward; leaving it for the next cycle", "repo", string(repo), "work_branch", wb.Name, "expected_tip", tip)
				return nil
			}
			return fmt.Errorf("adopting upstream commit %s into work branch %s in repo %s: %w", upstream, wb.Name, repo, err)
		}
		d.logger.InfoContext(ctx, "adopted an upstream fast-forward into a work branch", "repo", string(repo), "work_branch", wb.Name, "from", tip, "to", upstream)
	}
	if _, err := d.adoption.RecordAcceptedTip(ctx, wb.ID, upstream); err != nil {
		return fmt.Errorf("recording adopted tip %s for work branch %s in repo %s: %w", upstream, wb.Name, repo, err)
	}
	return d.recordDrift(ctx, repo, wb, workbranchstore.DriftNone)
}

// reopenReview is the approvals reset, and it reuses the mechanism that
// already exists rather than adding a second one: a new numbered review
// round makes every earlier verdict stale by derivation (verdicts carry no
// stale column -- docs/web-spec.md: "`stale` is derived"), and only
// non-stale approves count toward the accept bar.
//
// Which branches get one follows the PRINCIPLE internal/catchup states --
// a round belongs to a branch that is actually in, or being returned to,
// review -- not its roundBelongsToThisRestore predicate, which decides a
// different question (whether a conflict CLEAR was a transition into
// reviewable) off a value this path does not have:
//
//   - reviewed: the branch is sitting in the admin's queue with live
//     approvals. It opens a round and goes back to REVIEWABLE -- the
//     ordinary re-review transition an author or an admin send-back uses.
//     Without the state move the branch would keep a state that says
//     "decided" while carrying no live verdict, and reviewers would never
//     see it again: the awaiting-verdict filter every reviewer's queue is
//     built on matches state = 'reviewable' only
//     (internal/db/queries/work_branches.sql).
//   - reviewable: already in review; the round bump alone is the reset. Its
//     state is not touched (draft -> reviewable is the only other move
//     UpdateState allows into it, and it is not where this branch is).
//   - draft: nothing at all. A draft branch has no live review to
//     invalidate and no path to acceptance without passing through
//     request-review -- which opens its own round -- so a round opened here
//     would be, in catchup's words, "a review round for a branch nobody has
//     asked anyone to review".
//
// The round is opened BEFORE the state move, and the state move's failure
// is logged rather than returned. That asymmetry is the whole point: the
// ROUND is the reset (CurrentRoundApproveCount, which both the accept gate
// and the proposal queue read, counts only current-round verdicts), while
// the state move is REVIEWER VISIBILITY. Doing them the other way round
// makes a failing state move suppress the reset entirely, and
// UpdateWorkBranchState's reviewed -> reviewable arm can genuinely fail: it
// requires a non-empty title AND description, so a reviewed branch carrying
// a blank description would fail that move on every tick, never reach
// OpenRound, and never write accepted_tip -- an adopted, unreviewed commit
// with live approvals, forever. No shipped client can produce that row
// today (the CLI rejects empty values and the console has no
// UpdateWorkBranch caller), which makes it latent rather than live, and
// exactly the kind of thing to close while the ordering is being fixed
// anyway.
//
// Returning the error instead would also make the failure repeat unbounded:
// accepted_tip stays unwritten, so every tick re-enters this path and opens
// another round. Logging it leaves the branch reset, gated, and reconciled,
// with one loud line naming the row an operator has to fix by hand.
func (d *StoreDriftReconciler) reopenReview(ctx context.Context, repo RepoID, wb workbranchstore.WorkBranch) error {
	if wb.State == workbranchstore.StateDraft {
		d.logger.InfoContext(ctx, "adopted commit landed on a draft work branch; no review round to reset", "repo", string(repo), "work_branch", wb.Name)
		return nil
	}
	round, err := d.rounds.OpenRound(ctx, wb.ID, driftRoundRequestedBy)
	if err != nil {
		return fmt.Errorf("opening a review round for work branch %s in repo %s after adopting an upstream commit: %w", wb.Name, repo, err)
	}
	d.logger.InfoContext(ctx, "reset approvals after adopting an upstream commit", "repo", string(repo), "work_branch", wb.Name, "round", round.Number, "requested_by", driftRoundRequestedBy)
	if wb.State != workbranchstore.StateReviewed {
		return nil
	}
	if _, err := d.adoption.UpdateState(ctx, wb.ID, workbranchstore.StateReviewable); err != nil {
		d.logger.ErrorContext(ctx, "approvals were reset but the work branch could not be returned to reviewable; it is gated but invisible to reviewers until an admin moves it", "repo", string(repo), "work_branch", wb.Name, "error", err)
	}
	return nil
}

// recordDrift writes want, unless the row already carries it. The read-back
// check is not an optimization detail worth hiding: the overwhelmingly
// common outcome of this whole step is "upstream is where we left it, drift
// is already none", and without it every enrolled repo would issue one
// UPDATE per accepted work branch per tick, bumping updated_at on rows
// nothing happened to.
//
// It is safe to decide from the row this cycle read because this step is
// the only writer of the column, and the scheduler never runs two cycles
// for the same repo at once (tryStart, scheduler.go).
func (d *StoreDriftReconciler) recordDrift(ctx context.Context, repo RepoID, wb workbranchstore.WorkBranch, want workbranchstore.UpstreamDrift) error {
	if wb.UpstreamDrift == want {
		return nil
	}
	if _, err := d.drift.SetUpstreamDrift(ctx, wb.ID, want); err != nil {
		return fmt.Errorf("recording upstream drift %s for work branch %s in repo %s: %w", want, wb.Name, repo, err)
	}
	return nil
}

// driftSet pages through every work_branches row for repoID by keyset
// cursor -- the same full-enumeration shape, and the same reason for it, as
// StorePRPoller's pollSet -- and returns the ones worth comparing: a
// recorded upstream PR, a recorded accepted_tip, and a non-terminal state,
// sorted by name for a stable order.
//
// accepted_tip is part of the filter, not just the PR number, because it is
// the only thing the comparison can be made against. A row with a PR but no
// tip is every branch accepted before that column existed (loam-cgg); there
// is nothing to say about such a row except that Loam cannot prove anything
// about it, which is exactly how ListProposals already reads the same NULL.
// The next accept records a tip and the row joins this set for good.
func (d *StoreDriftReconciler) driftSet(ctx context.Context, repoID uuid.UUID) ([]workbranchstore.WorkBranch, error) {
	var comparable []workbranchstore.WorkBranch
	var after *workbranchstore.Cursor
	for {
		page, err := d.branches.ListByCursor(ctx, workbranchstore.ListFilter{RepoID: &repoID}, workBranchListPageSize, after)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, wb := range page {
			if wb.UpstreamPRNumber == nil || wb.AcceptedTip == nil {
				continue
			}
			if isTerminalWorkBranchState(wb.State) {
				continue
			}
			comparable = append(comparable, wb)
		}
		last := page[len(page)-1]
		after = &workbranchstore.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	sort.Slice(comparable, func(i, j int) bool { return comparable[i].Name < comparable[j].Name })
	return comparable, nil
}
