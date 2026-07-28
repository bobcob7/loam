// Package catchup implements docs/git-spec.md -> "Target Advances &
// Catch-Up"'s recovery half: a conflict-flagged work branch recovers BY
// PUSH, and this package is what notices. It hooks the policy socket's
// post-accept seam (internal/hooksocket.PostAcceptFunc), asks whether the
// pushed history now contains the branch's current target tip, and, when
// it does, clears the conflict -- opening a fresh review round for the one
// case that is a transition into reviewable.
//
// # Why this is not in the mergeability checker
//
// internal/mirrorsync.StoreMergeabilityChecker deliberately holds no
// clearing seam at all: a clean re-check there would restore a demoted
// branch with no agent push and no fresh round. Flagging is level-
// triggered on target advances; clearing is edge-triggered on a push. The
// two halves of the lifecycle live in different packages on purpose, and
// that checker's own doc comment names this package as the other half.
//
// # A failure here never fails the push
//
// The post-accept seam has no error return, by construction: by the time
// it runs, policy has already accepted the push, and there is no honest
// way to un-accept it from here. Every failure this package can observe --
// an unresolvable ref, a database error, a lost round insert -- is logged
// and abandoned, leaving the work branch exactly as it was. That is
// recoverable state, not corrupt state: the flag simply stays, and the
// next catch-up push re-runs the whole check from scratch.
package catchup

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . ancestryChecker conflictClearer roundOpener

// ancestryChecker is the git seam Detector consumes to answer docs/git-
// spec.md's containment question ("its history contains the current target
// tip"). Production binds it to *gitancestry.Checker; tests drive a moq
// mock, so this package's own decision table needs no git subprocess at
// all.
//
// extraObjectDir is receive-pack's quarantine directory, forwarded from
// the pre-receive hook: the pushed commit does not exist in the mirror's
// own object store yet at the moment this runs (see
// internal/gitancestry's package doc comment). A non-nil error must mean
// "the check did not run", never "not contained".
type ancestryChecker interface {
	Contains(ctx context.Context, mirrorDir, extraObjectDir, ancestor, descendant string) (bool, error)
}

// conflictClearer is the work_branches write seam for a confirmed
// catch-up. Production binds it to *workbranchstore.Store, whose
// ClearConflict is the single statement that both clears the flag and, for
// a demoted branch only, moves the state back to reviewable. Detector
// deliberately does NOT reach for UpdateState to do the second half: the
// demoted/merely-flagged distinction is already encoded in that one
// guarded UPDATE and must not be re-derived by a second writer.
type conflictClearer interface {
	ClearConflict(ctx context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error)
}

// roundOpener is the review_rounds write seam. Production binds it to
// *reviewstore.RoundStore, whose OpenRound is named by
// internal/db/queries/review_rounds.sql as having exactly three callers --
// author request-review, admin send-back, and this one, the catch-up
// auto-restore.
type roundOpener interface {
	OpenRound(ctx context.Context, workBranchID uuid.UUID, requestedBy string) (reviewstore.Round, error)
}
