package mirrorsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

const (
	pollRepoName    = "acme/widgets"
	pollForgeHost   = "forge.example.com"
	pollUpstreamURL = "https://forge.example.com/acme/widgets.git"
	pollDataDir     = "/srv/loam"
)

func pollLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// pollRepoFixture resolves pollRepoName to a repo row carrying the forge
// host and upstream URL branch cleanup needs.
func pollRepoFixture(repoID uuid.UUID) *repoByNameLookupMock {
	return &repoByNameLookupMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			if name != pollRepoName {
				return reposstore.Repo{}, errors.New("unexpected repo name " + name)
			}
			return reposstore.Repo{ID: repoID, Name: name, ForgeHost: pollForgeHost, UpstreamURL: pollUpstreamURL}, nil
		},
	}
}

// pollBranchLister returns a workBranchNameLister serving branches as a
// single page.
func pollBranchLister(repoID uuid.UUID, branches ...workbranchstore.WorkBranch) *workBranchNameListerMock {
	served := false
	return &workBranchNameListerMock{
		ListByCursorFunc: func(_ context.Context, filter workbranchstore.ListFilter, _ int32, _ *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			if filter.RepoID == nil || *filter.RepoID != repoID {
				return nil, errors.New("work branch list was not scoped to the repo")
			}
			if served {
				return nil, nil
			}
			served = true
			return branches, nil
		},
	}
}

// prNum is a *int32 helper for work_branches.upstream_pr_number.
func prNum(n int32) *int32 { return &n }

// workBranchFixture builds a poll-eligible work branch: reviewed, with a
// recorded PR number.
func workBranchFixture(name string, id uuid.UUID, number int32) workbranchstore.WorkBranch {
	return workbranchstore.WorkBranch{
		ID:               id,
		Name:             name,
		Target:           "main",
		State:            workbranchstore.StateReviewed,
		UpstreamPRNumber: prNum(number),
	}
}

// pollHarness wires a StorePRPoller over fully-configured mocks that record
// every call, so a test asserting "this was never called" fails on a real
// assertion against a recorded slice rather than on an unconfigured-mock
// panic.
type pollHarness struct {
	poller     *StorePRPoller
	completed  *[]uuid.UUID
	closed     *[]string
	deleted    *[]string
	closedPRs  *[]int
	stateCalls *[]int
}

type pollHarnessOpts struct {
	states       map[int]string
	stateErr     error
	completeErr  error
	closeErr     error
	deleteErr    error
	closePRErr   error
	repos        *repoByNameLookupMock
	branchLister *workBranchNameListerMock
}

func newPollHarness(t *testing.T, repoID uuid.UUID, opts pollHarnessOpts, branches ...workbranchstore.WorkBranch) pollHarness {
	t.Helper()
	completed := new([]uuid.UUID)
	closed := new([]string)
	deleted := new([]string)
	closedPRs := new([]int)
	stateCalls := new([]int)
	repos := opts.repos
	if repos == nil {
		repos = pollRepoFixture(repoID)
	}
	lister := opts.branchLister
	if lister == nil {
		lister = pollBranchLister(repoID, branches...)
	}
	transitions := &workBranchTerminatorMock{
		CompleteFunc: func(_ context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error) {
			*completed = append(*completed, id)
			if opts.completeErr != nil {
				return workbranchstore.WorkBranch{}, opts.completeErr
			}
			return workbranchstore.WorkBranch{ID: id, State: workbranchstore.StateComplete}, nil
		},
		CloseFunc: func(_ context.Context, id uuid.UUID, reason string) (workbranchstore.WorkBranch, error) {
			*closed = append(*closed, id.String()+"|"+reason)
			if opts.closeErr != nil {
				return workbranchstore.WorkBranch{}, opts.closeErr
			}
			return workbranchstore.WorkBranch{ID: id, State: workbranchstore.StateClosed}, nil
		},
	}
	tracker := &pullRequestTrackerMock{
		GetPRStateFunc: func(_ context.Context, repo string, prNumber int) (string, error) {
			*stateCalls = append(*stateCalls, prNumber)
			assert.Equal(t, pollRepoName, repo, "GetPRState must be called with repos.name, the forge's own <group>/<repo> path")
			if opts.stateErr != nil {
				return "", opts.stateErr
			}
			return opts.states[prNumber], nil
		},
		ClosePRFunc: func(_ context.Context, _ string, prNumber int) error {
			*closedPRs = append(*closedPRs, prNumber)
			return opts.closePRErr
		},
	}
	deleter := &upstreamRefDeleterMock{
		DeleteRemoteRefFunc: func(_ context.Context, host, mirrorDir, upstreamURL, ref string) ([]byte, error) {
			assert.Equal(t, pollForgeHost, host)
			assert.Equal(t, pollDataDir+"/mirrors/"+pollRepoName+".git", mirrorDir)
			assert.Equal(t, pollUpstreamURL, upstreamURL)
			*deleted = append(*deleted, ref)
			return nil, opts.deleteErr
		},
	}
	return pollHarness{
		poller:     NewStorePRPoller(pollDataDir, pollLogger(), repos, lister, transitions, tracker, deleter),
		completed:  completed,
		closed:     closed,
		deleted:    deleted,
		closedPRs:  closedPRs,
		stateCalls: stateCalls,
	}
}

// TestPollPRs_MergedCompletesAndDeletesUpstreamBranch is the bead's primary
// path: a merged PR completes the work branch and removes the loam/ branch
// it was pushed from. The ref assertion is exact, not a prefix check --
// deleting the wrong ref is the one irreversible mistake this code can
// make.
func TestPollPRs_MergedCompletesAndDeletesUpstreamBranch(t *testing.T) {
	t.Parallel()
	repoID, wbID := uuid.New(), uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: prStateMerged}}, workBranchFixture("wb-9c2f1a", wbID, 7))
	require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
	assert.Equal(t, []uuid.UUID{wbID}, *h.completed)
	assert.Empty(t, *h.closed, "a merged PR must complete the branch, never close it")
	assert.Equal(t, []string{"refs/heads/loam/wb-9c2f1a"}, *h.deleted)
}

// TestPollPRs_ClosedClosesBranchWithReasonAndDeletesUpstreamBranch is the
// closed-without-merge path. The recorded close_reason is asserted because
// it is what distinguishes this row from an admin close in the database.
func TestPollPRs_ClosedClosesBranchWithReasonAndDeletesUpstreamBranch(t *testing.T) {
	t.Parallel()
	repoID, wbID := uuid.New(), uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: prStateClosed}}, workBranchFixture("wb-9c2f1a", wbID, 7))
	require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
	assert.Equal(t, []string{wbID.String() + "|" + closedUpstreamPRReason}, *h.closed)
	assert.Empty(t, *h.completed, "a closed-without-merge PR must never complete the branch")
	assert.Equal(t, []string{"refs/heads/loam/wb-9c2f1a"}, *h.deleted)
}

// TestPollPRs_OpenTransitionsNothing pins the do-nothing case: an open PR
// must not transition the branch and must not touch its upstream branch.
func TestPollPRs_OpenTransitionsNothing(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: prStateOpen}}, workBranchFixture("wb-9c2f1a", uuid.New(), 7))
	require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
	assert.Equal(t, []int{7}, *h.stateCalls, "an open PR must still have been polled")
	assert.Empty(t, *h.completed)
	assert.Empty(t, *h.closed)
	assert.Empty(t, *h.deleted, "an open PR's branch is still live -- it must not be deleted")
}

// TestPollPRs_UnknownStateIsNonDestructive is the load-bearing safety test.
// A state string the Provider contract does not define -- including the
// empty string a half-failed decode would produce -- must transition
// nothing and delete nothing, and must surface as an error rather than
// being quietly rounded to the nearest terminal state. Reading an unknown
// state as "closed" would tear down a live work branch.
func TestPollPRs_UnknownStateIsNonDestructive(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"", "merging", "reopened", "CLOSED", "Merged"} {
		t.Run("state="+state, func(t *testing.T) {
			t.Parallel()
			repoID := uuid.New()
			h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: state}}, workBranchFixture("wb-9c2f1a", uuid.New(), 7))
			err := h.poller.PollPRs(t.Context(), RepoID(pollRepoName))
			require.Error(t, err)
			assert.ErrorIs(t, err, errUnknownPRState)
			assert.Empty(t, *h.completed)
			assert.Empty(t, *h.closed)
			assert.Empty(t, *h.deleted)
		})
	}
}

// TestPollPRs_LookupFailureIsNotTreatedAsClosed is the same guarantee for
// the other half of "the poll itself failed": a transport error, a forge
// 5xx, or a 404 must never be interpreted as a terminal PR state. The
// branch stays exactly as it was and its upstream branch stays put.
func TestPollPRs_LookupFailureIsNotTreatedAsClosed(t *testing.T) {
	t.Parallel()
	lookupErr := errors.New("forge: 503 service unavailable")
	repoID := uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{stateErr: lookupErr}, workBranchFixture("wb-9c2f1a", uuid.New(), 7))
	err := h.poller.PollPRs(t.Context(), RepoID(pollRepoName))
	require.Error(t, err)
	assert.ErrorIs(t, err, lookupErr)
	assert.Empty(t, *h.completed)
	assert.Empty(t, *h.closed)
	assert.Empty(t, *h.deleted, "a failed poll must never delete a live work branch's upstream branch")
}

// TestPollPRs_OneBranchFailureDoesNotStarveTheRest proves per-branch
// failure isolation: the first branch's forge lookup failing must not stop
// the second branch's merged PR from completing.
func TestPollPRs_OneBranchFailureDoesNotStarveTheRest(t *testing.T) {
	t.Parallel()
	repoID, goodID := uuid.New(), uuid.New()
	lookupErr := errors.New("forge: 503 service unavailable")
	var stateCalls []int
	tracker := &pullRequestTrackerMock{
		GetPRStateFunc: func(_ context.Context, _ string, prNumber int) (string, error) {
			stateCalls = append(stateCalls, prNumber)
			if prNumber == 1 {
				return "", lookupErr
			}
			return prStateMerged, nil
		},
		ClosePRFunc: func(context.Context, string, int) error { return nil },
	}
	var completed []uuid.UUID
	transitions := &workBranchTerminatorMock{
		CompleteFunc: func(_ context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error) {
			completed = append(completed, id)
			return workbranchstore.WorkBranch{}, nil
		},
		CloseFunc: func(context.Context, uuid.UUID, string) (workbranchstore.WorkBranch, error) {
			return workbranchstore.WorkBranch{}, errors.New("Close must not be called")
		},
	}
	var deleted []string
	deleter := &upstreamRefDeleterMock{
		DeleteRemoteRefFunc: func(_ context.Context, _, _, _, ref string) ([]byte, error) {
			deleted = append(deleted, ref)
			return nil, nil
		},
	}
	poller := NewStorePRPoller(pollDataDir, pollLogger(), pollRepoFixture(repoID),
		pollBranchLister(repoID, workBranchFixture("wb-aaa", uuid.New(), 1), workBranchFixture("wb-bbb", goodID, 2)),
		transitions, tracker, deleter)
	err := poller.PollPRs(t.Context(), RepoID(pollRepoName))
	require.Error(t, err)
	assert.ErrorIs(t, err, lookupErr)
	assert.Equal(t, []int{1, 2}, stateCalls, "the second branch must still be polled after the first one failed")
	assert.Equal(t, []uuid.UUID{goodID}, completed)
	assert.Equal(t, []string{"refs/heads/loam/wb-bbb"}, deleted)
}

// TestPollPRs_PollSetExcludesBranchesWithoutARecordedPR proves the poll set
// is chosen on PR presence: a branch whose upstream_pr_number is NULL is
// never polled at all, so a work branch that was never accepted cannot be
// transitioned by this step.
func TestPollPRs_PollSetExcludesBranchesWithoutARecordedPR(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	noPR := workBranchFixture("wb-nopr", uuid.New(), 0)
	noPR.UpstreamPRNumber = nil
	h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: prStateMerged}}, noPR, workBranchFixture("wb-withpr", uuid.New(), 7))
	require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
	assert.Equal(t, []int{7}, *h.stateCalls, "only the branch with a recorded PR number may be polled")
}

// TestPollPRs_PollSetKeepsNonTerminalStatesIncludingDraft is the bead's
// explicit DESIGN point: the poll set is PR-presence-driven, not
// state-driven, because a conflicting target advance can reset an accepted
// proposal all the way back to draft while leaving its PR open and
// untouched (docs/git-spec.md -> Target Advances & Catch-Up). Dropping
// draft/reviewable from the poll set would strand exactly those branches.
func TestPollPRs_PollSetKeepsNonTerminalStatesIncludingDraft(t *testing.T) {
	t.Parallel()
	for _, state := range []workbranchstore.State{workbranchstore.StateDraft, workbranchstore.StateReviewable, workbranchstore.StateReviewed} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			repoID, wbID := uuid.New(), uuid.New()
			wb := workBranchFixture("wb-9c2f1a", wbID, 7)
			wb.State = state
			h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: prStateMerged}}, wb)
			require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
			assert.Equal(t, []int{7}, *h.stateCalls)
			assert.Equal(t, []uuid.UUID{wbID}, *h.completed, "the forge merge is authoritative whatever conflict state the branch was reset into")
		})
	}
}

// TestPollPRs_AlreadyTerminalBranchIsNeverRepolled is the idempotency
// proof: once a branch is complete or closed it leaves the poll set
// permanently, so a second tick over the same already-merged PR issues no
// forge call, no transition, and no second delete.
func TestPollPRs_AlreadyTerminalBranchIsNeverRepolled(t *testing.T) {
	t.Parallel()
	for _, state := range []workbranchstore.State{workbranchstore.StateComplete, workbranchstore.StateClosed} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			repoID := uuid.New()
			wb := workBranchFixture("wb-9c2f1a", uuid.New(), 7)
			wb.State = state
			h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: prStateMerged}}, wb)
			require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
			assert.Empty(t, *h.stateCalls, "a terminal branch must not even be polled")
			assert.Empty(t, *h.completed)
			assert.Empty(t, *h.closed)
			assert.Empty(t, *h.deleted, "a terminal branch's upstream branch must not be re-deleted every tick")
		})
	}
}

// TestPollPRs_FailedTransitionSkipsBranchCleanup pins the ordering
// invariant: the delete is a consequence of a committed terminal state
// change, never of the forge's answer alone. If the store write fails the
// branch is still live, so its upstream branch must survive for the next
// tick to retry.
func TestPollPRs_FailedTransitionSkipsBranchCleanup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		opts  pollHarnessOpts
	}{
		{"complete fails", prStateMerged, pollHarnessOpts{completeErr: errors.New("postgres: connection reset")}},
		{"close fails", prStateClosed, pollHarnessOpts{closeErr: errors.New("postgres: connection reset")}},
		{"complete rejected as illegal", prStateMerged, pollHarnessOpts{completeErr: workbranchstore.ErrIllegalTransition}},
		{"close rejected as illegal", prStateClosed, pollHarnessOpts{closeErr: workbranchstore.ErrIllegalTransition}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repoID := uuid.New()
			opts := tt.opts
			opts.states = map[int]string{7: tt.state}
			h := newPollHarness(t, repoID, opts, workBranchFixture("wb-9c2f1a", uuid.New(), 7))
			err := h.poller.PollPRs(t.Context(), RepoID(pollRepoName))
			require.Error(t, err)
			assert.Empty(t, *h.deleted, "cleanup must only follow a transition that actually committed")
		})
	}
}

// TestPollPRs_DeleteFailureIsSwallowed proves branch cleanup is genuinely
// best-effort (docs/sync-spec.md: "failures are ignored -- forges with
// auto-delete-on-merge make this a no-op"): the transition already
// committed, so a failed delete must not drag the repo into sync_state =
// error.
func TestPollPRs_DeleteFailureIsSwallowed(t *testing.T) {
	t.Parallel()
	repoID, wbID := uuid.New(), uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{
		states:    map[int]string{7: prStateMerged},
		deleteErr: errors.New("remote: ref does not exist"),
	}, workBranchFixture("wb-9c2f1a", wbID, 7))
	assert.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)), "a best-effort cleanup failure must not fail the cycle")
	assert.Equal(t, []uuid.UUID{wbID}, *h.completed, "the transition still stands")
}

// TestPollPRs_UnsafeWorkBranchNameDeletesNothing proves the ref this poller
// builds can never escape refs/heads/loam/: a row whose name column would
// interpolate into some other ref, or into a git flag, deletes nothing at
// all. The transition still commits -- refusing to clean up is safe;
// cleaning up the wrong ref is not.
func TestPollPRs_UnsafeWorkBranchNameDeletesNothing(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "../main", "wb/../../main", "-D", "wb 1", "refs/heads/main", "wb..x"} {
		t.Run("name="+name, func(t *testing.T) {
			t.Parallel()
			repoID, wbID := uuid.New(), uuid.New()
			h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{7: prStateMerged}}, workBranchFixture(name, wbID, 7))
			require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
			assert.Equal(t, []uuid.UUID{wbID}, *h.completed)
			assert.Empty(t, *h.deleted, "an unsafe name must produce no delete at all, not a best-effort one")
		})
	}
}

// TestPollPRs_CanceledContextStopsBeforeTransitioning proves a canceled
// context aborts the loop instead of burning through every remaining branch
// against a dead context -- and, more importantly, that cancellation is
// reported as an error rather than silently ending the poll as if every
// branch had been checked.
func TestPollPRs_CanceledContextStopsBeforeTransitioning(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{1: prStateMerged, 2: prStateMerged}},
		workBranchFixture("wb-aaa", uuid.New(), 1), workBranchFixture("wb-bbb", uuid.New(), 2))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := h.poller.PollPRs(ctx, RepoID(pollRepoName))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, *h.stateCalls, "a canceled poll must not issue forge calls")
	assert.Empty(t, *h.completed)
	assert.Empty(t, *h.deleted)
}

// TestPollPRs_PollsInStableNameOrder pins the deterministic ordering the
// poll set imposes, so a store that pages rows back in an arbitrary order
// cannot make the sequence of forge calls (and therefore the sequence of
// deletes) vary between ticks.
func TestPollPRs_PollsInStableNameOrder(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{states: map[int]string{1: prStateOpen, 2: prStateOpen, 3: prStateOpen}},
		workBranchFixture("wb-ccc", uuid.New(), 3), workBranchFixture("wb-aaa", uuid.New(), 1), workBranchFixture("wb-bbb", uuid.New(), 2))
	require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
	assert.Equal(t, []int{1, 2, 3}, *h.stateCalls)
}

// TestPollPRs_PagesThroughEveryWorkBranch proves the poll set pages rather
// than reading only the first page: a repo with more work branches than one
// page holds must still have its later branches polled.
func TestPollPRs_PagesThroughEveryWorkBranch(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	total := workBranchListPageSize + 2
	all := make([]workbranchstore.WorkBranch, 0, total)
	states := make(map[int]string, total)
	for i := range total {
		all = append(all, workBranchFixture("wb-"+string(rune('a'+i%26))+uuid.NewString(), uuid.New(), int32(i+1)))
		states[i+1] = prStateOpen
	}
	pos := 0
	lister := &workBranchNameListerMock{
		ListByCursorFunc: func(_ context.Context, _ workbranchstore.ListFilter, limit int32, _ *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			if pos >= len(all) {
				return nil, nil
			}
			end := min(pos+int(limit), len(all))
			page := all[pos:end]
			pos = end
			return page, nil
		},
	}
	h := newPollHarness(t, repoID, pollHarnessOpts{states: states, branchLister: lister})
	require.NoError(t, h.poller.PollPRs(t.Context(), RepoID(pollRepoName)))
	assert.Len(t, *h.stateCalls, total, "every page's branches must be polled, not just the first page's")
}

// TestPollPRs_RepoLookupFailureAbortsTheStep covers the two whole-step
// failures: without the repo row (or its work branches) there is nothing to
// poll, so the error propagates instead of being swallowed into an empty
// poll set that would look identical to "no proposals are open."
func TestPollPRs_RepoLookupFailureAbortsTheStep(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	lookupErr := errors.New("postgres: connection refused")
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, lookupErr
		},
	}
	h := newPollHarness(t, repoID, pollHarnessOpts{repos: repos}, workBranchFixture("wb-9c2f1a", uuid.New(), 7))
	err := h.poller.PollPRs(t.Context(), RepoID(pollRepoName))
	require.Error(t, err)
	assert.ErrorIs(t, err, lookupErr)
	assert.Empty(t, *h.stateCalls)
}

// TestPollPRs_BranchListFailureAbortsTheStep is the other whole-step
// failure.
func TestPollPRs_BranchListFailureAbortsTheStep(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	listErr := errors.New("postgres: connection refused")
	lister := &workBranchNameListerMock{
		ListByCursorFunc: func(context.Context, workbranchstore.ListFilter, int32, *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			return nil, listErr
		},
	}
	h := newPollHarness(t, repoID, pollHarnessOpts{branchLister: lister})
	err := h.poller.PollPRs(t.Context(), RepoID(pollRepoName))
	require.Error(t, err)
	assert.ErrorIs(t, err, listErr)
	assert.Empty(t, *h.stateCalls)
}

// TestCleanupUpstreamBranch_DeletesTheLoamRef covers the exported helper
// loam-ofg.14's admin-close path calls instead of reimplementing the
// delete: it must resolve the repo itself and produce the identical ref.
func TestCleanupUpstreamBranch_DeletesTheLoamRef(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{})
	h.poller.CleanupUpstreamBranch(t.Context(), RepoID(pollRepoName), "wb-9c2f1a")
	assert.Equal(t, []string{"refs/heads/loam/wb-9c2f1a"}, *h.deleted)
}

// TestCleanupUpstreamBranch_RepoLookupFailureDeletesNothing proves the
// exported helper cannot fall back to a partially-resolved delete when it
// cannot resolve the repo's upstream coordinates.
func TestCleanupUpstreamBranch_RepoLookupFailureDeletesNothing(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, errors.New("postgres: connection refused")
		},
	}
	h := newPollHarness(t, repoID, pollHarnessOpts{repos: repos})
	h.poller.CleanupUpstreamBranch(t.Context(), RepoID(pollRepoName), "wb-9c2f1a")
	assert.Empty(t, *h.deleted)
}

// TestClosePRAndCleanup_ClosesThenDeletes covers the admin-close direction
// end to end.
func TestClosePRAndCleanup_ClosesThenDeletes(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{})
	require.NoError(t, h.poller.ClosePRAndCleanup(t.Context(), RepoID(pollRepoName), "wb-9c2f1a", 7))
	assert.Equal(t, []int{7}, *h.closedPRs)
	assert.Equal(t, []string{"refs/heads/loam/wb-9c2f1a"}, *h.deleted)
}

// TestClosePRAndCleanup_AlreadyMergedStillCleansUp pins this bead's NOTES:
// a real Forgejo 9.0.3 refuses PATCH state=closed on a MERGED PR with a 412
// (forge.ErrPRAlreadyMerged) and leaves the state untouched. That is
// success-equivalent -- the PR is already terminal -- so the branch cleanup
// must still run, and it must not be retried as a close failure.
func TestClosePRAndCleanup_AlreadyMergedStillCleansUp(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	h := newPollHarness(t, repoID, pollHarnessOpts{closePRErr: forge.ErrPRAlreadyMerged})
	require.NoError(t, h.poller.ClosePRAndCleanup(t.Context(), RepoID(pollRepoName), "wb-9c2f1a", 7),
		"a 412 on an already-merged PR is success-equivalent, not a close failure a caller should retry")
	assert.Equal(t, []int{7}, *h.closedPRs, "ClosePR is attempted exactly once, never retried")
	assert.Equal(t, []string{"refs/heads/loam/wb-9c2f1a"}, *h.deleted, "a merged PR's head branch is exactly the branch that needs removing")
}

// TestClosePRAndCleanup_CloseFailureStillCleansUp keeps the two best-effort
// halves independent: a genuine close failure must not also cost the branch
// cleanup.
func TestClosePRAndCleanup_CloseFailureStillCleansUp(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	closeErr := errors.New("forge: 503 service unavailable")
	h := newPollHarness(t, repoID, pollHarnessOpts{closePRErr: closeErr})
	err := h.poller.ClosePRAndCleanup(t.Context(), RepoID(pollRepoName), "wb-9c2f1a", 7)
	require.Error(t, err, "a genuine close failure must be distinguishable from an already-merged rejection")
	assert.ErrorIs(t, err, closeErr)
	assert.Equal(t, []string{"refs/heads/loam/wb-9c2f1a"}, *h.deleted)
}

// TestSafeWorkBranchName tables the ref-construction guard directly, so the
// accept/reject boundary is pinned independently of any poll path.
func TestSafeWorkBranchName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantRef string
		wantErr bool
	}{
		{"generated name", "wb-9c2f1a", "refs/heads/loam/wb-9c2f1a", false},
		{"dots and underscores", "wb_1.2-3", "refs/heads/loam/wb_1.2-3", false},
		{"empty", "", "", true},
		{"slash escapes the namespace", "wb/x", "", true},
		{"parent traversal", "../main", "", true},
		{"double dot inside", "wb..x", "", true},
		{"leading dash parses as a git flag", "-D", "", true},
		{"leading dot", ".hidden", "", true},
		{"whitespace", "wb 1", "", true},
		{"newline injection", "wb\nrefs/heads/main", "", true},
		{"full ref path", "refs/heads/main", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref, err := safeWorkBranchName(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, errUnsafeWorkBranchName)
				assert.Empty(t, ref, "a rejected name must yield no ref at all")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRef, ref)
		})
	}
}

// TestStorePRPollerSatisfiesPRPoller is the compile-time check that this
// adapter still matches the interface the Scheduler calls, since no
// production Scheduler is constructed anywhere in the tree yet to catch
// drift.
func TestStorePRPollerSatisfiesPRPoller(t *testing.T) {
	t.Parallel()
	var _ PRPoller = (*StorePRPoller)(nil)
}
