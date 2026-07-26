package mirrorsync

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
// the test: which repo finished, its error (nil on ReportIdle), and
// whether step 4 enqueued an ingest job before the cycle ended.
type repoOutcome struct {
	repo           RepoID
	err            error
	enqueuedIngest bool
}

// harness bundles a Scheduler wired to moq mocks with the channels tests
// use to drive and observe it deterministically: an explicit tick channel
// (the manual-scheduler seam) in, and a buffered outcome channel out, fed
// by the state-reporter mock. No test in this file sleeps or polls; every
// wait is either a blocking receive on one of these channels or a call to
// scheduler.waitIdle, which blocks on the scheduler's own WaitGroup.
type harness struct {
	scheduler *Scheduler
	ticks     chan time.Time
	repoList  *RepoListerMock
	fetch     *FetcherMock
	advances  *AdvanceDetectorMock
	merge     *MergeabilityCheckerMock
	ingest    *IngestEnqueuerMock
	prs       *PRPollerMock
	state     *SyncStateReporterMock
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
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) { return FetchResult{}, nil }
	h.advances.DetectAdvancesFunc = func(ctx context.Context, repo RepoID, fetched FetchResult) ([]Advance, error) { return nil, nil }
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []Advance) error { return nil }
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []Advance) error { return nil }
	h.prs.PollPRsFunc = func(ctx context.Context, repo RepoID) error { return nil }
	h.state = &SyncStateReporterMock{
		ReportSyncingFunc: func(ctx context.Context, repo RepoID) error { return nil },
		ReportIdleFunc: func(ctx context.Context, repo RepoID, enqueuedIngest bool) error {
			h.outcomes <- repoOutcome{repo: repo, enqueuedIngest: enqueuedIngest}
			return nil
		},
		ReportErrorFunc: func(ctx context.Context, repo RepoID, err error, enqueuedIngest bool) error {
			h.outcomes <- repoOutcome{repo: repo, err: err, enqueuedIngest: enqueuedIngest}
			return nil
		},
	}
	h.scheduler = New(testLogger(), h.ticks, h.repoList, h.fetch, h.advances, h.merge, h.ingest, h.prs, h.state)
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
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		log.add(string(repo) + ":fetch")
		return FetchResult{}, nil
	}
	h.advances.DetectAdvancesFunc = func(ctx context.Context, repo RepoID, fetched FetchResult) ([]Advance, error) {
		log.add(string(repo) + ":advances")
		return []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "bbb"}}, nil
	}
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []Advance) error {
		log.add(string(repo) + ":merge")
		return nil
	}
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []Advance) error {
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
	assert.True(t, first.enqueuedIngest)
	assert.True(t, second.enqueuedIngest)
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
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []Advance) error {
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
	assert.False(t, outcomes["repoA"].enqueuedIngest, "repoA never reached step 4")
	assert.True(t, outcomes["repoB"].enqueuedIngest)
	assert.Len(t, h.prs.PollPRsCalls(), 1)
}

func TestScheduler_FailingStepAbortsRemainingStepsForThatRepo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	wantErr := errors.New("merge-tree exploded")
	h.merge.CheckMergeabilityFunc = func(ctx context.Context, repo RepoID, advanced []Advance) error { return wantErr }
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	outcome := <-h.outcomes
	require.ErrorIs(t, outcome.err, wantErr)
	assert.False(t, outcome.enqueuedIngest)
	assert.Len(t, h.fetch.FetchCalls(), 1)
	assert.Len(t, h.advances.DetectAdvancesCalls(), 1)
	assert.Len(t, h.merge.CheckMergeabilityCalls(), 1)
	assert.Empty(t, h.ingest.EnqueueIngestCalls())
	assert.Empty(t, h.prs.PollPRsCalls())
}

// TestScheduler_ReportsEnqueuedIngestWhenLaterStepFails covers the case
// loam-giq.9's DESIGN turns on: step 4 (ingest enqueue) succeeds, handing
// off sync_state ownership for this tick, and step 5 (PR polling) then
// fails. The error outcome must still carry enqueuedIngest = true so
// giq.9 can tell its own error write would race the ingest worker's.
func TestScheduler_ReportsEnqueuedIngestWhenLaterStepFails(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	wantErr := errors.New("forge unreachable")
	h.prs.PollPRsFunc = func(ctx context.Context, repo RepoID) error { return wantErr }
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	outcome := <-h.outcomes
	require.ErrorIs(t, outcome.err, wantErr)
	assert.True(t, outcome.enqueuedIngest, "step 4 succeeded before step 5 failed")
}

// TestScheduler_RepoDoesNotStartSecondCycleWhileFirstInFlight drives the
// in-flight guard directly through the unexported tick method rather than
// through the ticks channel, and asserts on tick's return value — the
// repos it actually started — rather than on a mock's call count read
// from a different goroutine. Asserting a goroutine side effect right
// after tick returns (e.g. FetchCalls()) races the very goroutine tick
// just spawned; asserting tick's synchronous return value does not.
// waitIdle (backed by the scheduler's WaitGroup) is the sleep-free
// happens-before barrier used to wait for that goroutine before
// inspecting call counts afterward.
func TestScheduler_RepoDoesNotStartSecondCycleWhileFirstInFlight(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	h := newHarness("repoA")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		once.Do(func() { entered <- struct{}{} })
		<-release
		return FetchResult{}, nil
	}
	started, err := h.scheduler.tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RepoID{"repoA"}, started)
	<-entered
	secondStarted, err := h.scheduler.tick(ctx)
	require.NoError(t, err)
	assert.Empty(t, secondStarted, "second tick must not start a new cycle while the first is in flight")
	close(release)
	h.scheduler.waitIdle()
	assert.Len(t, h.fetch.FetchCalls(), 1)
	started, err = h.scheduler.tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RepoID{"repoA"}, started, "once the first cycle finishes, a later tick may start a new one")
	h.scheduler.waitIdle()
	assert.Len(t, h.fetch.FetchCalls(), 2)
}

// TestScheduler_GuardHeldUntilOutcomeIsReported guards against releasing
// the in-flight guard before a cycle's outcome is reported: it blocks
// cycle 1 inside ReportIdle and fires a second tick while it is stuck
// there. If finish ran before the report (as it once did), the second
// tick would start cycle 2 — and cycle 2's ReportSyncing could then land
// before cycle 1's still-pending ReportIdle, so a reporter would briefly
// see a stale idle overwrite an in-progress syncing. Asserting on tick's
// return value (not a goroutine side effect) keeps this deterministic.
func TestScheduler_GuardHeldUntilOutcomeIsReported(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	h := newHarness("repoA")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	h.state.ReportIdleFunc = func(ctx context.Context, repo RepoID, enqueuedIngest bool) error {
		once.Do(func() { entered <- struct{}{} })
		<-release
		return nil
	}
	started, err := h.scheduler.tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RepoID{"repoA"}, started)
	<-entered
	secondStarted, err := h.scheduler.tick(ctx)
	require.NoError(t, err)
	assert.Empty(t, secondStarted, "guard must still be held while the outcome is being reported")
	close(release)
	h.scheduler.waitIdle()
	started, err = h.scheduler.tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RepoID{"repoA"}, started, "guard releases once the outcome has been reported")
	h.scheduler.waitIdle()
}

// DEFERRED-WIP: Sync failures are retried on the next cycle -> TestScheduler_SyncFailureIsRetriedOnTheNextTick (establishes the scheduler-level mechanism only: a repo whose step fails on tick 1 is retried, from step 1, on tick 2, with no retry logic inside the cycle itself — only the next tick attempts it again. Does NOT cover the real "upstream forge unreachable" condition, which is owned by giq.2's Fetcher plus the fake-forge control API; does NOT cover persistence of repos.sync_state to error/healthy, owned by giq.9; and does NOT cover web-visible sync status, which testing-spec:60-67 drives through the connect-go admin client rather than this package. The godog scenario in features/sync.feature stays @wip.)
//
// TestScheduler_SyncFailureIsRetriedOnTheNextTick establishes, at the
// scheduler level, the behavior named by the still-@wip acceptance
// scenario "sync.feature: Sync failures are retried on the next cycle":
// a repo whose step fails on one tick is attempted again — from step 1 —
// on the next tick, with no retry/backoff logic inside the cycle itself.
func TestScheduler_SyncFailureIsRetriedOnTheNextTick(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	wantErr := errors.New("upstream forge unreachable")
	failNext := true
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		if failNext {
			failNext = false
			return FetchResult{}, wantErr
		}
		return FetchResult{}, nil
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

// TestScheduler_Tick_PropagatesListReposError is loam-hhh's core claim:
// Tick's error return lets a manual-tick caller tell a ListRepos failure
// apart from an empty enrollment, unlike the value tick alone (and the
// pre-fix Tick) returned.
func TestScheduler_Tick_PropagatesListReposError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	h := newHarness()
	wantErr := errors.New("db unreachable")
	h.repoList.ListReposFunc = func(ctx context.Context) ([]RepoID, error) { return nil, wantErr }
	started, err := h.scheduler.Tick(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, started)
}

// TestScheduler_Tick_EmptyEnrollmentReturnsNoError is
// TestScheduler_Tick_PropagatesListReposError's contrasting case: a
// ListRepos call that succeeds with zero repos is not an error at all, so
// Tick must return a nil error alongside the empty slice -- the two
// "nothing started" outcomes loam-hhh's bug report says were previously
// indistinguishable.
func TestScheduler_Tick_EmptyEnrollmentReturnsNoError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	h := newHarness()
	started, err := h.scheduler.Tick(ctx)
	require.NoError(t, err)
	assert.Empty(t, started)
}

// TestScheduler_Run_ListReposFailureDoesNotStopLoop drives Run through a
// tick whose ListRepos fails, followed by a tick whose ListRepos succeeds,
// and proves both that Run kept accepting ticks after the failure (the
// send on h.ticks would time out if Run had returned) and that the
// following tick ran an entirely normal cycle to completion -- not just
// "didn't crash", but true log-and-continue, unchanged by giving Tick an
// error return.
func TestScheduler_Run_ListReposFailureDoesNotStopLoop(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	wantErr := errors.New("db unreachable")
	failNext := true
	h.repoList.ListReposFunc = func(ctx context.Context) ([]RepoID, error) {
		if failNext {
			failNext = false
			return nil, wantErr
		}
		return []RepoID{"repoA"}, nil
	}
	go h.scheduler.Run(ctx)
	sendTick := func() {
		select {
		case h.ticks <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not accept a tick -- it may have stopped running after the ListRepos failure")
		}
	}
	sendTick()
	sendTick()
	select {
	case outcome := <-h.outcomes:
		require.NoError(t, outcome.err)
		assert.Equal(t, RepoID("repoA"), outcome.repo)
	case <-time.After(5 * time.Second):
		t.Fatal("the tick after the ListRepos failure never produced an outcome -- Run did not continue normally")
	}
	assert.Len(t, h.fetch.FetchCalls(), 1, "only the second, successfully-listed tick ever reached a cycle")
}
