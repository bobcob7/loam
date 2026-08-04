package testsched

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newStateMock returns a syncStateReporterMock with every method wired to
// a no-op default, so a test that only cares about one method does not
// have to stub the other two just to avoid moq's nil-func panic.
func newStateMock() *syncStateReporterMock {
	return &syncStateReporterMock{
		ReportSyncingFunc: func(ctx context.Context, repo mirrorsync.RepoID) error { return nil },
		ReportIdleFunc:    func(ctx context.Context, repo mirrorsync.RepoID, enqueuedIngest bool) error { return nil },
		ReportErrorFunc: func(ctx context.Context, repo mirrorsync.RepoID, err error, enqueuedIngest bool) error {
			return nil
		},
	}
}

// newTestScheduler builds a real mirrorsync.Scheduler over lister and
// state, with noop stand-ins for the five collaborators this package
// neither consumes nor wraps (see sync_realscheduler_test.go for why they
// are hand-written rather than moq-generated). The tick channel is
// unused: every test in this file drives the scheduler via
// SyncHarness.Tick directly and never calls scheduler.Run.
func newTestScheduler(lister mirrorsync.RepoLister, state mirrorsync.SyncStateReporter) *mirrorsync.Scheduler {
	unusedTicks := make(chan time.Time)
	return mirrorsync.New(testLogger(), unusedTicks, lister, noopFetcher{},
		noopAdvanceDetector{}, noopMergeabilityChecker{}, noopIngestEnqueuer{}, noopPRPoller{}, noopDriftReconciler{}, state)
}

func TestSyncHarness_TickReturnsNoReposWhenNoneEnrolled(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) { return nil, nil }}
	h := NewSyncHarness(newTestScheduler(lister, newStateMock()))
	repos := h.TickT(t.Context(), t)
	assert.Empty(t, repos)
}

// TestSyncHarness_TickRunsCycleToCompletionForEveryEnrolledRepo proves the
// core contract in the bead's DESIGN: by the time Tick returns, every
// enrolled repo has reported a terminal outcome -- checked synchronously,
// right after Tick returns, with no wait of its own needed in the test.
// If Tick only started cycles without waiting for them, ReportIdleCalls
// below would be flaky (sometimes short) rather than reliably complete.
func TestSyncHarness_TickRunsCycleToCompletionForEveryEnrolledRepo(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return []mirrorsync.RepoID{"repoA", "repoB"}, nil
	}}
	state := newStateMock()
	h := NewSyncHarness(newTestScheduler(lister, state))
	repos := h.TickT(t.Context(), t)
	assert.ElementsMatch(t, []mirrorsync.RepoID{"repoA", "repoB"}, repos)
	require.Len(t, state.ReportIdleCalls(), 2, "Tick must not return before both repos have reported idle")
}

// TestSyncHarness_TickUnblocksEvenWhenACycleErrors mirrors mirrorsync's
// own TestScheduler_ReportsEnqueuedIngestWhenLaterStepFails class of
// case at this package's boundary: Tick must still return promptly (and
// with the repo in its result) when a repo's cycle ends in ReportError
// rather than ReportIdle.
func TestSyncHarness_TickUnblocksEvenWhenACycleErrors(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return []mirrorsync.RepoID{"repoA"}, nil
	}}
	state := newStateMock()
	wantErr := errors.New("merge-tree exploded")
	scheduler := mirrorsync.New(testLogger(), make(chan time.Time), lister, noopFetcher{},
		noopAdvanceDetector{}, failingMergeabilityChecker{err: wantErr}, noopIngestEnqueuer{}, noopPRPoller{}, noopDriftReconciler{}, state)
	h := NewSyncHarness(scheduler)
	repos := h.TickT(t.Context(), t)
	assert.Equal(t, []mirrorsync.RepoID{"repoA"}, repos)
	require.Len(t, state.ReportErrorCalls(), 1)
	assert.ErrorIs(t, state.ReportErrorCalls()[0].Err, wantErr)
}

// TestSyncHarness_Tick_PropagatesListReposError drives loam-hhh's failing-
// lister path end to end through this package's own seam: SyncHarness.Tick
// must surface mirrorsync.Scheduler.Tick's ListRepos failure rather than
// swallowing it the way the pre-fix Tick (returning only a slice) had to.
func TestSyncHarness_Tick_PropagatesListReposError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("db unreachable")
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return nil, wantErr
	}}
	h := NewSyncHarness(newTestScheduler(lister, newStateMock()))
	repos, err := h.Tick(t.Context())
	require.ErrorIs(t, err, wantErr)
	assert.Empty(t, repos)
}

// TestSyncHarness_TickT_FailsTBOnListReposError proves TickT -- restored in
// loam-hhh now that Tick has an error to carry -- fails its testing.TB
// argument on a ListRepos failure instead of silently returning an empty
// slice a test could mistake for "no repo enrolled".
func TestSyncHarness_TickT_FailsTBOnListReposError(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return nil, errors.New("db unreachable")
	}}
	h := NewSyncHarness(newTestScheduler(lister, newStateMock()))
	stub := &fatalRecordingTB{TB: t}
	repos := h.TickT(t.Context(), stub)
	assert.True(t, stub.fataled, "TickT must fail tb when Tick returns a ListRepos error")
	assert.Empty(t, repos)
}

// This package deliberately does NOT re-test "a repo already mid-cycle
// is skipped by a concurrent tick" -- that property belongs one layer
// down, at internal/mirrorsync (scheduler_test.go's
// TestScheduler_RepoDoesNotStartSecondCycleWhileFirstInFlight), which
// asserts on tick's synchronous return value from a single goroutine and
// cannot race. An earlier version of this test drove two concurrent
// SyncHarness.Tick calls and synchronized on ListRepos call counts,
// which does not actually prove tryStart has evaluated (that happens
// after ListRepos returns, inside tick's per-repo loop) -- it failed
// under `-race -count=2000` and flaked under plain `-count=5000`. Fixing
// it would mean re-deriving mirrorsync's own internal ordering from
// outside the package; docs/testing-spec.md's "one behavior, one home"
// puts this one at the lowest layer that can observe it honestly, which
// is not here. Concurrent Tick calls are also disallowed by SyncHarness's
// own contract (see Tick's doc comment), so this package has no
// supported way to construct the scenario safely in the first place.
