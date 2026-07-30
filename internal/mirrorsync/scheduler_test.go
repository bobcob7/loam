package mirrorsync

import (
	"context"
	"errors"
	"fmt"
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

// buildHarness wires every mock a harness needs and its ListRepos answer,
// but does not construct the Scheduler itself -- newHarness and
// newHarnessWithOptions each do that over the same mocks, the latter
// forwarding opts (e.g. WithMaxConcurrentCycles) to New.
func buildHarness(repoIDs []RepoID) *harness {
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
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []Advance) (bool, error) { return false, nil }
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
	return h
}

func newHarness(repoIDs ...RepoID) *harness {
	h := buildHarness(repoIDs)
	h.scheduler = New(testLogger(), h.ticks, h.repoList, h.fetch, h.advances, h.merge, h.ingest, h.prs, h.state)
	return h
}

// newHarnessWithOptions is newHarness plus Scheduler.Option support (e.g.
// WithMaxConcurrentCycles), for tests that need to override a New default
// no other harness constructor exposes.
func newHarnessWithOptions(repoIDs []RepoID, opts ...Option) *harness {
	h := buildHarness(repoIDs)
	h.scheduler = New(testLogger(), h.ticks, h.repoList, h.fetch, h.advances, h.merge, h.ingest, h.prs, h.state, opts...)
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
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []Advance) (bool, error) {
		log.add(string(repo) + ":ingest")
		return true, nil
	}
	h.prs.PollPRsFunc = recordStep(&log, "pollprs")
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	first := <-h.outcomes
	second := <-h.outcomes
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.True(t, first.enqueuedIngest, "the enqueuer reported a genuine enqueue for this advance")
	assert.True(t, second.enqueuedIngest, "the enqueuer reported a genuine enqueue for this advance")
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
	assert.False(t, outcomes["repoB"].enqueuedIngest, "repoB reached step 4 but the harness's default IngestEnqueuer enqueues nothing (loam-ax1: reaching step 4 without error is not the same as actually enqueuing a job)")
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
	h.advances.DetectAdvancesFunc = func(ctx context.Context, repo RepoID, fetched FetchResult) ([]Advance, error) {
		return []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "bbb"}}, nil
	}
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []Advance) (bool, error) { return true, nil }
	h.prs.PollPRsFunc = func(ctx context.Context, repo RepoID) error { return wantErr }
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	outcome := <-h.outcomes
	require.ErrorIs(t, outcome.err, wantErr)
	assert.True(t, outcome.enqueuedIngest, "step 4 genuinely enqueued a job before step 5 failed")
}

// TestScheduler_PropagatesWhatIngestEnqueuerActuallyReports is loam-ax1's
// core regression proof: runSteps must forward IngestEnqueuer's own
// enqueued return value, never synthesize true from "step 4 did not
// error". Before this fix, every non-error path out of runSteps returned
// enqueuedIngest=true unconditionally, so a cycle where nothing was
// enqueued (e.g. no upstream advances) looked identical, from
// SyncStateReporter's side, to one that genuinely handed sync_state
// ownership to the ingest worker -- permanently suppressing
// ReportIdle/ReportError and pinning repos.sync_state at 'syncing'
// forever (docs/sync-spec.md :85). Both subtests reach step 4 without
// error; only the IngestEnqueuer mock's own answer differs, so a
// hardcoded-true runSteps passes the "true" subtest but fails the
// "false" one by assertion, not by hang or panic.
//
// Each subtest's harness is kept internally consistent with
// IngestEnqueuer's own contract ("enqueued is false only when advanced was
// empty ..."): the false subtest leaves DetectAdvances at the harness
// default (no advances), and the true subtest wires a real advance
// alongside the enqueuer's true answer, so neither fixture models a
// combination the interface itself declares impossible.
func TestScheduler_PropagatesWhatIngestEnqueuerActuallyReports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		hasAdvance bool
		enqueued   bool
	}{
		{name: "nothing enqueued lets the terminal report through", hasAdvance: false, enqueued: false},
		{name: "a genuine enqueue hands off ownership", hasAdvance: true, enqueued: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := newHarness("repoA")
			if tt.hasAdvance {
				h.advances.DetectAdvancesFunc = func(ctx context.Context, repo RepoID, fetched FetchResult) ([]Advance, error) {
					return []Advance{{Branch: "main", OldSHA: "aaa", NewSHA: "bbb"}}, nil
				}
			}
			h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []Advance) (bool, error) {
				return tt.enqueued, nil
			}
			go h.scheduler.Run(ctx)
			h.ticks <- time.Now()
			outcome := <-h.outcomes
			require.NoError(t, outcome.err)
			assert.Equal(t, tt.enqueued, outcome.enqueuedIngest, "enqueuedIngest must equal exactly what IngestEnqueuer reported")
		})
	}
}

// TestScheduler_PropagatesEnqueuedEvenWhenEnqueueIngestErrors covers
// IngestEnqueuer's documented partial-failure contract: advanced is a
// slice, so a real implementation can enqueue an earlier branch and then
// fail on a later one, returning (true, err) -- not (false, err). Before
// this fix, runSteps discarded the returned enqueued on EnqueueIngest's
// error path and always reported false there, which would silently drop
// an ownership hand-off that had already happened and let ReportError
// clobber a row the ingest worker now owns -- the same synthesised-value
// defect this bead exists to remove, just on the error path instead of
// the success path.
func TestScheduler_PropagatesEnqueuedEvenWhenEnqueueIngestErrors(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	wantErr := errors.New("enqueuing branch b: connection refused")
	h.ingest.EnqueueIngestFunc = func(ctx context.Context, repo RepoID, advanced []Advance) (bool, error) {
		return true, wantErr
	}
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	outcome := <-h.outcomes
	require.ErrorIs(t, outcome.err, wantErr)
	assert.True(t, outcome.enqueuedIngest, "a partial enqueue before the error must still be reported, not coerced to false")
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

// TestScheduler_Shutdown_NoInFlightCyclesReturnsImmediately proves the
// trivial case: with nothing ever ticked, Shutdown's wait is already
// satisfied and it returns nil without waiting out ctx's deadline at all.
func TestScheduler_Shutdown_NoInFlightCyclesReturnsImmediately(t *testing.T) {
	t.Parallel()
	h := newHarness("repoA")
	assert.NoError(t, h.scheduler.Shutdown(t.Context()))
}

// TestScheduler_Shutdown_BlocksUntilInFlightCycleFinishes is the
// discriminating proof that Shutdown actually waits for a cycle already
// running rather than merely riding Run's own (immediate) return on ctx
// cancellation: a mutant Shutdown that returned nil right away -- e.g. by
// forgetting to call waitIdle, or by racing it against ctx.Done() instead
// of gating on it -- would pass every OTHER test in this file (they never
// call Shutdown) yet fail this one, since it asserts, via a real
// happens-before channel signal rather than a timing guess, that
// Shutdown has NOT returned while the cycle is deliberately held open,
// and DOES return the instant it is released.
func TestScheduler_Shutdown_BlocksUntilInFlightCycleFinishes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	fetchStarted := make(chan struct{})
	releaseFetch := make(chan struct{})
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		close(fetchStarted)
		<-releaseFetch
		return FetchResult{}, nil
	}
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	<-fetchStarted // the cycle is now genuinely in flight, blocked in Fetch

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- h.scheduler.Shutdown(context.Background()) }()

	select {
	case err := <-shutdownErr:
		t.Fatalf("Shutdown returned (err=%v) before the in-flight cycle was released -- it is not actually waiting for it", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseFetch)
	select {
	case err := <-shutdownErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the in-flight cycle finished")
	}
	<-h.outcomes // drain the cycle's terminal report so it does not leak past the test
}

// TestScheduler_Shutdown_AbandonsWaitWhenContextExpiresFirst proves the
// grace-period half of the contract: a cycle that outlives the ctx handed
// to Shutdown causes Shutdown to give up and return that ctx's error,
// rather than blocking forever regardless of what the caller asked for --
// the property docs/server-spec.md's Shutdown grace period depends on.
func TestScheduler_Shutdown_AbandonsWaitWhenContextExpiresFirst(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	fetchStarted := make(chan struct{})
	releaseFetch := make(chan struct{})
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		close(fetchStarted)
		<-releaseFetch
		return FetchResult{}, nil
	}
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	<-fetchStarted

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shutdownCancel()
	err := h.scheduler.Shutdown(shutdownCtx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	close(releaseFetch) // let the still-running cycle finish so it does not leak past the test
	<-h.outcomes
}

// TestScheduler_MaxConcurrentCyclesBoundsTotalInFlightCycles is loam-5v5's
// core claim: WithMaxConcurrentCycles caps concurrent CYCLES ACROSS repos,
// an axis the per-repo tryStart guard never covered (tryStart only stops
// the SAME repo starting a second cycle while its first is in flight --
// see TestScheduler_RepoDoesNotStartSecondCycleWhileFirstInFlight, which
// covers exactly one repo and proves nothing about N of them at once).
//
// n=5 repos, a bound k=2: every Fetch call reports its own entry on
// started before blocking on release, a handshake instrumented through
// the Fetcher collaborator seam rather than any sleep. The test reads
// exactly k values off started (guaranteed to arrive: a bounded pool of
// size k always eventually runs k workers, however the scheduler happens
// to be scheduled) and then asserts, via the same bounded-wait idiom this
// file already uses to prove an absence
// (TestScheduler_Shutdown_BlocksUntilInFlightCycleFinishes's 200ms
// select), that a (k+1)th does not arrive before any release. It then
// drains all n by releasing one slot at a time and confirming a queued
// cycle proceeds only once room exists, which is the failing half without
// the bound: an unbounded scheduler starts all n Fetch calls immediately,
// so this file's own non-vacuity check (temporarily neutering the sem
// gate in cycle) makes both the initial "no more than k" assertion and
// the later per-release accounting fail. Verified concretely: removing the
// sem acquire/release block from cycle, and separately widening the buffer
// to n+1 (an off-by-one), each fail this test at its "beyond the bound"
// assertion. Its sibling below passes under both, by design -- that one
// pins loam-f75's blocking contract, not this bound.
func TestScheduler_MaxConcurrentCyclesBoundsTotalInFlightCycles(t *testing.T) {
	t.Parallel()
	const n, k = 5, 2
	repoIDs := make([]RepoID, n)
	for i := range repoIDs {
		repoIDs[i] = RepoID(fmt.Sprintf("repo%d", i))
	}
	h := newHarnessWithOptions(repoIDs, WithMaxConcurrentCycles(k))
	started := make(chan struct{}, n)
	release := make(chan struct{})
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		started <- struct{}{}
		<-release
		return FetchResult{}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	for i := range k {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of the expected %d concurrent cycles started", i, k)
		}
	}
	select {
	case <-started:
		t.Fatal("a cycle beyond the bound started before any of the first k released -- the bound did not hold")
	case <-time.After(200 * time.Millisecond):
	}
	for i := range n {
		release <- struct{}{}
		if i < n-k {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatalf("releasing a slot (release %d) did not let a queued cycle proceed -- the freed slot was not reused", i)
			}
		}
	}
	for range n {
		<-h.outcomes
	}
}

// tickResult carries Scheduler.Tick's two return values across a
// goroutine boundary; TestScheduler_Tick_StillBlocksUntilEveryBoundedCycleFinishes
// uses it rather than asserting inside the goroutine that calls Tick,
// since a testify require/assert failure there would not fail this test
// the way a failure on the test's own goroutine does.
type tickResult struct {
	started []RepoID
	err     error
}

// TestScheduler_Tick_StillBlocksUntilEveryBoundedCycleFinishes is the
// acceptance criteria's second claim: bounding concurrency must not weaken
// Tick's contract ("blocks until every cycle it started has finished
// reporting", loam-f75). n=3 repos, bound k=1, so every cycle is
// serialized behind the bound; Tick must still not return until all three
// have reported, not just the first one the bound let through.
func TestScheduler_Tick_StillBlocksUntilEveryBoundedCycleFinishes(t *testing.T) {
	t.Parallel()
	const n, k = 3, 1
	repoIDs := []RepoID{"repoA", "repoB", "repoC"}
	h := newHarnessWithOptions(repoIDs, WithMaxConcurrentCycles(k))
	release := make(chan struct{})
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		<-release
		return FetchResult{}, nil
	}
	tickDone := make(chan tickResult, 1)
	go func() {
		started, err := h.scheduler.Tick(t.Context())
		tickDone <- tickResult{started: started, err: err}
	}()
	select {
	case <-tickDone:
		t.Fatal("Tick returned before any bounded cycle was released -- it is not actually waiting for them")
	case <-time.After(200 * time.Millisecond):
	}
	for range n {
		release <- struct{}{}
	}
	select {
	case result := <-tickDone:
		require.NoError(t, result.err)
		assert.ElementsMatch(t, repoIDs, result.started)
	case <-time.After(5 * time.Second):
		t.Fatal("Tick did not return after every bounded cycle finished")
	}
	for range n {
		<-h.outcomes
	}
}

// TestScheduler_ConcurrentTickBlocksUntilFirstCompletes is loam-f75's core
// claim, proven deterministically rather than by stress: a second Tick
// call arriving while an earlier one's cycle is still in flight on the
// same Scheduler must BLOCK behind driveMu, not race the shared
// WaitGroup. Before driveMu existed, this exact interleaving -- a fresh
// tick()'s wg.Add racing an in-flight call's wg.Wait -- panicked with
// "sync: WaitGroup is reused before previous Wait has returned"
// (reproduced on demand, both under -race and without it, by looping this
// shape a few hundred times over a fresh Scheduler each time; see
// TestScheduler_ConcurrentTicksDoNotRaceTheSharedWaitGroup below for that
// same proof kept as a permanent regression). This test instead asserts
// the documented, now-safe behavior directly: the second call does not
// return early, and once it does, it reports it started nothing new,
// because repoA was still claimed by the first call's cycle at the moment
// the second call's own tick() ran.
func TestScheduler_ConcurrentTickBlocksUntilFirstCompletes(t *testing.T) {
	t.Parallel()
	h := newHarness("repoA")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return FetchResult{}, nil
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := h.scheduler.Tick(context.Background())
		firstDone <- err
	}()
	<-entered // the first Tick's cycle is now genuinely in flight, blocked in Fetch

	secondDone := make(chan tickResult, 1)
	go func() {
		started, err := h.scheduler.Tick(context.Background())
		secondDone <- tickResult{started: started, err: err}
	}()

	select {
	case <-secondDone:
		t.Fatal("the second, concurrent Tick call returned before the first Tick's cycle finished -- it must serialize behind driveMu, not race the shared WaitGroup (loam-f75)")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-firstDone)
	select {
	case result := <-secondDone:
		require.NoError(t, result.err)
		assert.Empty(t, result.started, "repoA was already running under the first call's cycle when the second call's own tick() ran, so it starts nothing new -- it only had to wait its turn")
	case <-time.After(5 * time.Second):
		t.Fatal("the second Tick call never returned after the first call's in-flight cycle completed")
	}
	<-h.outcomes // drain the one cycle's terminal report so it does not leak past the test
}

// TestScheduler_TickDuringRunSerializesInsteadOfRacing is loam-f75's other
// named interleaving: a manual Tick call arriving while Run's own tick is
// still driving a cycle. Same proof shape as
// TestScheduler_ConcurrentTickBlocksUntilFirstCompletes above, just with
// Run standing in for the first caller.
func TestScheduler_TickDuringRunSerializesInsteadOfRacing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := newHarness("repoA")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return FetchResult{}, nil
	}
	go h.scheduler.Run(ctx)
	h.ticks <- time.Now()
	<-entered // Run's own tick has started repoA's cycle and it is in flight

	tickDone := make(chan tickResult, 1)
	go func() {
		started, err := h.scheduler.Tick(context.Background())
		tickDone <- tickResult{started: started, err: err}
	}()

	select {
	case <-tickDone:
		t.Fatal("Tick returned while Run's own cycle was still in flight -- it must serialize behind driveMu, not race the shared WaitGroup (loam-f75)")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case result := <-tickDone:
		require.NoError(t, result.err)
		assert.Empty(t, result.started, "repoA was already running under Run's cycle when Tick's own tick() ran, so it starts nothing new")
	case <-time.After(5 * time.Second):
		t.Fatal("Tick never returned after Run's in-flight cycle completed")
	}
	<-h.outcomes // drain the one cycle's terminal report so it does not leak past the test
}

// TestScheduler_ConcurrentTicksDoNotRaceTheSharedWaitGroup is
// loam-f75's stress-shaped regression: it reproduces, near-verbatim, the
// interleaving the bead reported (repeated concurrent Tick calls racing
// finish/wg.Done against a fresh tick's wg.Add as repos free up and get
// re-claimed almost immediately). Confirmed, before driveMu existed, to
// panic with "sync: WaitGroup is reused before previous Wait has
// returned" reliably within the first handful of the loop's 200
// iterations, both under -race (as a data race) and without it (as the
// literal panic) -- reverting the driveMu change and rerunning this test
// reproduces that panic again. With the fix, every iteration must
// complete cleanly: this is the shape a vacuous "guard removed" pass
// cannot fake, since the old code fails it almost immediately.
func TestScheduler_ConcurrentTicksDoNotRaceTheSharedWaitGroup(t *testing.T) {
	t.Parallel()
	for range 200 {
		h := newHarness("repoA", "repoB")
		h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
			return FetchResult{}, nil
		}
		h.state.ReportIdleFunc = func(ctx context.Context, repo RepoID, enqueuedIngest bool) error { return nil }
		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		for range 4 {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				_, err := h.scheduler.Tick(context.Background())
				assert.NoError(t, err)
			}()
		}
		start.Done()
		done.Wait()
	}
}

// TestScheduler_TickDuringRunDoesNotRaceTheSharedWaitGroup is
// TestScheduler_ConcurrentTicksDoNotRaceTheSharedWaitGroup's sibling for
// the Tick-during-Run interleaving: Run is fed a steady stream of ticks
// on its own goroutine while several goroutines hammer Tick concurrently
// on the same Scheduler. Confirmed, before driveMu existed, to panic
// reliably on the first iteration.
func TestScheduler_TickDuringRunDoesNotRaceTheSharedWaitGroup(t *testing.T) {
	t.Parallel()
	for range 50 {
		h := newHarness("repoA", "repoB")
		h.fetch.FetchFunc = func(ctx context.Context, repo RepoID) (FetchResult, error) {
			return FetchResult{}, nil
		}
		h.state.ReportIdleFunc = func(ctx context.Context, repo RepoID, enqueuedIngest bool) error { return nil }
		ctx, cancel := context.WithCancel(context.Background())
		go h.scheduler.Run(ctx)
		var done sync.WaitGroup
		for range 4 {
			done.Add(1)
			go func() {
				defer done.Done()
				for range 20 {
					_, err := h.scheduler.Tick(context.Background())
					assert.NoError(t, err)
				}
			}()
		}
		go func() {
			for range 200 {
				select {
				case h.ticks <- time.Now():
				case <-ctx.Done():
					return
				}
			}
		}()
		done.Wait()
		cancel()
	}
}

// TestScheduler_WithMaxConcurrentCyclesNonPositiveIsANoOp pins the
// documented contract on WithMaxConcurrentCycles that n <= 0 leaves the
// scheduler unbounded rather than, say, creating a zero-capacity channel
// that would deadlock every cycle forever. Nothing else asserted it, so
// the guard could have been dropped silently.
func TestScheduler_WithMaxConcurrentCyclesNonPositiveIsANoOp(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, -1} {
		s := New(testLogger(), nil, nil, nil, nil, nil, nil, nil, nil, WithMaxConcurrentCycles(n))
		assert.Nil(t, s.sem, "WithMaxConcurrentCycles(%d) must leave the scheduler unbounded, not install a zero-capacity gate", n)
	}
}
