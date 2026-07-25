package sync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// eventLog is a thread-safe append-only log the tests use to assert the
// order steps ran in across the several per-step mocks a cycle drives.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) forRepo(repo RepoID) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		if strings.HasPrefix(e, string(repo)+":") {
			out = append(out, e)
		}
	}
	return out
}

// repoOutcome is what the harness's SyncStateReporter mock feeds back to
// the test: which repo finished, and its error (nil on ReportIdle).
type repoOutcome struct {
	repo RepoID
	err  error
}

// harness bundles a Scheduler wired to moq mocks with the channels tests
// use to drive and observe it deterministically: an explicit tick channel
// (the manual-scheduler seam) in, and a buffered outcome channel out, fed
// by the state-reporter mock. No test in this file sleeps or polls; every
// wait is a blocking receive on one of these channels.
type harness struct {
	scheduler *Scheduler
	ticks     chan time.Time
	repoList  *RepoListerMock
	fetch     *FetcherMock
	advances  *AdvanceDetectorMock
	merge     *MergeabilityCheckerMock
	ingest    *IngestEnqueuerMock
	prs       *PRPollerMock
	outcomes  chan repoOutcome
}

func newHarness(repoIDs ...RepoID) *harness {
	h := &harness{
		ticks:    make(chan time.Time),
		repoList: &RepoListerMock{},
		fetch:    &FetcherMock{},
		advances: &AdvanceDetectorMock{},
		merge:    &MergeabilityCheckerMock{},
		ingest:   &IngestEnqueuerMock{},
		prs:      &PRPollerMock{},
		outcomes: make(chan repoOutcome, 16),
	}
	h.repoList.ListReposFunc = func(ctx context.Context) ([]RepoID, error) {
		return repoIDs, nil
	}
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) error { return nil }
	h.advances.DetectAdvancesFunc = func(ctx context.Context, repo RepoID) ([]string, error) { return nil, nil }
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []string) error { return nil }
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []string) error { return nil }
	h.prs.PollPRsFunc = func(ctx context.Context, repo RepoID) error { return nil }
	state := &SyncStateReporterMock{
		ReportSyncingFunc: func(ctx context.Context, repo RepoID) {},
		ReportIdleFunc: func(ctx context.Context, repo RepoID) {
			h.outcomes <- repoOutcome{repo: repo}
		},
		ReportErrorFunc: func(ctx context.Context, repo RepoID, err error) {
			h.outcomes <- repoOutcome{repo: repo, err: err}
		},
	}
	h.scheduler = New(testLogger(), h.ticks, h.repoList, h.fetch, h.advances, h.merge, h.ingest, h.prs, state)
	return h
}

// recordStep wires event into an eventLog under key, tagged with the repo
// the call was made for, so assertions can check per-repo call order.
func recordStep(log *eventLog, step string) func(ctx context.Context, repo RepoID) error {
	return func(ctx context.Context, repo RepoID) error {
		log.add(string(repo) + ":" + step)
		return nil
	}
}

func TestScheduler_RunsFiveStepsInOrderForEveryEnrolledRepo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var log eventLog
	h := newHarness("repoA", "repoB")
	h.fetch.FetchFunc = recordStep(&log, "fetch")
	h.advances.DetectAdvancesFunc = func(ctx context.Context, repo RepoID) ([]string, error) {
		log.add(string(repo) + ":advances")
		return []string{"main"}, nil
	}
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []string) error {
		log.add(string(repo) + ":merge")
		return nil
	}
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []string) error {
		log.add(string(repo) + ":ingest")
		return nil
	}
	h.prs.PollPRsFunc = recordStep(&log, "pollprs")
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	first := <-h.outcomes
	second := <-h.outcomes
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.ElementsMatch(t, []RepoID{"repoA", "repoB"}, []RepoID{first.repo, second.repo})
	assert.Equal(t, []string{"repoA:fetch", "repoA:advances", "repoA:merge", "repoA:ingest", "repoA:pollprs"}, log.forRepo("repoA"))
	assert.Equal(t, []string{"repoB:fetch", "repoB:advances", "repoB:merge", "repoB:ingest", "repoB:pollprs"}, log.forRepo("repoB"))
}

func TestScheduler_FailingStepInOneRepoDoesNotBlockAnother(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA", "repoB")
	wantErr := errors.New("merge-tree exploded")
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []string) error {
		if repo == "repoA" {
			return wantErr
		}
		return nil
	}
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	outcomes := map[RepoID]repoOutcome{}
	for range 2 {
		o := <-h.outcomes
		outcomes[o.repo] = o
	}
	require.ErrorIs(t, outcomes["repoA"].err, wantErr)
	require.NoError(t, outcomes["repoB"].err)
	assert.Len(t, h.prs.PollPRsCalls(), 1)
}

func TestScheduler_FailingStepAbortsRemainingStepsForThatRepo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	wantErr := errors.New("merge-tree exploded")
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []string) error { return wantErr }
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	outcome := <-h.outcomes
	require.ErrorIs(t, outcome.err, wantErr)
	assert.Len(t, h.fetch.FetchCalls(), 1)
	assert.Len(t, h.advances.DetectAdvancesCalls(), 1)
	assert.Len(t, h.merge.CheckMergeabilityCalls(), 1)
	assert.Empty(t, h.ingest.EnqueueIngestCalls())
	assert.Empty(t, h.prs.PollPRsCalls())
}

// TestScheduler_RepoDoesNotStartSecondCycleWhileFirstInFlight drives the
// in-flight guard directly through the unexported tick method rather than
// through the ticks channel: the guard is a property of tick's
// synchronous tryStart pass, and calling it directly from the test
// goroutine gives a hard happens-before guarantee (no channel-timing
// assumptions) that the second tick's guard check has really run before
// the assertions below fire.
func TestScheduler_RepoDoesNotStartSecondCycleWhileFirstInFlight(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	h := newHarness("repoA")
	entered := make(chan struct{})
	release := make(chan struct{})
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) error {
		close(entered)
		<-release
		return nil
	}
	h.scheduler.tick(ctx)
	<-entered
	h.scheduler.tick(ctx)
	assert.Len(t, h.fetch.FetchCalls(), 1, "second tick must not start a new cycle while the first is in flight")
	close(release)
	outcome := <-h.outcomes
	require.NoError(t, outcome.err)
	assert.Equal(t, RepoID("repoA"), outcome.repo)
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) error { return nil }
	h.scheduler.tick(ctx)
	outcome = <-h.outcomes
	require.NoError(t, outcome.err)
	assert.Len(t, h.fetch.FetchCalls(), 2, "once the first cycle finishes, a later tick may start a new one")
}

// TestScheduler_SyncFailureIsRetriedOnTheNextTick establishes, at the
// scheduler level, the behavior named by the still-@wip acceptance
// scenario "sync.feature: Sync failures are retried on the next cycle":
// a repo whose step fails on one tick is attempted again — from step 1 —
// on the next tick, with no retry/backoff logic inside the cycle itself.
// See the DEFERRED-WIP note in the bead report for what this test does
// and does not cover.
func TestScheduler_SyncFailureIsRetriedOnTheNextTick(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	wantErr := errors.New("upstream forge unreachable")
	failNext := true
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) error {
		if failNext {
			failNext = false
			return wantErr
		}
		return nil
	}
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	firstOutcome := <-h.outcomes
	require.ErrorIs(t, firstOutcome.err, wantErr)
	h.ticks <- time.Now()
	secondOutcome := <-h.outcomes
	require.NoError(t, secondOutcome.err)
	assert.Len(t, h.fetch.FetchCalls(), 2)
	assert.Len(t, h.advances.DetectAdvancesCalls(), 1, "the failed tick must not have reached step 2")
	assert.Len(t, h.prs.PollPRsCalls(), 1, "only the successful, second-tick cycle reaches step 5")
}
