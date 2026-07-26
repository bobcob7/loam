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
		noopAdvanceDetector{}, noopMergeabilityChecker{}, noopIngestEnqueuer{}, noopPRPoller{}, state)
}

func TestSyncHarness_TickReturnsNoReposWhenNoneEnrolled(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) { return nil, nil }}
	h := NewSyncHarness(newTestScheduler(lister, newStateMock()))
	repos := h.Tick(t.Context())
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
	repos := h.Tick(t.Context())
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
		noopAdvanceDetector{}, failingMergeabilityChecker{err: wantErr}, noopIngestEnqueuer{}, noopPRPoller{}, state)
	h := NewSyncHarness(scheduler)
	repos := h.Tick(t.Context())
	assert.Equal(t, []mirrorsync.RepoID{"repoA"}, repos)
	require.Len(t, state.ReportErrorCalls(), 1)
	assert.ErrorIs(t, state.ReportErrorCalls()[0].Err, wantErr)
}

// TestSyncHarness_TickSkipsRepoAlreadyMidCycle proves the per-repo guard
// still holds through the exported Tick: a second Tick call, issued while
// the first repo's cycle is still blocked inside Fetch, must not start a
// second cycle for that repo -- its own returned slice must be empty --
// even though both calls only return once the blocked cycle finishes
// (Tick's happens-before is scheduler-wide, not per-call). Both
// synchronization points (waiting for the second call's own ListRepos to
// have run before releasing the gate) are channel receives, never a
// sleep.
func TestSyncHarness_TickSkipsRepoAlreadyMidCycle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	listCalled := make(chan struct{}, 2)
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		listCalled <- struct{}{}
		return []mirrorsync.RepoID{"repoA"}, nil
	}}
	fetcher := &blockingFetcher{entered: make(chan struct{}), release: make(chan struct{})}
	scheduler := mirrorsync.New(testLogger(), make(chan time.Time), lister, fetcher,
		noopAdvanceDetector{}, noopMergeabilityChecker{}, noopIngestEnqueuer{}, noopPRPoller{}, newStateMock())
	h := NewSyncHarness(scheduler)
	firstDone := make(chan []mirrorsync.RepoID, 1)
	go func() { firstDone <- h.Tick(ctx) }()
	<-listCalled      // first Tick's ListRepos has run
	<-fetcher.entered // and its cycle has reached (and is blocked in) Fetch, so tryStart has already claimed repoA
	secondDone := make(chan []mirrorsync.RepoID, 1)
	go func() { secondDone <- h.Tick(ctx) }()
	<-listCalled // second Tick's ListRepos has also run -- its tryStart has already evaluated (and must have skipped) repoA
	close(fetcher.release)
	first := <-firstDone
	second := <-secondDone
	assert.Equal(t, []mirrorsync.RepoID{"repoA"}, first, "the first call started repoA's cycle")
	assert.Empty(t, second, "the second call must skip repoA -- its cycle was still in flight when tryStart ran")
}
