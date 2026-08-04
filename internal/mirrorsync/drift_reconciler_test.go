package mirrorsync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/gitref"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// The three commits every test in this file is built out of, deliberately
// three DISTINCT values rather than the two a decision table strictly needs.
//
// The trap this avoids is specific. In production the common shape is
// accepted_tip == the work-branch tip, so a fixture built that way makes an
// implementation that compares upstream against ACCEPTED_TIP and one that
// compares it against the WORK-BRANCH TIP produce identical output for
// every case -- the comparison this whole feature is, unasserted. Every
// test below therefore keeps driftAcceptedSHA and driftWorkTipSHA
// separable, and TestReconcileDrift_UpstreamMatchesTheAcceptedTip is built
// on their disagreement on purpose.
const (
	driftAcceptedSHA = "1111111111111111111111111111111111111111"
	driftWorkTipSHA  = "2222222222222222222222222222222222222222"
	driftUpstreamSHA = "3333333333333333333333333333333333333333"
)

const driftRepoName = "acme/widgets"

// driftBranchFixture builds a reconcile-eligible work branch: non-terminal,
// with both a recorded PR number and a recorded accepted_tip, and no drift
// yet observed. state and the two SHAs are what each test varies.
func driftBranchFixture(name string, id uuid.UUID, state workbranchstore.State, acceptedTip string) workbranchstore.WorkBranch {
	tip := acceptedTip
	return workbranchstore.WorkBranch{
		ID:               id,
		Name:             name,
		Target:           "main",
		State:            state,
		UpstreamPRNumber: prNum(7),
		AcceptedTip:      &tip,
		Conflict:         workbranchstore.ConflictNone,
		UpstreamDrift:    workbranchstore.DriftNone,
	}
}

// driftHarness wires a StoreDriftReconciler over fully-configured mocks
// that record every call, so "this was never called" is a real assertion
// against a recorded slice rather than an unconfigured-mock panic.
//
// calls is a single ordered log across every WRITE seam, because the order
// of the three writes an adoption makes is a documented crash-safety
// property (review round before accepted_tip), not an implementation
// detail: asserting each mock's own call count separately would let the two
// swap places silently.
type driftHarness struct {
	reconciler *StoreDriftReconciler
	repos      *repoByNameLookupMock
	branches   *workBranchNameListerMock
	tips       *mirrorTipResolverMock
	refs       *workBranchRefAdvancerMock
	ancestry   *ancestryCheckerMock
	drift      *workBranchDriftMarkerMock
	adoption   *workBranchAdoptionWriterMock
	rounds     *roundOpenerMock
	mu         sync.Mutex
	calls      []string
}

// driftUpstream is the mirror state a test seeds: what each work branch's
// refs/heads/loam/<name> and refs/heads/loam-reserved/<name> resolve to. A
// name absent from upstream resolves as gitref.ErrRefMissing, which is the
// mirror's honest answer for a branch that was never accepted.
type driftUpstream struct {
	upstream map[string]string
	workTip  map[string]string
}

func newDriftHarness(t *testing.T, repoID uuid.UUID, refs driftUpstream, branches ...workbranchstore.WorkBranch) *driftHarness {
	t.Helper()
	h := &driftHarness{}
	h.repos = pollRepoFixture(repoID)
	h.branches = pollBranchLister(repoID, branches...)
	h.tips = &mirrorTipResolverMock{
		ResolveUpstreamProposalRefFunc: func(_ context.Context, repo, name string) (string, error) {
			if repo != driftRepoName {
				return "", errors.New("unexpected repo " + repo)
			}
			sha, ok := refs.upstream[name]
			if !ok {
				return "", gitref.ErrRefMissing
			}
			return sha, nil
		},
		ResolveWorkBranchRefFunc: func(_ context.Context, repo, name string) (string, error) {
			sha, ok := refs.workTip[name]
			if !ok {
				return "", gitref.ErrRefMissing
			}
			return sha, nil
		},
	}
	h.refs = &workBranchRefAdvancerMock{
		AdvanceWorkBranchRefFunc: func(_ context.Context, _, name, from, to string) error {
			h.record("advance:" + name + ":" + from + "->" + to)
			return nil
		},
	}
	h.ancestry = &ancestryCheckerMock{
		ContainsFunc: func(_ context.Context, _, _, ancestor, descendant string) (bool, error) {
			// The real check: is the work-branch tip contained in the
			// upstream history? The fixture SHAs are opaque, so the default
			// answer here is the one a genuine fast-forward gives, and the
			// diverged tests override it.
			return true, nil
		},
	}
	h.drift = &workBranchDriftMarkerMock{
		SetUpstreamDriftFunc: func(_ context.Context, id uuid.UUID, drift workbranchstore.UpstreamDrift) (workbranchstore.WorkBranch, error) {
			h.record("drift:" + nameOf(branches, id) + ":" + string(drift))
			return workbranchstore.WorkBranch{}, nil
		},
	}
	h.adoption = &workBranchAdoptionWriterMock{
		UpdateStateFunc: func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
			h.record("state:" + nameOf(branches, id) + ":" + string(to))
			return workbranchstore.WorkBranch{}, nil
		},
		RecordAcceptedTipFunc: func(_ context.Context, id uuid.UUID, tip string) (workbranchstore.WorkBranch, error) {
			h.record("tip:" + nameOf(branches, id) + ":" + tip)
			return workbranchstore.WorkBranch{}, nil
		},
	}
	h.rounds = &roundOpenerMock{
		OpenRoundFunc: func(_ context.Context, id uuid.UUID, requestedBy string) (reviewstore.Round, error) {
			h.record("round:" + nameOf(branches, id) + ":" + requestedBy)
			return reviewstore.Round{Number: 2, RequestedBy: requestedBy}, nil
		},
	}
	h.reconciler = NewStoreDriftReconciler(pollDataDir, pollLogger(), h.repos, h.branches, h.tips, h.refs, h.ancestry, h.drift, h.adoption, h.rounds)
	return h
}

func (h *driftHarness) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, event)
}

func (h *driftHarness) events() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// nameOf maps a work branch id back to its name so the event log reads in
// branch names rather than UUIDs.
func nameOf(branches []workbranchstore.WorkBranch, id uuid.UUID) string {
	for _, wb := range branches {
		if wb.ID == id {
			return wb.Name
		}
	}
	return "unknown-" + id.String()
}

// TestReconcileDrift_UpstreamMatchesTheAcceptedTip is the common case, and
// it is built to fail against the wrong comparison rather than merely to
// pass against the right one.
//
// The work branch has been pushed to SINCE it was accepted, so its tip and
// its accepted_tip disagree, while upstream still holds exactly what Loam
// pushed. An implementation comparing upstream against the WORK-BRANCH tip
// would see a difference here, ask about ancestry, and (the work-branch tip
// not being contained in an older upstream) flag a perfectly healthy branch
// as diverged. Nothing else in the suite would catch that, because every
// other scenario can be reached with either comparison.
func TestReconcileDrift_UpstreamMatchesTheAcceptedTip(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftAcceptedSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Empty(t, h.events(), "an upstream branch sitting exactly where Loam pushed it must change nothing")
	assert.Empty(t, h.ancestry.ContainsCalls(), "there is nothing to classify when upstream equals the accepted tip")
}

// TestReconcileDrift_FastForwardIsAdopted is the reported case
// (loam-giq.11): the admin pushed one commit straight onto loam/<name>, and
// the work-branch tip is an ancestor of it.
//
// Every write is asserted for its VALUE, not merely its occurrence: the
// swap must be from the tip that was read to the commit that arrived (a
// swap built from the wrong pair would still be "one advance call"), the
// round must be attributed to the server, and accepted_tip must absorb the
// upstream commit rather than the work-branch tip it replaced.
func TestReconcileDrift_FastForwardIsAdopted(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Equal(t, []string{
		"advance:wb-9c2f1a:" + driftWorkTipSHA + "->" + driftUpstreamSHA,
		"state:wb-9c2f1a:reviewable",
		"round:wb-9c2f1a:server",
		"tip:wb-9c2f1a:" + driftUpstreamSHA,
	}, h.events())
	require.Len(t, h.ancestry.ContainsCalls(), 1)
	call := h.ancestry.ContainsCalls()[0]
	assert.Equal(t, driftWorkTipSHA, call.Ancestor, "the fast-forward question is whether the WORK BRANCH tip is contained upstream")
	assert.Equal(t, driftUpstreamSHA, call.Descendant)
	assert.Equal(t, mirrorpath.Dir(pollDataDir, driftRepoName), call.MirrorDir)
	assert.Empty(t, call.ExtraObjectDir, "both commits are already in the mirror; there is no push quarantine here")
	assert.Empty(t, h.drift.SetUpstreamDriftCalls(), "an adopted branch was already 'none' and must not be rewritten")
}

// TestReconcileDrift_AdoptionOrdersTheRoundBeforeTheAcceptedTip pins the
// crash-safety property the write order exists for. Nothing here is
// transactional across git and Postgres: if accepted_tip were written
// first, a crash before the round left a branch that had silently adopted
// an unreviewed commit with its old approvals intact and nothing left to
// notice it, because the next tick would see upstream == accepted_tip and
// do nothing. The reverse order at worst opens one extra round.
//
// This asserts the ORDER specifically, with the round's write failing, so a
// future edit that reorders the two lines fails here rather than in a
// scenario nobody runs.
func TestReconcileDrift_AdoptionOrdersTheRoundBeforeTheAcceptedTip(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewable, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)
	wantErr := errors.New("review_rounds insert failed")
	h.rounds.OpenRoundFunc = func(context.Context, uuid.UUID, string) (reviewstore.Round, error) {
		h.record("round:attempted")
		return reviewstore.Round{}, wantErr
	}

	err := h.reconciler.ReconcileDrift(t.Context(), driftRepoName)

	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, []string{
		"advance:wb-9c2f1a:" + driftWorkTipSHA + "->" + driftUpstreamSHA,
		"round:attempted",
	}, h.events(), "a failed round must leave accepted_tip unwritten, so the next tick re-runs the adoption")
	assert.Empty(t, h.adoption.RecordAcceptedTipCalls())
}

// TestReconcileDrift_AdoptionRetryAfterACrashSkipsTheRefAdvance covers the
// state the previous test's failure leaves behind: the ref is already at
// the upstream commit, accepted_tip still is not. The pass must finish the
// job -- open the round, write the tip -- without attempting a swap whose
// "from" and "to" are the same commit.
func TestReconcileDrift_AdoptionRetryAfterACrashSkipsTheRefAdvance(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewable, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftUpstreamSHA},
	}, wb)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Equal(t, []string{
		"round:wb-9c2f1a:server",
		"tip:wb-9c2f1a:" + driftUpstreamSHA,
	}, h.events())
	assert.Empty(t, h.refs.AdvanceWorkBranchRefCalls(), "the ref is already there; a swap from a commit to itself is not a move")
}

// TestReconcileDrift_ReopensReviewPerState is the approvals-reset decision
// table. The reset is expressed only as a new round -- verdicts carry no
// stale column -- and the state move exists for the one case where leaving
// it alone would hide the branch from reviewers: the awaiting-verdict
// filter every reviewer's queue is built on matches 'reviewable' only.
func TestReconcileDrift_ReopensReviewPerState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state workbranchstore.State
		want  []string
	}{
		{
			state: workbranchstore.StateReviewed,
			want:  []string{"advance", "state:wb-9c2f1a:reviewable", "round:wb-9c2f1a:server", "tip"},
		},
		{
			state: workbranchstore.StateReviewable,
			want:  []string{"advance", "round:wb-9c2f1a:server", "tip"},
		},
		{
			// A draft branch cannot be accepted at all, and reaching
			// reviewable again means passing through request-review, which
			// opens its own round. Opening one here would invent a review
			// round for a branch nobody asked anyone to review
			// (internal/catchup reached the identical conclusion).
			state: workbranchstore.StateDraft,
			want:  []string{"advance", "tip"},
		},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			wb := driftBranchFixture("wb-9c2f1a", id, tc.state, driftAcceptedSHA)
			h := newDriftHarness(t, repoID, driftUpstream{
				upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
				workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
			}, wb)

			require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

			assert.Equal(t, tc.want, shortEvents(h.events()))
		})
	}
}

// shortEvents collapses the value-carrying advance/tip events to their kind
// so a state-machine table asserts on the shape without restating SHAs the
// value-level tests above already pin.
func shortEvents(events []string) []string {
	short := make([]string, 0, len(events))
	for _, e := range events {
		switch {
		case strings.HasPrefix(e, "advance:"):
			short = append(short, "advance")
		case strings.HasPrefix(e, "tip:"):
			short = append(short, "tip")
		default:
			short = append(short, e)
		}
	}
	return short
}

// TestReconcileDrift_DivergedIsRecordedAndNothingElseHappens is the case
// Loam refuses to guess at. Neither tip contains the other, so there is no
// non-destructive reconciliation: it records the fact and touches nothing
// else -- not the ref, not the round, not accepted_tip.
func TestReconcileDrift_DivergedIsRecordedAndNothingElseHappens(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)
	h.ancestry.ContainsFunc = func(context.Context, string, string, string, string) (bool, error) {
		return false, nil
	}

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Equal(t, []string{"drift:wb-9c2f1a:diverged"}, h.events())
}

// TestReconcileDrift_DivergedIsClearedOnceUpstreamIsReconciled is the
// level-triggered half, and it is what makes the flag survivable: there is
// no "clear drift" command and no push that clears it, because the operator
// fixes the branch on the FORGE and Loam never sees that happen. The only
// route back to 'none' is this step re-deriving it, so a branch already
// flagged whose upstream now matches again must clear on the next tick.
func TestReconcileDrift_DivergedIsClearedOnceUpstreamIsReconciled(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	wb.UpstreamDrift = workbranchstore.DriftDiverged
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftAcceptedSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Equal(t, []string{"drift:wb-9c2f1a:none"}, h.events())
}

// TestReconcileDrift_AlreadyDivergedIsNotRewritten proves the flag is not
// re-stamped every cycle. It matters beyond noise: this step runs for every
// accepted branch of every enrolled repo on every tick, and an
// unconditional write would bump updated_at on rows nothing happened to.
func TestReconcileDrift_AlreadyDivergedIsNotRewritten(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	wb.UpstreamDrift = workbranchstore.DriftDiverged
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)
	h.ancestry.ContainsFunc = func(context.Context, string, string, string, string) (bool, error) {
		return false, nil
	}

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Empty(t, h.events())
}

// TestReconcileDrift_AdoptionClearsAnEarlierDivergence covers the recovery
// path a diverged branch actually has: the operator merges the work
// branch's commits into loam/<name> upstream, which makes the work-branch
// tip an ancestor of it again. The adoption must both run AND take the flag
// down, or the branch stays unacceptable forever.
func TestReconcileDrift_AdoptionClearsAnEarlierDivergence(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewable, driftAcceptedSHA)
	wb.UpstreamDrift = workbranchstore.DriftDiverged
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Equal(t, []string{
		"advance:wb-9c2f1a:" + driftWorkTipSHA + "->" + driftUpstreamSHA,
		"round:wb-9c2f1a:server",
		"tip:wb-9c2f1a:" + driftUpstreamSHA,
		"drift:wb-9c2f1a:none",
	}, h.events())
}

// TestReconcileDrift_ARefusedSwapLeavesEverythingAlone covers an agent push
// landing between this step's read of the tip and its attempt to move it.
// The compare-and-swap is refused, and the correct response is to abandon
// the whole pass for this branch: the tip, the ancestry answer, and the
// adoption decision were all derived from a state that no longer exists.
// Writing accepted_tip or opening a round anyway would record a decision
// about a commit that is no longer the branch's tip.
func TestReconcileDrift_ARefusedSwapLeavesEverythingAlone(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)
	h.refs.AdvanceWorkBranchRefFunc = func(context.Context, string, string, string, string) error {
		h.record("advance:refused")
		return gitref.ErrRefMoved
	}

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName), "losing a race to an agent push is not a cycle failure")

	assert.Equal(t, []string{"advance:refused"}, h.events())
}

// TestReconcileDrift_AnAbsentUpstreamBranchIsSkipped covers a state that is
// reachable without anything being wrong: a forge configured to delete a
// merged PR's head branch removes loam/<name> the moment the PR merges,
// seconds before the poller flips the branch to complete.
//
// It must not clear a drift flag either, which is why the flagged branch is
// the fixture: a missing ref is not evidence that upstream has been
// reconciled.
func TestReconcileDrift_AnAbsentUpstreamBranchIsSkipped(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	wb.UpstreamDrift = workbranchstore.DriftDiverged
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Empty(t, h.events())
	assert.Empty(t, h.tips.ResolveWorkBranchRefCalls(), "there is nothing to compare, so the work-branch tip is never even read")
}

// TestReconcileDrift_AFailedAncestryCheckChangesNothing pins the rule
// internal/gitmergetree's package doc comment already paid for once: a
// check that could not be PERFORMED must never read as its negative answer.
// Here the negative answer flags the branch and blocks every future accept,
// so a corrupt mirror or a canceled context must surface as a cycle error
// instead.
func TestReconcileDrift_AFailedAncestryCheckChangesNothing(t *testing.T) {
	t.Parallel()
	repoID, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", id, workbranchstore.StateReviewed, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)
	wantErr := errors.New("mirror is corrupt")
	h.ancestry.ContainsFunc = func(context.Context, string, string, string, string) (bool, error) {
		return false, wantErr
	}

	err := h.reconciler.ReconcileDrift(t.Context(), driftRepoName)

	require.ErrorIs(t, err, wantErr)
	assert.Empty(t, h.events(), "a check that did not run must not be read as 'diverged'")
}

// TestReconcileDrift_ComparesOnlyBranchesItCanCompare pins the set. A row
// with no PR was never pushed anywhere; a row with a PR but no accepted_tip
// predates that column (loam-cgg) and has nothing to compare against; a
// terminal row is done and its upstream branch has been reaped. The one
// eligible branch here is the proof the filter is not simply excluding
// everything.
func TestReconcileDrift_ComparesOnlyBranchesItCanCompare(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	eligible := driftBranchFixture("wb-eligible", uuid.Must(uuid.NewV7()), workbranchstore.StateReviewed, driftAcceptedSHA)
	noPR := driftBranchFixture("wb-nopr", uuid.Must(uuid.NewV7()), workbranchstore.StateReviewed, driftAcceptedSHA)
	noPR.UpstreamPRNumber = nil
	noTip := driftBranchFixture("wb-notip", uuid.Must(uuid.NewV7()), workbranchstore.StateReviewed, driftAcceptedSHA)
	noTip.AcceptedTip = nil
	complete := driftBranchFixture("wb-complete", uuid.Must(uuid.NewV7()), workbranchstore.StateComplete, driftAcceptedSHA)
	closed := driftBranchFixture("wb-closed", uuid.Must(uuid.NewV7()), workbranchstore.StateClosed, driftAcceptedSHA)
	upstream := map[string]string{}
	workTip := map[string]string{}
	for _, name := range []string{"wb-eligible", "wb-nopr", "wb-notip", "wb-complete", "wb-closed"} {
		upstream[name] = driftUpstreamSHA
		workTip[name] = driftWorkTipSHA
	}
	h := newDriftHarness(t, repoID, driftUpstream{upstream: upstream, workTip: workTip}, eligible, noPR, noTip, complete, closed)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	require.Len(t, h.tips.ResolveUpstreamProposalRefCalls(), 1, "exactly one of these five branches can be compared")
	assert.Equal(t, "wb-eligible", h.tips.ResolveUpstreamProposalRefCalls()[0].Name)
}

// TestReconcileDrift_OneBranchsFailureDoesNotStarveTheRest matches
// StorePRPoller's own isolation rule: a repo can hold many accepted
// branches, and one unreadable ref must not stop every other branch's
// reconciliation. The failure is still reported, so the repo lands in
// sync_state = error rather than looking healthy.
func TestReconcileDrift_OneBranchsFailureDoesNotStarveTheRest(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	broken := driftBranchFixture("wb-broken", uuid.Must(uuid.NewV7()), workbranchstore.StateReviewable, driftAcceptedSHA)
	healthy := driftBranchFixture("wb-healthy", uuid.Must(uuid.NewV7()), workbranchstore.StateReviewable, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-broken": driftUpstreamSHA, "wb-healthy": driftUpstreamSHA},
		workTip:  map[string]string{"wb-healthy": driftWorkTipSHA},
	}, broken, healthy)

	err := h.reconciler.ReconcileDrift(t.Context(), driftRepoName)

	require.Error(t, err)
	assert.ErrorIs(t, err, gitref.ErrRefMissing)
	assert.Equal(t, []string{
		"advance:wb-healthy:" + driftWorkTipSHA + "->" + driftUpstreamSHA,
		"round:wb-healthy:server",
		"tip:wb-healthy:" + driftUpstreamSHA,
	}, h.events(), "the healthy branch is still reconciled")
}

// TestReconcileDrift_ARepoWithNothingAcceptedIsFree guards the cost of
// running this every tick for every enrolled repo: a repo whose work
// branches have never been accepted must not touch git at all.
func TestReconcileDrift_ARepoWithNothingAcceptedIsFree(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	draft := driftBranchFixture("wb-draft", uuid.Must(uuid.NewV7()), workbranchstore.StateDraft, driftAcceptedSHA)
	draft.UpstreamPRNumber = nil
	draft.AcceptedTip = nil
	h := newDriftHarness(t, repoID, driftUpstream{upstream: map[string]string{}, workTip: map[string]string{}}, draft)

	require.NoError(t, h.reconciler.ReconcileDrift(t.Context(), driftRepoName))

	assert.Empty(t, h.tips.ResolveUpstreamProposalRefCalls())
	assert.Empty(t, h.events())
}

// TestReconcileDrift_UnresolvableRepoAbortsTheStep separates the two kinds
// of failure this step can have: a per-branch one is collected and the rest
// continue, but a repo that cannot be resolved leaves nothing to iterate,
// so it aborts before any branch is read.
func TestReconcileDrift_UnresolvableRepoAbortsTheStep(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV7())
	wb := driftBranchFixture("wb-9c2f1a", uuid.Must(uuid.NewV7()), workbranchstore.StateReviewed, driftAcceptedSHA)
	h := newDriftHarness(t, repoID, driftUpstream{
		upstream: map[string]string{"wb-9c2f1a": driftUpstreamSHA},
		workTip:  map[string]string{"wb-9c2f1a": driftWorkTipSHA},
	}, wb)
	wantErr := errors.New("repo lookup failed")
	h.repos.GetRepoByNameFunc = func(context.Context, string) (reposstore.Repo, error) { return reposstore.Repo{}, wantErr }

	err := h.reconciler.ReconcileDrift(t.Context(), driftRepoName)

	require.ErrorIs(t, err, wantErr)
	assert.Empty(t, h.branches.ListByCursorCalls())
}
