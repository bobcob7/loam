package mirrorsync

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// DEFERRED-WIP: work-branch-lifecycle.feature:73-77 "A clean target
// advance leaves the work branch untouched" ->
// TestCheckMergeability_CleanBranchIsLeftUntouched (plus
// internal/gitmergetree's TestMergeTree_CleanMergeReportsNotConflicted for
// the real-git half). work-branch-lifecycle.feature:80-85 "A conflicting
// target advance resets the work branch to draft" ->
// TestCheckMergeability_ConflictingBranchIsFlagged plus
// internal/workbranchstore's own MarkConflicted coverage, which is where
// the reviewable/reviewed -> draft demotion and the conflict='reset' stamp
// actually happen. admin-proposals.feature:71-75 "A conflicting target
// advance removes a proposal from the queue" is NOT covered here at all:
// the queue predicate lives in the proposal-listing query, not in this
// package, and the scenario's "no longer appears in the proposal queue"
// step needs an admin RPC round trip.
//
// The @wip tags on all three scenarios STAY. Nothing in this tree runs
// godog yet -- the dependency is in go.mod but there is no step-definition
// suite, no runner, and no actor drivers (docs/testing-spec.md), so
// features/README.md's "for now they are the specification of record"
// still holds. Un-@wip-ing a scenario no harness executes would assert
// coverage that does not exist, which is the same convention
// internal/ingest, internal/storesuite, and internal/mirrorsync/state
// already record for their own beads' acceptance criteria.

// mergeCall is one recorded mergeTreeRunner invocation, so tests can
// assert on WHAT was checked (which ref, against which tip, in which
// mirror) and not merely on how many checks happened.
type mergeCall struct {
	mirrorDir string
	ours      string
	theirs    string
}

// mergeVerdict is one canned answer keyed by the work-branch ref the
// checker passes as "ours": conflicted when err is nil, or a check
// failure when it is not.
type mergeVerdict struct {
	conflicted bool
	err        error
}

// checkerFixture wires a StoreMergeabilityChecker over fully configured
// moq mocks and keeps every recorded call. Every mock method the checker
// could reach is configured, so a test that means to assert "this was
// never called" fails on a real assertion over a recorded slice rather
// than on an unconfigured-mock panic -- the same discipline
// ingest_enqueuer_test.go's spies use.
type checkerFixture struct {
	checker     *StoreMergeabilityChecker
	dataDir     string
	repoID      uuid.UUID
	mergeCalls  *[]mergeCall
	markedIDs   *[]uuid.UUID
	repoLookups *int
	listOffsets *[]int32
}

// fixtureOptions are the per-test knobs newCheckerFixture takes: which
// work branches the store reports, what merge-tree answers for each work
// branch ref, and the failure to inject on each seam.
type fixtureOptions struct {
	workBranches []workbranchstore.WorkBranch
	verdicts     map[string]mergeVerdict
	pageSize     int32
	repoErr      error
	listErr      error
	markErr      error
}

const fixtureRepoName = "acme/widgets"

func newCheckerFixture(t *testing.T, opts fixtureOptions) checkerFixture {
	t.Helper()
	repoID := uuid.New()
	dataDir := t.TempDir()
	mergeCalls := new([]mergeCall)
	markedIDs := new([]uuid.UUID)
	repoLookups := new(int)
	listOffsets := new([]int32)
	repos := &repoByNameLookupMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			*repoLookups++
			assert.Equal(t, fixtureRepoName, name)
			if opts.repoErr != nil {
				return reposstore.Repo{}, opts.repoErr
			}
			return reposstore.Repo{ID: repoID, Name: name}, nil
		},
	}
	pageSize := opts.pageSize
	if pageSize <= 0 {
		pageSize = workBranchListPageSize
	}
	branches := &workBranchNameListerMock{
		ListFunc: func(_ context.Context, filter workbranchstore.ListFilter, limit, offset int32) ([]workbranchstore.WorkBranch, int64, error) {
			*listOffsets = append(*listOffsets, offset)
			require.NotNil(t, filter.RepoID)
			assert.Equal(t, repoID, *filter.RepoID)
			assert.Equal(t, int32(workBranchListPageSize), limit)
			if opts.listErr != nil {
				return nil, 0, opts.listErr
			}
			total := int64(len(opts.workBranches))
			if offset >= int32(len(opts.workBranches)) {
				return nil, total, nil
			}
			end := offset + pageSize
			if end > int32(len(opts.workBranches)) {
				end = int32(len(opts.workBranches))
			}
			return opts.workBranches[offset:end], total, nil
		},
	}
	merger := &mergeTreeRunnerMock{
		MergeTreeFunc: func(_ context.Context, mirrorDir, ours, theirs string) (bool, error) {
			*mergeCalls = append(*mergeCalls, mergeCall{mirrorDir: mirrorDir, ours: ours, theirs: theirs})
			verdict := opts.verdicts[ours]
			return verdict.conflicted, verdict.err
		},
	}
	conflicts := &workBranchConflictMarkerMock{
		MarkConflictedFunc: func(_ context.Context, id uuid.UUID) (workbranchstore.WorkBranch, error) {
			*markedIDs = append(*markedIDs, id)
			if opts.markErr != nil {
				return workbranchstore.WorkBranch{}, opts.markErr
			}
			return workbranchstore.WorkBranch{ID: id, State: workbranchstore.StateDraft, Conflict: workbranchstore.ConflictReset}, nil
		},
	}
	return checkerFixture{
		checker:     NewStoreMergeabilityChecker(dataDir, repos, branches, merger, conflicts),
		dataDir:     dataDir,
		repoID:      repoID,
		mergeCalls:  mergeCalls,
		markedIDs:   markedIDs,
		repoLookups: repoLookups,
		listOffsets: listOffsets,
	}
}

// workBranch builds one work_branches row for the fixture.
func workBranch(name, target string, state workbranchstore.State, conflict workbranchstore.Conflict) workbranchstore.WorkBranch {
	return workbranchstore.WorkBranch{ID: uuid.New(), Name: name, Target: target, State: state, Conflict: conflict}
}

// advance is the shorthand for one target-branch advance.
func advance(branch, newSHA string) Advance {
	return Advance{Branch: branch, OldSHA: "old" + newSHA, NewSHA: newSHA}
}

// TestCheckMergeability_ConflictingBranchIsFlagged is the core write path:
// docs/sync-spec.md's Mergeability Check "Conflict -> the branch is marked
// conflicted". The draft-vs-demotion split itself lives in
// MarkWorkBranchConflicted's guarded UPDATE, so what this layer owes is
// exactly one MarkConflicted call naming the right branch.
func TestCheckMergeability_ConflictingBranchIsFlagged(t *testing.T) {
	t.Parallel()
	wb := workBranch("wb-9c2f1a", "main", workbranchstore.StateReviewed, workbranchstore.ConflictNone)
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{wb},
		verdicts:     map[string]mergeVerdict{"refs/heads/loam-reserved/wb-9c2f1a": {conflicted: true}},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "newtip")}))
	assert.Equal(t, []uuid.UUID{wb.ID}, *fixture.markedIDs)
}

// TestCheckMergeability_CleanBranchIsLeftUntouched pins
// features/work-branch-lifecycle.feature's "A clean target advance leaves
// the work branch untouched": no write of any kind reaches the store.
func TestCheckMergeability_CleanBranchIsLeftUntouched(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{workBranch("wb-9c2f1a", "main", workbranchstore.StateReviewable, workbranchstore.ConflictNone)},
		verdicts:     map[string]mergeVerdict{"refs/heads/loam-reserved/wb-9c2f1a": {conflicted: false}},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "newtip")}))
	assert.Empty(t, *fixture.markedIDs, "a cleanly merging branch must have nothing written about it")
	assert.Len(t, *fixture.mergeCalls, 1, "it must still have been checked")
}

// TestCheckMergeability_CleanRecheckOfAFlaggedBranchDoesNotClearTheFlag is
// the deliberate NON-transition this bead is often expected to have and
// must not: docs/sync-spec.md's clean case is "nothing happens", and a
// flagged branch recovers by PUSH via catch-up detection (loam-giq.6),
// which is also what re-opens a review round. Clearing here would restore
// a demoted branch to reviewable with no agent push, no fresh round, and
// its stale verdicts abruptly counting again toward the approval bar. The
// checker holds no clearing seam at all, so the assertion is that the
// already-flagged, already-demoted branch is written to in no way
// whatsoever.
func TestCheckMergeability_CleanRecheckOfAFlaggedBranchDoesNotClearTheFlag(t *testing.T) {
	t.Parallel()
	flagged := workBranch("wb-9c2f1a", "main", workbranchstore.StateDraft, workbranchstore.ConflictReset)
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{flagged},
		verdicts:     map[string]mergeVerdict{"refs/heads/loam-reserved/wb-9c2f1a": {conflicted: false}},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "newtip")}))
	assert.Empty(t, *fixture.markedIDs)
	assert.Len(t, *fixture.mergeCalls, 1, "the branch is still re-checked; only the write is withheld")
}

// TestCheckMergeability_StillConflictingBranchIsFlaggedAgain pins the
// level-triggered contract MarkWorkBranchConflicted's own doc comment
// describes ("the same branch can be found still-conflicting on several
// advances in a row"): the checker does not suppress the call just because
// the row already carries a conflict, and preserving 'reset' over
// 'flagged' is the SQL's job, not a decision this layer may take by
// skipping the write.
func TestCheckMergeability_StillConflictingBranchIsFlaggedAgain(t *testing.T) {
	t.Parallel()
	flagged := workBranch("wb-9c2f1a", "main", workbranchstore.StateDraft, workbranchstore.ConflictReset)
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{flagged},
		verdicts:     map[string]mergeVerdict{"refs/heads/loam-reserved/wb-9c2f1a": {conflicted: true}},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "newtip")}))
	assert.Equal(t, []uuid.UUID{flagged.ID}, *fixture.markedIDs)
}

// TestCheckMergeability_ChecksEachBranchAgainstItsOwnTargetTip guards the
// wrong-SHA class of bug directly: with two targets advancing in the same
// tick, each work branch must be merge-tested against the tip of the
// branch IT records as its target, in the mirror derived from the repo
// name.
func TestCheckMergeability_ChecksEachBranchAgainstItsOwnTargetTip(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{
			workBranch("wb-main", "main", workbranchstore.StateDraft, workbranchstore.ConflictNone),
			workBranch("wb-rel", "release-1", workbranchstore.StateReviewable, workbranchstore.ConflictNone),
		},
	})
	advanced := []Advance{advance("main", "mainTip"), advance("release-1", "relTip")}
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), advanced))
	mirrorDir := mirrorpath.Dir(fixture.dataDir, fixtureRepoName)
	assert.Equal(t, []mergeCall{
		{mirrorDir: mirrorDir, ours: "refs/heads/loam-reserved/wb-main", theirs: "mainTip"},
		{mirrorDir: mirrorDir, ours: "refs/heads/loam-reserved/wb-rel", theirs: "relTip"},
	}, *fixture.mergeCalls)
}

// TestCheckMergeability_BranchesTargetingAnUnadvancedBranchAreNotChecked
// pins the filter loam-giq.4's wider union makes necessary: the detector
// reports every listed target plus every open work branch's recorded
// target, but only the ones that actually MOVED this tick are handed here,
// and a work branch whose own target is not among them is not merge-tested
// at all -- re-testing it would burn a subprocess per branch per tick for
// an answer that cannot have changed.
func TestCheckMergeability_BranchesTargetingAnUnadvancedBranchAreNotChecked(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{
			workBranch("wb-main", "main", workbranchstore.StateDraft, workbranchstore.ConflictNone),
			workBranch("wb-quiet", "untouched-branch", workbranchstore.StateDraft, workbranchstore.ConflictNone),
		},
		verdicts: map[string]mergeVerdict{"refs/heads/loam-reserved/wb-quiet": {conflicted: true}},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")}))
	require.Len(t, *fixture.mergeCalls, 1)
	assert.Equal(t, "refs/heads/loam-reserved/wb-main", (*fixture.mergeCalls)[0].ours)
	assert.Empty(t, *fixture.markedIDs, "the untouched target's branch must not be flagged despite its canned conflicting verdict")
}

// TestCheckMergeability_TerminalBranchesAreNotChecked pins that complete
// and closed rows are excluded (docs/sync-spec.md: "each OPEN
// (non-terminal) work branch"). This is not cosmetic: MarkConflicted's
// guarded UPDATE rejects a terminal branch outright, so reaching it would
// turn a merged proposal into a whole-repo sync error.
func TestCheckMergeability_TerminalBranchesAreNotChecked(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{
			workBranch("wb-done", "main", workbranchstore.StateComplete, workbranchstore.ConflictNone),
			workBranch("wb-gone", "main", workbranchstore.StateClosed, workbranchstore.ConflictNone),
			workBranch("wb-open", "main", workbranchstore.StateDraft, workbranchstore.ConflictNone),
		},
		verdicts: map[string]mergeVerdict{
			"refs/heads/loam-reserved/wb-done": {conflicted: true},
			"refs/heads/loam-reserved/wb-gone": {conflicted: true},
			"refs/heads/loam-reserved/wb-open": {conflicted: true},
		},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")}))
	require.Len(t, *fixture.mergeCalls, 1)
	assert.Equal(t, "refs/heads/loam-reserved/wb-open", (*fixture.mergeCalls)[0].ours)
}

// TestCheckMergeability_NoAdvancesDoesNothingAtAll pins the cheap common
// tick: with nothing advanced there is no repo lookup, no listing, and no
// subprocess.
func TestCheckMergeability_NoAdvancesDoesNothingAtAll(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{workBranch("wb-9c2f1a", "main", workbranchstore.StateDraft, workbranchstore.ConflictNone)},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), nil))
	assert.Zero(t, *fixture.repoLookups, "an empty advance set must not cost a database round trip")
	assert.Empty(t, *fixture.mergeCalls)
}

// TestCheckMergeability_DeletedTargetRefIsSkipped covers the advance shape
// giq.4 emits for a pruned ref (empty NewSHA, per Advance's doc comment):
// there is no tip to merge against, so it is dropped before anything else
// happens -- passing the empty string to git would be an unresolvable
// "ref" and would fail the whole cycle.
func TestCheckMergeability_DeletedTargetRefIsSkipped(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{workBranch("wb-9c2f1a", "gone", workbranchstore.StateDraft, workbranchstore.ConflictNone)},
		verdicts:     map[string]mergeVerdict{"refs/heads/loam-reserved/wb-9c2f1a": {conflicted: true}},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{{Branch: "gone", OldSHA: "oldtip", NewSHA: ""}}))
	assert.Zero(t, *fixture.repoLookups)
	assert.Empty(t, *fixture.mergeCalls)
	assert.Empty(t, *fixture.markedIDs)
}

// TestCheckMergeability_DeletedTargetDoesNotSuppressARealOne pins that
// dropping the empty-NewSHA entry drops only that entry: a real advance
// arriving in the same slice is still checked.
func TestCheckMergeability_DeletedTargetDoesNotSuppressARealOne(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{workBranch("wb-9c2f1a", "main", workbranchstore.StateDraft, workbranchstore.ConflictNone)},
	})
	advanced := []Advance{{Branch: "gone", NewSHA: ""}, advance("main", "mainTip")}
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), advanced))
	require.Len(t, *fixture.mergeCalls, 1)
	assert.Equal(t, "mainTip", (*fixture.mergeCalls)[0].theirs)
}

// TestCheckMergeability_MergeCheckFailureAbortsWithoutFlaggingAnything is
// the "a failed check is not a conflict" invariant at this layer. git
// reports an unresolvable ref with the same exit status as a conflict
// (internal/gitmergetree's package doc comment), so the seam's error must
// abort the whole call: no branch is flagged, including branches later in
// the list that were never reached.
func TestCheckMergeability_MergeCheckFailureAbortsWithoutFlaggingAnything(t *testing.T) {
	t.Parallel()
	boom := errors.New("merge-tree check did not complete")
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{
			workBranch("wb-broken", "main", workbranchstore.StateReviewed, workbranchstore.ConflictNone),
			workBranch("wb-later", "main", workbranchstore.StateReviewed, workbranchstore.ConflictNone),
		},
		verdicts: map[string]mergeVerdict{
			"refs/heads/loam-reserved/wb-broken": {err: boom},
			"refs/heads/loam-reserved/wb-later":  {conflicted: true},
		},
	})
	err := fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")})
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "wb-broken")
	assert.Contains(t, err.Error(), "mainTip")
	assert.Empty(t, *fixture.markedIDs, "a check that failed must never demote a work branch")
	assert.Len(t, *fixture.mergeCalls, 1, "the cycle aborts at the first failure rather than pressing on")
}

// TestCheckMergeability_MarkConflictedFailurePropagates pins that a store
// write failure surfaces as an error naming the branch, so the scheduler
// records repos.sync_state = error and retries the whole cycle next tick
// rather than silently losing a conflict verdict.
func TestCheckMergeability_MarkConflictedFailurePropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("illegal transition")
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{workBranch("wb-9c2f1a", "main", workbranchstore.StateReviewed, workbranchstore.ConflictNone)},
		verdicts:     map[string]mergeVerdict{"refs/heads/loam-reserved/wb-9c2f1a": {conflicted: true}},
		markErr:      boom,
	})
	err := fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")})
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "wb-9c2f1a")
}

// TestCheckMergeability_RepoLookupFailurePropagates covers the first seam.
func TestCheckMergeability_RepoLookupFailurePropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("no such repo")
	fixture := newCheckerFixture(t, fixtureOptions{repoErr: boom})
	err := fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")})
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), fixtureRepoName)
	assert.Empty(t, *fixture.mergeCalls)
}

// TestCheckMergeability_WorkBranchListingFailurePropagates covers the
// second seam.
func TestCheckMergeability_WorkBranchListingFailurePropagates(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection reset")
	fixture := newCheckerFixture(t, fixtureOptions{listErr: boom})
	err := fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")})
	require.ErrorIs(t, err, boom)
	assert.Empty(t, *fixture.mergeCalls)
}

// TestCheckMergeability_PagesThroughEveryWorkBranch pins that a repo with
// more open work branches than one page carries still gets every one of
// them checked -- a repo just over the page boundary silently skipping its
// tail would leave conflicts undetected indefinitely, with nothing failing
// to signal it.
func TestCheckMergeability_PagesThroughEveryWorkBranch(t *testing.T) {
	t.Parallel()
	const total = 7
	branches := make([]workbranchstore.WorkBranch, 0, total)
	verdicts := make(map[string]mergeVerdict, total)
	for i := range total {
		name := fmt.Sprintf("wb-%02d", i)
		branches = append(branches, workBranch(name, "main", workbranchstore.StateDraft, workbranchstore.ConflictNone))
		verdicts["refs/heads/loam-reserved/"+name] = mergeVerdict{conflicted: true}
	}
	fixture := newCheckerFixture(t, fixtureOptions{workBranches: branches, verdicts: verdicts, pageSize: 3})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")}))
	assert.Len(t, *fixture.mergeCalls, total, "every open work branch across every page must be checked")
	assert.Len(t, *fixture.markedIDs, total)
	assert.Equal(t, []int32{0, 3, 6}, *fixture.listOffsets, "paging must advance by the page actually returned")
}

// TestCheckMergeability_UsesTheRepoNameDerivedMirrorPath pins that the
// mirror handed to git is internal/mirrorpath's single source of the
// "<dataDir>/mirrors/<group>/<repo_name>.git" convention
// (docs/server-spec.md's LOAM_DATA_DIR row) rather than a second spelling
// of the same join.
func TestCheckMergeability_UsesTheRepoNameDerivedMirrorPath(t *testing.T) {
	t.Parallel()
	fixture := newCheckerFixture(t, fixtureOptions{
		workBranches: []workbranchstore.WorkBranch{workBranch("wb-9c2f1a", "main", workbranchstore.StateDraft, workbranchstore.ConflictNone)},
	})
	require.NoError(t, fixture.checker.CheckMergeability(t.Context(), RepoID(fixtureRepoName), []Advance{advance("main", "mainTip")}))
	require.Len(t, *fixture.mergeCalls, 1)
	assert.Equal(t, mirrorpath.Dir(fixture.dataDir, fixtureRepoName), (*fixture.mergeCalls)[0].mirrorDir)
}
