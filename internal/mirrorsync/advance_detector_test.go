package mirrorsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// newDetectorFixture wires a StoreAdvanceDetector over moq mocks configured
// with every method the detector might call, so an unconfigured method
// panicking (rather than a real assertion failing) can never be mistaken
// for a passing test.
func newDetectorFixture(t *testing.T, repoID uuid.UUID, listedTargets []string, workBranches []workbranchstore.WorkBranch) *StoreAdvanceDetector {
	t.Helper()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			assert.Equal(t, "acme/widgets", name)
			return reposstore.Repo{ID: repoID}, nil
		},
	}
	tbs := make([]reposstore.TargetBranch, len(listedTargets))
	for i, branch := range listedTargets {
		tbs[i] = reposstore.TargetBranch{RepoID: repoID, Branch: branch}
	}
	targets := &targetBranchListerMock{
		ListTargetBranchesFunc: func(_ context.Context, gotRepoID uuid.UUID) ([]reposstore.TargetBranch, error) {
			assert.Equal(t, repoID, gotRepoID)
			return tbs, nil
		},
	}
	served := false
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(_ context.Context, filter workbranchstore.ListFilter, _ int32, _ *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			require.NotNil(t, filter.RepoID)
			assert.Equal(t, repoID, *filter.RepoID)
			if served {
				return nil, nil
			}
			served = true
			return workBranches, nil
		},
	}
	return NewStoreAdvanceDetector(repos, targets, branches)
}

func TestDetectAdvancesReportsChangedListedTarget(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	detector := newDetectorFixture(t, repoID, []string{"main"}, nil)
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "bbb"}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	assert.Equal(t, []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "bbb"}}, advanced)
}

// TestDetectAdvancesReportsRecordedTargetOfOpenWorkBranchNotListed proves
// the bead's title clause: an advance on a branch that is ONLY the
// recorded target of an open work branch -- never enrolled in
// repo_target_branches -- still surfaces. Mutation killed: an
// implementation that unions only set (a) (listed targets) and ignores
// set (b) (recorded work-branch targets) returns an empty advanced slice
// here instead of the one Advance asserted.
func TestDetectAdvancesReportsRecordedTargetOfOpenWorkBranchNotListed(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	openBranch := workbranchstore.WorkBranch{Name: "wb-1", Target: "feature-x", State: workbranchstore.StateDraft}
	detector := newDetectorFixture(t, repoID, nil, []workbranchstore.WorkBranch{openBranch})
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/feature-x", OldSHA: "aaa", NewSHA: "bbb"}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	assert.Equal(t, []Advance{{Branch: "feature-x", OldSHA: "aaa", NewSHA: "bbb"}}, advanced)
}

// TestDetectAdvancesIgnoresRecordedTargetOfTerminalWorkBranch proves the
// "non-terminal" qualifier is enforced: a complete/closed work branch's
// recorded target does not pull an otherwise-irrelevant ref into scope.
func TestDetectAdvancesIgnoresRecordedTargetOfTerminalWorkBranch(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	closedBranch := workbranchstore.WorkBranch{Name: "wb-1", Target: "feature-x", State: workbranchstore.StateClosed}
	detector := newDetectorFixture(t, repoID, nil, []workbranchstore.WorkBranch{closedBranch})
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/feature-x", OldSHA: "aaa", NewSHA: "bbb"}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	assert.Empty(t, advanced)
}

// TestDetectAdvancesTreatsForcePushedNonFastForwardAsAnAdvance proves a
// force-pushed (upstream-wins, loam-giq.2) rewrite of history is reported
// exactly like a fast-forward: the two SHAs given here are deliberately
// unrelated (not one an ancestor of the other) since StoreAdvanceDetector
// must never attempt an ancestry check to decide "advance" -- only that
// fetched reported the ref changed at all. Mutation killed: any ancestry
// or fast-forward-only filter added to DetectAdvances drops this Advance.
func TestDetectAdvancesTreatsForcePushedNonFastForwardAsAnAdvance(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	detector := newDetectorFixture(t, repoID, []string{"main"}, nil)
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/main", OldSHA: "8c9a25ef69308c445dc914c7485e411a7312a167", NewSHA: "fbd521e0d1153d8ad2effa0474b56c99d4cbaba6"}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	require.Len(t, advanced, 1)
	assert.Equal(t, "fbd521e0d1153d8ad2effa0474b56c99d4cbaba6", advanced[0].NewSHA, "a force-pushed rewrite is still an advance even though old is not new's ancestor")
}

// TestDetectAdvancesSkipsUnionBranchAbsentFromFetchResult is the
// idempotency proof: a branch in scope (union of sets (a) and (b)) that
// simply did not change this tick never appears in fetched.Refs at all
// (parsePorcelainFetch only emits a line per changed ref), and
// DetectAdvances must not manufacture an Advance for it regardless.
// Mutation killed: an implementation that reports every union branch
// unconditionally (ignoring whether fetched actually mentions it) would
// enqueue on every tick even when nothing changed.
func TestDetectAdvancesSkipsUnionBranchAbsentFromFetchResult(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	detector := newDetectorFixture(t, repoID, []string{"main"}, nil)
	fetched := FetchResult{} // nothing changed this fetch
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	assert.Empty(t, advanced, "an unchanged ref must not be reported as an advance")
}

// TestDetectAdvancesReportsNewRefAsAdvanceWithEmptyOldSHA proves the
// new-ref case (first enrollment, or a listed/recorded target created
// upstream after enrollment): OldSHA empty, NewSHA populated. Mutation
// killed: an implementation that treats an empty OldSHA as "not a real
// advance" (e.g. skipping when ref.OldSHA == "") drops this Advance.
func TestDetectAdvancesReportsNewRefAsAdvanceWithEmptyOldSHA(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	detector := newDetectorFixture(t, repoID, []string{"main"}, nil)
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/main", OldSHA: "", NewSHA: "4f19bdffac2774e6d125bd189f030c166529b260"}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	require.Len(t, advanced, 1)
	assert.Equal(t, Advance{Branch: "main", OldSHA: "", NewSHA: "4f19bdffac2774e6d125bd189f030c166529b260"}, advanced[0])
}

// TestDetectAdvancesErrorsWhenListedTargetBranchDeletedUpstream proves the
// whole-repo failure path: a listed target branch (set (a)) pruned this
// fetch (NewSHA empty) must abort with an error naming the branch, not a
// silent success. Mutation killed: an implementation that drops the
// deleted-ref case (treats NewSHA=="" the same as any other change, or
// skips it entirely) returns (advanced, nil) here instead of an error, and
// the scheduler would go on to run mergeability/ingest/PR-poll and report
// idle for a repo that just lost a listed target.
func TestDetectAdvancesErrorsWhenListedTargetBranchDeletedUpstream(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	detector := newDetectorFixture(t, repoID, []string{"main"}, nil)
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: ""}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.Error(t, err)
	assert.ErrorIs(t, err, errMissingTargetBranch)
	assert.Contains(t, err.Error(), "main")
	assert.Nil(t, advanced)
}

// TestDetectAdvancesReportsDeletedRecordedTargetOfOpenWorkBranchAsAdvance
// covers the DE-LISTED-but-still-recorded case set (b) exists for: a
// branch that is ONLY an open work branch's recorded target (never listed
// in repo_target_branches) is deleted upstream. This must NOT abort the
// whole repo the way a listed target's deletion does -- only a listed
// target's disappearance is the whole-repo failure DESIGN describes --
// but it also must not be silently dropped, since the mergeability
// checker (giq.5) still needs to know the branch's recorded target
// vanished so it can flag the work branches targeting it.
func TestDetectAdvancesReportsDeletedRecordedTargetOfOpenWorkBranchAsAdvance(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	openBranch := workbranchstore.WorkBranch{Name: "wb-1", Target: "feature-x", State: workbranchstore.StateReviewable}
	detector := newDetectorFixture(t, repoID, nil, []workbranchstore.WorkBranch{openBranch})
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/feature-x", OldSHA: "aaa", NewSHA: ""}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err, "a de-listed, work-branch-only target's deletion is not the whole-repo failure a LISTED target's deletion is")
	assert.Equal(t, []Advance{{Branch: "feature-x", OldSHA: "aaa", NewSHA: ""}}, advanced)
}

func TestDetectAdvancesUnionsSetsADeduplicatingSameBranchInBoth(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	openBranch := workbranchstore.WorkBranch{Name: "wb-1", Target: "main", State: workbranchstore.StateDraft}
	detector := newDetectorFixture(t, repoID, []string{"main"}, []workbranchstore.WorkBranch{openBranch})
	fetched := FetchResult{Refs: []RefUpdate{{Ref: "refs/heads/main", OldSHA: "aaa", NewSHA: "bbb"}}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	assert.Equal(t, []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "bbb"}}, advanced, "a branch in both sets must be reported once, not duplicated")
}

func TestDetectAdvancesPropagatesGetRepoByNameError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: repo not found")
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, wantErr
		},
	}
	targets := &targetBranchListerMock{
		ListTargetBranchesFunc: func(context.Context, uuid.UUID) ([]reposstore.TargetBranch, error) {
			t.Fatal("ListTargetBranches must not be called when the repo lookup already failed")
			return nil, nil
		},
	}
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(context.Context, workbranchstore.ListFilter, int32, *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			t.Fatal("ListByCursor must not be called when the repo lookup already failed")
			return nil, nil
		},
	}
	detector := NewStoreAdvanceDetector(repos, targets, branches)
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), FetchResult{})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Nil(t, advanced)
}

func TestDetectAdvancesPropagatesListTargetBranchesError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: list target branches failed")
	repoID := uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID}, nil
		},
	}
	targets := &targetBranchListerMock{
		ListTargetBranchesFunc: func(context.Context, uuid.UUID) ([]reposstore.TargetBranch, error) {
			return nil, wantErr
		},
	}
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(context.Context, workbranchstore.ListFilter, int32, *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			t.Fatal("ListByCursor must not be called when ListTargetBranches already failed")
			return nil, nil
		},
	}
	detector := NewStoreAdvanceDetector(repos, targets, branches)
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), FetchResult{})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Nil(t, advanced)
}

func TestDetectAdvancesPropagatesWorkBranchListError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: work branch list failed")
	repoID := uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID}, nil
		},
	}
	targets := &targetBranchListerMock{
		ListTargetBranchesFunc: func(context.Context, uuid.UUID) ([]reposstore.TargetBranch, error) {
			return nil, nil
		},
	}
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(context.Context, workbranchstore.ListFilter, int32, *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			return nil, wantErr
		},
	}
	detector := NewStoreAdvanceDetector(repos, targets, branches)
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), FetchResult{})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Nil(t, advanced)
}

func TestDetectAdvancesPagesThroughAllWorkBranches(t *testing.T) {
	t.Parallel()
	repoID := uuid.New()
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID}, nil
		},
	}
	targets := &targetBranchListerMock{
		ListTargetBranchesFunc: func(context.Context, uuid.UUID) ([]reposstore.TargetBranch, error) {
			return nil, nil
		},
	}
	var seenCursors []*workbranchstore.Cursor
	calls := 0
	branches := &workBranchNameListerMock{
		ListByCursorFunc: func(_ context.Context, _ workbranchstore.ListFilter, _ int32, after *workbranchstore.Cursor) ([]workbranchstore.WorkBranch, error) {
			seenCursors = append(seenCursors, after)
			calls++
			switch calls {
			case 1:
				return []workbranchstore.WorkBranch{
					{ID: uuid.New(), CreatedAt: time.Unix(200, 0), Target: "a", State: workbranchstore.StateDraft},
					{ID: uuid.New(), CreatedAt: time.Unix(100, 0), Target: "b", State: workbranchstore.StateDraft},
				}, nil
			case 2:
				return []workbranchstore.WorkBranch{{ID: uuid.New(), CreatedAt: time.Unix(50, 0), Target: "c", State: workbranchstore.StateDraft}}, nil
			default:
				return nil, nil
			}
		},
	}
	detector := NewStoreAdvanceDetector(repos, targets, branches)
	fetched := FetchResult{Refs: []RefUpdate{
		{Ref: "refs/heads/a", OldSHA: "1", NewSHA: "2"},
		{Ref: "refs/heads/b", OldSHA: "1", NewSHA: "2"},
		{Ref: "refs/heads/c", OldSHA: "1", NewSHA: "2"},
	}}
	advanced, err := detector.DetectAdvances(t.Context(), RepoID("acme/widgets"), fetched)
	require.NoError(t, err)
	assert.Len(t, advanced, 3)
	require.Len(t, seenCursors, 3, "a third call, on the terminating empty page, must still happen")
	assert.Nil(t, seenCursors[0], "the first call carries no cursor")
	require.NotNil(t, seenCursors[1])
	assert.Equal(t, time.Unix(100, 0), seenCursors[1].CreatedAt, "the second call resumes from the LAST row of page one")
	require.NotNil(t, seenCursors[2])
	assert.Equal(t, time.Unix(50, 0), seenCursors[2].CreatedAt, "the third call resumes from the last row of page two")
}
