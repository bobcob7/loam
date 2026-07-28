package catchup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/refpolicy"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

const (
	testDataDir  = "/var/lib/loam"
	testRepoName = "acme/widgets"
	testNewSHA   = "1111111111111111111111111111111111111111"
	testQuardir  = "/var/lib/loam/mirrors/acme/widgets.git/objects/tmp_objdir-incoming-abc"
)

// testLogger builds a discard-everything *slog.Logger, matching this
// repo's test-logger convention.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// testWorkBranch is a work branch row in the shape refpolicy.EvaluatePush
// would have fetched for the push under test: conflict is the state the
// branch was in BEFORE the push, which is what the whole demoted-versus-
// merely-flagged rule is decided on.
func testWorkBranch(conflict workbranchstore.Conflict, state workbranchstore.State) workbranchstore.WorkBranch {
	return workbranchstore.WorkBranch{
		ID:       uuid.MustParse("018f0000-0000-7000-8000-000000000001"),
		Name:     "wb-9c2f1a",
		Target:   "main",
		State:    state,
		Conflict: conflict,
	}
}

// acceptedPush is one accepted work-branch ref update carrying wb, shaped
// exactly as internal/hooksocket builds it.
func acceptedPush(wb workbranchstore.WorkBranch, newSHA string) hooksocket.AcceptedPush {
	return hooksocket.AcceptedPush{
		Repo:          testRepoName,
		QuarantineDir: testQuardir,
		WorkBranch:    wb,
		Update:        refpolicy.RefUpdate{OldSHA: "2222222222222222222222222222222222222222", NewSHA: newSHA, Ref: "refs/heads/" + wb.Name},
	}
}

// fixture wires a Detector over three mocks whose defaults are the happy
// path: the push HAS caught up, the clear succeeds, the round opens.
// Individual tests override only the one behavior they are about.
type fixture struct {
	detector  *Detector
	ancestry  *ancestryCheckerMock
	conflicts *conflictClearerMock
	rounds    *roundOpenerMock
}

func newFixture(t *testing.T, cleared workbranchstore.WorkBranch) fixture {
	t.Helper()
	ancestry := &ancestryCheckerMock{
		ContainsFunc: func(_ context.Context, _, _, _, _ string) (bool, error) { return true, nil },
	}
	conflicts := &conflictClearerMock{
		ClearConflictFunc: func(_ context.Context, _ uuid.UUID) (workbranchstore.WorkBranch, error) { return cleared, nil },
	}
	rounds := &roundOpenerMock{
		OpenRoundFunc: func(_ context.Context, workBranchID uuid.UUID, requestedBy string) (reviewstore.Round, error) {
			return reviewstore.Round{WorkBranchID: workBranchID, Number: 2, RequestedBy: requestedBy}, nil
		},
	}
	return fixture{
		detector:  New(testDataDir, ancestry, conflicts, rounds, testLogger()),
		ancestry:  ancestry,
		conflicts: conflicts,
		rounds:    rounds,
	}
}

// TestOnAcceptedPush_DemotedBranchCaughtUp_ClearsAndOpensARound is the
// bead's Definition of Done for the DEMOTED half of the conditional rule
// (loam-lb6, docs/git-spec.md -> "Target Advances & Catch-Up"): a branch
// whose conflict was 'reset' -- it had been reviewable/reviewed and was
// demoted to draft -- flips back to reviewable on catch-up, and because
// that IS a transition into reviewable it opens a fresh numbered round
// attributed to the server.
func TestOnAcceptedPush_DemotedBranchCaughtUp_ClearsAndOpensARound(t *testing.T) {
	t.Parallel()
	wb := testWorkBranch(workbranchstore.ConflictReset, workbranchstore.StateDraft)
	f := newFixture(t, testWorkBranch(workbranchstore.ConflictNone, workbranchstore.StateReviewable))
	f.detector.OnAcceptedPush(t.Context(), acceptedPush(wb, testNewSHA))
	require.Len(t, f.conflicts.ClearConflictCalls(), 1, "a caught-up demoted branch must have its conflict cleared")
	assert.Equal(t, wb.ID, f.conflicts.ClearConflictCalls()[0].ID)
	require.Len(t, f.rounds.OpenRoundCalls(), 1, "the restore is a transition into reviewable, so it must open a new numbered round -- staleness is derived from MAX(review_rounds.number), so resuming the old round would keep pre-conflict verdicts counting as current")
	assert.Equal(t, wb.ID, f.rounds.OpenRoundCalls()[0].WorkBranchID)
	assert.Equal(t, "server", f.rounds.OpenRoundCalls()[0].RequestedBy, "a catch-up round must be attributable to the server, not to any agent or to admin")
}

// TestOnAcceptedPush_MerelyFlaggedDraftCaughtUp_ClearsWithoutARound is the
// regression this bead exists to guard against, and the half nothing in
// the tree caught before: a branch that was flagged while draft and stayed
// draft throughout loses only its flag. There is no transition into
// reviewable, so opening a round here would invent a review round for a
// branch nobody has asked anyone to review.
func TestOnAcceptedPush_MerelyFlaggedDraftCaughtUp_ClearsWithoutARound(t *testing.T) {
	t.Parallel()
	wb := testWorkBranch(workbranchstore.ConflictFlagged, workbranchstore.StateDraft)
	f := newFixture(t, testWorkBranch(workbranchstore.ConflictNone, workbranchstore.StateDraft))
	f.detector.OnAcceptedPush(t.Context(), acceptedPush(wb, testNewSHA))
	require.Len(t, f.conflicts.ClearConflictCalls(), 1, "a merely flagged branch still loses its flag on catch-up")
	assert.Empty(t, f.rounds.OpenRoundCalls(), "a merely flagged branch never transitioned into reviewable, so no round may be opened")
}

// TestOnAcceptedPush_FlaggedBranchAlreadyReviewable_ClearsWithoutARound
// pins the edge the post-clear ROW cannot distinguish: a branch flagged
// while draft, then moved to reviewable by an ordinary request-review,
// reads back as reviewable after the clear despite never having been
// demoted -- and it already has the round request-review opened. Deciding
// on the returned state rather than the pre-push conflict value would give
// it a spurious second round.
func TestOnAcceptedPush_FlaggedBranchAlreadyReviewable_ClearsWithoutARound(t *testing.T) {
	t.Parallel()
	wb := testWorkBranch(workbranchstore.ConflictFlagged, workbranchstore.StateReviewable)
	f := newFixture(t, testWorkBranch(workbranchstore.ConflictNone, workbranchstore.StateReviewable))
	f.detector.OnAcceptedPush(t.Context(), acceptedPush(wb, testNewSHA))
	require.Len(t, f.conflicts.ClearConflictCalls(), 1)
	assert.Empty(t, f.rounds.OpenRoundCalls(), "'flagged' means never demoted, whatever state the cleared row reports")
}

// TestOnAcceptedPush_NotCaughtUp_LeavesEverythingAlone proves docs/git-
// spec.md's "If the target has advanced again since the reset, the flag
// simply stays until a push catches up": a push whose history does not
// contain the CURRENT target tip writes nothing at all.
func TestOnAcceptedPush_NotCaughtUp_LeavesEverythingAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t, workbranchstore.WorkBranch{})
	f.ancestry.ContainsFunc = func(_ context.Context, _, _, _, _ string) (bool, error) { return false, nil }
	f.detector.OnAcceptedPush(t.Context(), acceptedPush(testWorkBranch(workbranchstore.ConflictReset, workbranchstore.StateDraft), testNewSHA))
	assert.Empty(t, f.conflicts.ClearConflictCalls(), "a branch still behind the target keeps its conflict flag")
	assert.Empty(t, f.rounds.OpenRoundCalls())
}

// TestOnAcceptedPush_AsksGitWhetherTheHistoryContainsTheCurrentTargetTip
// pins the shape of the question itself: the branch's history (the pushed
// new SHA) must CONTAIN the target's ref, resolved live from the bare
// mirror at check time -- not the reverse relation, not a SHA recorded
// when the conflict was flagged. Argument order is load-bearing:
// Contains(ancestor, descendant) with the operands swapped answers "has
// the target absorbed this branch", which is a different (and for an
// unmerged branch, false) question.
func TestOnAcceptedPush_AsksGitWhetherTheHistoryContainsTheCurrentTargetTip(t *testing.T) {
	t.Parallel()
	f := newFixture(t, testWorkBranch(workbranchstore.ConflictNone, workbranchstore.StateReviewable))
	f.detector.OnAcceptedPush(t.Context(), acceptedPush(testWorkBranch(workbranchstore.ConflictReset, workbranchstore.StateDraft), testNewSHA))
	require.Len(t, f.ancestry.ContainsCalls(), 1)
	call := f.ancestry.ContainsCalls()[0]
	assert.Equal(t, filepath.Join(testDataDir, "mirrors", testRepoName+".git"), call.MirrorDir, "the check must run against the repo's own bare mirror")
	assert.Equal(t, testQuardir, call.ExtraObjectDir, "the pushed objects are still quarantined; without this the new tip does not resolve at all")
	assert.Equal(t, "refs/heads/main", call.Ancestor, "the ancestor is the target's ref, read live so a target that advanced again is what the push is measured against")
	assert.Equal(t, testNewSHA, call.Descendant, "the descendant is the pushed tip")
}

// TestOnAcceptedPush_UnflaggedBranch_NeverEvenAsksGit proves the ordinary
// push -- the overwhelmingly common case -- costs no git subprocess and no
// database write. It also covers the zero-value WorkBranch (Conflict ""),
// which must short-circuit here rather than reaching git with an empty
// target branch name.
func TestOnAcceptedPush_UnflaggedBranch_NeverEvenAsksGit(t *testing.T) {
	t.Parallel()
	for _, conflict := range []workbranchstore.Conflict{workbranchstore.ConflictNone, ""} {
		t.Run(string("conflict="+conflict), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, workbranchstore.WorkBranch{})
			f.detector.OnAcceptedPush(t.Context(), acceptedPush(testWorkBranch(conflict, workbranchstore.StateDraft), testNewSHA))
			assert.Empty(t, f.ancestry.ContainsCalls(), "an unflagged branch has nothing to clear, so it must not pay for a git invocation")
			assert.Empty(t, f.conflicts.ClearConflictCalls())
			assert.Empty(t, f.rounds.OpenRoundCalls())
		})
	}
}

// TestOnAcceptedPush_DeletedRef_NeverEvenAsksGit proves an all-zero new
// SHA (git's own wire encoding for a ref deletion) is dropped before git
// is asked anything. Asking whether the target tip is an ancestor of
// "000...0" is a check FAILURE, not an answer.
func TestOnAcceptedPush_DeletedRef_NeverEvenAsksGit(t *testing.T) {
	t.Parallel()
	for _, sha := range []string{"0000000000000000000000000000000000000000", ""} {
		t.Run("new_sha="+sha, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, workbranchstore.WorkBranch{})
			f.detector.OnAcceptedPush(t.Context(), acceptedPush(testWorkBranch(workbranchstore.ConflictReset, workbranchstore.StateDraft), sha))
			assert.Empty(t, f.ancestry.ContainsCalls())
			assert.Empty(t, f.conflicts.ClearConflictCalls())
		})
	}
}

// TestOnAcceptedPush_AncestryCheckFailed_WritesNothing proves "we could
// not check" is never treated as either answer. A failed check must leave
// the work branch exactly as it was -- the same rule internal/mirrorsync's
// mergeability checker follows in the other direction, where a failed
// merge-tree must not demote a reviewable branch.
func TestOnAcceptedPush_AncestryCheckFailed_WritesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, workbranchstore.WorkBranch{})
	f.ancestry.ContainsFunc = func(_ context.Context, _, _, _, _ string) (bool, error) {
		return false, errors.New("mirror is unreadable")
	}
	f.detector.OnAcceptedPush(t.Context(), acceptedPush(testWorkBranch(workbranchstore.ConflictReset, workbranchstore.StateDraft), testNewSHA))
	assert.Empty(t, f.conflicts.ClearConflictCalls(), "a check that did not run must not clear a flag")
	assert.Empty(t, f.rounds.OpenRoundCalls())
}

// TestOnAcceptedPush_ClearFailed_NoRoundIsOpened proves the round is
// strictly downstream of the clear landing. A round opened after a failed
// clear would make the branch's pre-conflict verdicts derive as stale
// while the branch itself is still flagged and still demoted -- state no
// other path in this system can produce.
func TestOnAcceptedPush_ClearFailed_NoRoundIsOpened(t *testing.T) {
	t.Parallel()
	f := newFixture(t, workbranchstore.WorkBranch{})
	f.conflicts.ClearConflictFunc = func(_ context.Context, _ uuid.UUID) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, workbranchstore.ErrIllegalTransition
	}
	f.detector.OnAcceptedPush(t.Context(), acceptedPush(testWorkBranch(workbranchstore.ConflictReset, workbranchstore.StateDraft), testNewSHA))
	require.Len(t, f.conflicts.ClearConflictCalls(), 1)
	assert.Empty(t, f.rounds.OpenRoundCalls(), "no round may be opened when the transition itself did not land")
}

// TestOnAcceptedPush_OpenRoundFailed_IsSurvivable proves a failed round
// insert is logged and abandoned rather than panicking or retrying: the
// push has already been accepted and there is nothing to undo. The
// resulting state (flag cleared, branch reviewable, round not yet bumped)
// is recoverable -- exactly the shape internal/handler/workbranch's own
// RequestReview self-heal already knows how to repair.
func TestOnAcceptedPush_OpenRoundFailed_IsSurvivable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, testWorkBranch(workbranchstore.ConflictNone, workbranchstore.StateReviewable))
	f.rounds.OpenRoundFunc = func(_ context.Context, _ uuid.UUID, _ string) (reviewstore.Round, error) {
		return reviewstore.Round{}, errors.New("database is unreachable")
	}
	assert.NotPanics(t, func() {
		f.detector.OnAcceptedPush(t.Context(), acceptedPush(testWorkBranch(workbranchstore.ConflictReset, workbranchstore.StateDraft), testNewSHA))
	})
	require.Len(t, f.rounds.OpenRoundCalls(), 1)
}
