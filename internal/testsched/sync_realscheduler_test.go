package testsched

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file proves SyncHarness against the real internal/mirrorsync.Scheduler
// rather than a hand-simulated stand-in, so the happens-before it claims is
// checked against production code, not this package's own mental model of
// it. mirrorsync.New requires five collaborator interfaces (Fetcher,
// AdvanceDetector, MergeabilityChecker, IngestEnqueuer, PRPoller) that
// SyncHarness itself neither consumes nor wraps -- they exist only because
// Go needs a concrete value to construct someone else's type. moq mocks
// are this repo's convention for interfaces a package's own logic depends
// on and a test sets expectations against; these five are neither (their
// orchestration is internal/mirrorsync's own test suite's job, see
// scheduler_test.go there), and moq cannot generate them without an
// explicit -pkg flag this repo's go:generate convention forbids (source
// and destination packages differ). So they are trivial, deliberately
// inert stand-ins: noopFetcher et al. return zero values and nothing else,
// except blockingFetcher and failingMergeabilityChecker, which exist
// solely to gate or fail a cycle deterministically for a specific test.

type noopFetcher struct{}

func (noopFetcher) Fetch(context.Context, mirrorsync.RepoID) (mirrorsync.FetchResult, error) {
	return mirrorsync.FetchResult{}, nil
}

type noopAdvanceDetector struct{}

func (noopAdvanceDetector) DetectAdvances(context.Context, mirrorsync.RepoID, mirrorsync.FetchResult) ([]mirrorsync.Advance, error) {
	return nil, nil
}

type noopMergeabilityChecker struct{}

func (noopMergeabilityChecker) CheckMergeability(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) error {
	return nil
}

// failingMergeabilityChecker always fails with err, so a test can force a
// cycle down the ReportError path deterministically.
type failingMergeabilityChecker struct{ err error }

func (f failingMergeabilityChecker) CheckMergeability(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) error {
	return f.err
}

type noopIngestEnqueuer struct{}

func (noopIngestEnqueuer) EnqueueIngest(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) (bool, error) {
	return false, nil
}

type noopPRPoller struct{}

func (noopPRPoller) PollPRs(context.Context, mirrorsync.RepoID) error { return nil }

// blockingFetcher blocks Fetch on release for every repo, first signaling
// entered exactly once, so a test can prove Tick has not returned while
// the collaborator is still running.
type blockingFetcher struct {
	entered chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (f *blockingFetcher) Fetch(ctx context.Context, repo mirrorsync.RepoID) (mirrorsync.FetchResult, error) {
	if f.once.CompareAndSwap(false, true) {
		close(f.entered)
	}
	<-f.release
	return mirrorsync.FetchResult{}, nil
}

// TestSyncHarness_TickWithRealScheduler_BlocksUntilCycleActuallyFinishes is
// the mutation-test proof required by the wave-4 brief: it gates one
// repo's Fetch step on an explicit release (an artificial delay in a real
// collaborator, not a sleep), and shows that Tick provably has not
// returned while Fetch is blocked (a non-blocking select, not a timing
// guess) and only returns -- with the terminal report already recorded --
// once the gate opens. Tick is called directly; scheduler.Run is never
// started, since Tick drives a cycle in-line rather than through the tick
// channel.
func TestSyncHarness_TickWithRealScheduler_BlocksUntilCycleActuallyFinishes(t *testing.T) {
	t.Parallel()
	lister := &repoListerMock{ListReposFunc: func(ctx context.Context) ([]mirrorsync.RepoID, error) {
		return []mirrorsync.RepoID{"repoA"}, nil
	}}
	var idleReported atomic.Bool
	state := newStateMock()
	state.ReportIdleFunc = func(ctx context.Context, repo mirrorsync.RepoID, enqueuedIngest bool) error {
		idleReported.Store(true)
		return nil
	}
	fetcher := &blockingFetcher{entered: make(chan struct{}), release: make(chan struct{})}
	scheduler := mirrorsync.New(testLogger(), make(chan time.Time), lister, fetcher,
		noopAdvanceDetector{}, noopMergeabilityChecker{}, noopIngestEnqueuer{}, noopPRPoller{}, state)
	h := NewSyncHarness(scheduler)
	tickDone := make(chan []mirrorsync.RepoID, 1)
	go func() { tickDone <- h.Tick(t.Context()) }()
	<-fetcher.entered
	select {
	case <-tickDone:
		t.Fatal("Tick returned while the real scheduler's Fetch step was still blocked -- not a real happens-before")
	default:
	}
	assert.False(t, idleReported.Load(), "the cycle cannot have reported idle before Fetch returned")
	close(fetcher.release)
	var repos []mirrorsync.RepoID
	select {
	case repos = <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick did not return after the real scheduler's blocked Fetch step was released")
	}
	require.Equal(t, []mirrorsync.RepoID{"repoA"}, repos)
	assert.True(t, idleReported.Load(), "Tick must not return before the real scheduler's ReportIdle actually ran")
}
