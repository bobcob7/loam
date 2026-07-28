package catchup

import (
	"context"
	"log/slog"

	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// RoundRequestedBy is the review_rounds.requested_by value a catch-up
// auto-restore writes. It is a literal sentinel, deliberately distinct
// from every agent identifier and from "admin" (internal/handler/workbranch's
// own send-back value), so a round the SERVER opened because a push caught
// the branch up is distinguishable, in the row itself, from one an author
// or an admin asked for.
const RoundRequestedBy = "server"

// refsHeadsPrefix is the namespace a target branch occupies in the bare
// mirror (docs/git-spec.md -> "Ref Policy (push)": mirrored target refs
// live under refs/heads/ and are read-only to agents). The full ref path
// is handed to git rather than the bare branch name so a same-named tag in
// the mirror can never be resolved instead.
const refsHeadsPrefix = "refs/heads/"

// Detector is docs/git-spec.md -> "Target Advances & Catch-Up"'s catch-up
// half, wired as the policy socket's post-accept hook. Construct with New
// and pass its OnAcceptedPush method to hooksocket.Listen.
type Detector struct {
	dataDir   string
	ancestry  ancestryChecker
	conflicts conflictClearer
	rounds    roundOpener
	logger    *slog.Logger
}

// New builds a Detector rooted at dataDir (LOAM_DATA_DIR; the bare mirror
// path is derived through internal/mirrorpath exactly as
// internal/mirrorsync's own components derive theirs), checking ancestry
// through ancestry (typically *gitancestry.Checker), clearing flags
// through conflicts (typically *workbranchstore.Store), and opening rounds
// through rounds (typically *reviewstore.RoundStore).
func New(dataDir string, ancestry ancestryChecker, conflicts conflictClearer, rounds roundOpener, logger *slog.Logger) *Detector {
	return &Detector{dataDir: dataDir, ancestry: ancestry, conflicts: conflicts, rounds: rounds, logger: logger}
}

// OnAcceptedPush satisfies hooksocket.PostAcceptFunc: it runs once per
// accepted ref update, after the WHOLE push has passed ref policy
// (refpolicy.EvaluatePush's own guarantee -- an atomically rejected push
// never reaches here, so a ref that individually looked fine in a doomed
// push never triggers this bookkeeping).
//
// The decision, in order:
//
//  1. A branch with no conflict flag is left alone without so much as a
//     git invocation. Ordinary pushes to unflagged branches are the
//     overwhelmingly common case and must cost nothing. This check is
//     written as an allowlist of the two flagged values rather than "!=
//     none" so a zero-value WorkBranch (Conflict "") also short-circuits
//     here instead of reaching git with an empty target name.
//  2. A ref DELETION carries no history to inspect. It cannot reach here
//     in production (internal/mirrorreconcile installs
//     receive.denyDeletes on every mirror, and git rejects the delete
//     before the ref update lands) but the all-zero new-sha is git's own
//     wire encoding for it, and asking git whether the target tip is an
//     ancestor of "000...0" would be a check failure, not an answer.
//  3. Catch-up is CONTAINMENT of the current target tip, read live from
//     the mirror at this instant -- not against whatever tip was current
//     when the conflict was flagged. If the target advanced again in the
//     meantime, a push that only catches up to the older tip does not
//     clear the flag, which is docs/git-spec.md's own rule: "If the target
//     has advanced again since the reset, the flag simply stays until a
//     push catches up."
//  4. ClearConflict, whose one guarded UPDATE both clears the flag and
//     restores a DEMOTED branch to reviewable.
//  5. A new review round -- ONLY for the demoted case. See
//     roundBelongsToThisRestore.
func (d *Detector) OnAcceptedPush(ctx context.Context, accepted hooksocket.AcceptedPush) {
	wb := accepted.WorkBranch
	if wb.Conflict != workbranchstore.ConflictFlagged && wb.Conflict != workbranchstore.ConflictReset {
		return
	}
	if isZeroSHA(accepted.Update.NewSHA) {
		return
	}
	mirrorDir := mirrorpath.Dir(d.dataDir, accepted.Repo)
	targetRef := refsHeadsPrefix + wb.Target
	caughtUp, err := d.ancestry.Contains(ctx, mirrorDir, accepted.QuarantineDir, targetRef, accepted.Update.NewSHA)
	if err != nil {
		d.logger.ErrorContext(ctx, "catch-up check failed; leaving conflict flag as-is", "work_branch_id", wb.ID, "work_branch", wb.Name, "repo", accepted.Repo, "target", wb.Target, "new_sha", accepted.Update.NewSHA, "error", err)
		return
	}
	if !caughtUp {
		d.logger.DebugContext(ctx, "pushed history does not contain the current target tip; conflict flag stays", "work_branch_id", wb.ID, "work_branch", wb.Name, "target", wb.Target)
		return
	}
	restored, err := d.conflicts.ClearConflict(ctx, wb.ID)
	if err != nil {
		d.logger.ErrorContext(ctx, "clearing conflict after a catch-up push failed", "work_branch_id", wb.ID, "work_branch", wb.Name, "error", err)
		return
	}
	if !roundBelongsToThisRestore(wb.Conflict) {
		d.logger.InfoContext(ctx, "catch-up push cleared a conflict flag", "work_branch_id", wb.ID, "work_branch", wb.Name, "state", restored.State)
		return
	}
	round, err := d.rounds.OpenRound(ctx, wb.ID, RoundRequestedBy)
	if err != nil {
		d.logger.ErrorContext(ctx, "opening the catch-up restore's review round failed", "work_branch_id", wb.ID, "work_branch", wb.Name, "error", err)
		return
	}
	d.logger.InfoContext(ctx, "catch-up push restored a conflict-reset work branch to review", "work_branch_id", wb.ID, "work_branch", wb.Name, "state", restored.State, "round", round.Number, "requested_by", RoundRequestedBy)
}

// roundBelongsToThisRestore is the whole conditional rule (loam-lb6,
// recorded in docs/git-spec.md -> "Target Advances & Catch-Up"): a
// catch-up opens a new numbered review round ONLY when the clear is
// actually a transition INTO reviewable.
//
//   - conflict was 'reset' -- the branch had been reviewable or reviewed
//     and was DEMOTED to draft by the advance. ClearConflict flips it back
//     to reviewable, and that is a transition, so it opens a round like
//     every other transition into reviewable.
//   - conflict was 'flagged' -- the branch stayed draft throughout.
//     ClearConflict removes the flag and does not touch state at all.
//     Opening a round here would invent a review round for a branch nobody
//     has asked anyone to review.
//
// The pre-push conflict value is what decides this, not the post-clear
// row: ClearConflict has already reset conflict to 'none' by then, so the
// returned row cannot tell the two cases apart. The state it returns
// cannot substitute either -- a branch that gained the flag while draft
// and was then moved to reviewable by an ordinary request-review reads
// back as reviewable here while never having been demoted, and it already
// has the round that request-review opened.
//
// prior comes from the WorkBranch row refpolicy.EvaluatePush fetched to
// make this very push's policy decision, so it is this push's own view of
// the branch and needs no second query.
func roundBelongsToThisRestore(prior workbranchstore.Conflict) bool {
	return prior == workbranchstore.ConflictReset
}

// isZeroSHA reports whether sha is git's all-zero "no object" sentinel --
// every character '0', with at least one character present -- regardless
// of the hash algorithm's digest length (40 hex characters for SHA-1, 64
// for SHA-256). An empty string reports true here (unlike
// internal/refpolicy's identically-named helper, which must not
// misclassify it as a ref CREATE): there is no history behind an empty
// new-sha either, and this function's callers use it only to decide
// whether there is anything to ask git about.
func isZeroSHA(sha string) bool {
	for _, r := range sha {
		if r != '0' {
			return false
		}
	}
	return true
}
