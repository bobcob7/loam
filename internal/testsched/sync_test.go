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

func TestSyncHarness_TickReturnsNoReposWhenNoneEnrolled(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) { return nil, nil }}
	state := newStateMock()
	h := NewSyncHarness(lister, state)
	go driveTickOnce(t.Context(), h.Ticks(), h.RepoLister(), h.SyncStateReporter(), nil)
	repos, err := h.Tick(t.Context())
	require.NoError(t, err)
	assert.Empty(t, repos)
}

// TestSyncHarness_TickWaitsForAllStartedReposEvenWithArtificialDelay is
// this package's mutation-test proof (docs/testing-spec.md, wave-4
// brief): it drives an outcome function for "repoSlow" that blocks on an
// explicit gate, then proves -- via a non-blocking select, not a sleep or
// timeout guess -- that Tick has NOT returned while the gate is closed,
// and only returns, with the correct repo set, once the gate opens. If
// Tick raced instead of waiting, the non-blocking select below would
// observe a premature result.
func TestSyncHarness_TickWaitsForAllStartedReposEvenWithArtificialDelay(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	entered := make(chan struct{})
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return []mirrorsync.RepoID{"repoFast", "repoSlow"}, nil
	}}
	state := newStateMock()
	h := NewSyncHarness(lister, state)
	outcome := func(repo mirrorsync.RepoID) error {
		if repo == "repoSlow" {
			close(entered)
			<-gate
		}
		return nil
	}
	go driveTickOnce(t.Context(), h.Ticks(), h.RepoLister(), h.SyncStateReporter(), outcome)
	tickDone := make(chan []mirrorsync.RepoID, 1)
	tickErr := make(chan error, 1)
	go func() {
		repos, err := h.Tick(t.Context())
		tickErr <- err
		tickDone <- repos
	}()
	<-entered
	select {
	case <-tickDone:
		t.Fatal("Tick returned before repoSlow's outcome was released -- the wait is not a real happens-before")
	default:
	}
	close(gate)
	var repos []mirrorsync.RepoID
	select {
	case repos = <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick did not return after the gated outcome was released")
	}
	require.NoError(t, <-tickErr)
	assert.ElementsMatch(t, []mirrorsync.RepoID{"repoFast", "repoSlow"}, repos)
}

func TestSyncHarness_TickUnblocksOnErrorOutcomeToo(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return []mirrorsync.RepoID{"repoA"}, nil
	}}
	state := newStateMock()
	h := NewSyncHarness(lister, state)
	wantErr := errors.New("merge-tree exploded")
	go driveTickOnce(t.Context(), h.Ticks(), h.RepoLister(), h.SyncStateReporter(), func(repo mirrorsync.RepoID) error { return wantErr })
	repos, err := h.Tick(t.Context())
	require.NoError(t, err, "Tick itself does not fail on a cycle error -- ReportError still unblocks it")
	assert.Equal(t, []mirrorsync.RepoID{"repoA"}, repos)
	require.Len(t, state.ReportErrorCalls(), 1)
	assert.ErrorIs(t, state.ReportErrorCalls()[0].Err, wantErr)
}

func TestSyncHarness_TickPropagatesCtxCancellationBeforeSchedulerConsumes(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{}
	state := newStateMock()
	h := NewSyncHarness(lister, state)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := h.Tick(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSyncHarness_TickSuccessiveTicksDriveIndependentCycles(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return []mirrorsync.RepoID{"repoA"}, nil
	}}
	state := newStateMock()
	h := NewSyncHarness(lister, state)
	go driveTickOnce(t.Context(), h.Ticks(), h.RepoLister(), h.SyncStateReporter(), nil)
	first, err := h.Tick(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []mirrorsync.RepoID{"repoA"}, first)
	go driveTickOnce(t.Context(), h.Ticks(), h.RepoLister(), h.SyncStateReporter(), nil)
	second, err := h.Tick(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []mirrorsync.RepoID{"repoA"}, second)
	assert.Len(t, state.ReportIdleCalls(), 2, "each tick must independently observe its own repoA cycle")
}

// driveTickOnce simulates exactly the call pattern mirrorsync.Scheduler.Run
// drives on one tick (see internal/mirrorsync/scheduler.go: tick lists
// repos synchronously, then spawns one goroutine per repo which reports
// syncing, runs its steps, then reports a terminal outcome) without
// depending on mirrorsync itself, so these tests isolate SyncHarness's
// own synchronization logic from the real scheduler's orchestration
// (which is internal/mirrorsync's own test suite's job). outcome may be
// nil, meaning every repo succeeds immediately.
func driveTickOnce(ctx context.Context, ticks <-chan time.Time, lister mirrorsync.RepoLister, reporter mirrorsync.SyncStateReporter, outcome func(repo mirrorsync.RepoID) error) {
	<-ticks
	repos, err := lister.ListRepos(ctx)
	if err != nil {
		return
	}
	for _, repo := range repos {
		go func(repo mirrorsync.RepoID) {
			_ = reporter.ReportSyncing(ctx, repo)
			var cycleErr error
			if outcome != nil {
				cycleErr = outcome(repo)
			}
			if cycleErr != nil {
				_ = reporter.ReportError(ctx, repo, cycleErr, false)
				return
			}
			_ = reporter.ReportIdle(ctx, repo, false)
		}(repo)
	}
}
