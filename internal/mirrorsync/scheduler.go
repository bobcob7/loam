package mirrorsync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Scheduler runs the Mirror Sync cycle for every enrolled repo on every
// received tick, serialized per repo: a repo never starts a new cycle
// while its previous one is still in flight, but different repos cycle
// concurrently. Construct one with New; run it with Run.
//
// "Concurrently" above has no ceiling by default: tick spawns one goroutine
// per enrolled repo on every tick (see tick), and the per-repo tryStart
// guard only ever stops the SAME repo from starting a second cycle while
// its first is in flight -- it caps nothing across DIFFERENT repos. A
// deployment enrolling a few thousand repos would otherwise issue that
// many concurrent git fetches and forge API calls on a single tick
// (loam-5v5). WithMaxConcurrentCycles, passed to New, bounds that total,
// across every repo combined.
//
// Run and Tick may be called concurrently with each other, or with
// themselves, on one Scheduler: driveMu (see Tick's doc comment)
// serializes every tick-plus-wait sequence, so a caller that does so
// blocks rather than corrupting the scheduler's internal WaitGroup
// (loam-f75). That is a safety net, not an invitation -- production and
// the acceptance harness each still keep their own Scheduler local and
// reachable through only one of the two methods (see cmd/server's
// newSyncRunner and acceptance_harness_test.go's newSyncHarness), because
// a Tick reachable on a Run-driven, wall-clock scheduler would silently
// block on the next real tick's drain, which is never useful even though
// it is now safe.
type Scheduler struct {
	logger       *slog.Logger
	ticks        <-chan time.Time
	repos        RepoLister
	fetcher      Fetcher
	advances     AdvanceDetector
	mergeability MergeabilityChecker
	ingest       IngestEnqueuer
	prPoller     PRPoller
	state        SyncStateReporter
	mu           sync.Mutex
	running      map[RepoID]struct{}
	wg           sync.WaitGroup
	sem          chan struct{}
	driveMu      sync.Mutex
}

// Option configures optional Scheduler behavior beyond New's required
// collaborators, mirroring internal/ingest.Pool's own Option pattern
// (pool.go: WithBackoff, WithPollInterval) for the same reason: a value
// only some callers need to override from New's default has no other seam
// -- docs/testing-spec.md's "the components already take their trigger as
// an input; tests just own it" principle covers the tick channel but not
// every tunable.
type Option func(*Scheduler)

// WithMaxConcurrentCycles bounds the number of repo cycles the scheduler
// runs at once, across every enrolled repo combined -- see Scheduler's own
// doc comment for why that is a different, previously-uncovered axis from
// the per-repo tryStart guard. The bound gates entry to cycle itself
// (acquired first thing, released via a deferred receive), not tick's
// spawn loop, so tick keeps returning its started list the instant every
// repo's tryStart guard has been claimed, exactly as before -- only the
// goroutines' own work (the five Mirror Sync steps and their terminal
// report) waits for a free slot. docs/testing-spec.md's "Manual
// scheduler" contract is unaffected by that wait: Scheduler.Tick still
// blocks, via waitIdle, until every cycle it started -- queued on the
// bound or not -- has finished reporting.
//
// n <= 0 is a no-op: New's default is unbounded, matching this package's
// behavior before this option existed, so a caller that does not pass
// WithMaxConcurrentCycles keeps its current fan-out unchanged.
func WithMaxConcurrentCycles(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.sem = make(chan struct{}, n)
		}
	}
}

// New builds a Scheduler. ticks is the trigger seam: production passes
// time.NewTicker(LOAM_SYNC_INTERVAL).C; tests pass a channel they write
// to explicitly, so cycles run to completion on an explicit tick rather
// than a wall-clock timer (docs/testing-spec.md -> Manual scheduler).
// opts applies after every required argument is set; see Option and
// WithMaxConcurrentCycles.
func New(logger *slog.Logger, ticks <-chan time.Time, repos RepoLister, fetcher Fetcher, advances AdvanceDetector, mergeability MergeabilityChecker, ingest IngestEnqueuer, prPoller PRPoller, state SyncStateReporter, opts ...Option) *Scheduler {
	s := &Scheduler{
		logger:       logger,
		ticks:        ticks,
		repos:        repos,
		fetcher:      fetcher,
		advances:     advances,
		mergeability: mergeability,
		ingest:       ingest,
		prPoller:     prPoller,
		state:        state,
		running:      make(map[RepoID]struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run blocks, starting one cycle per enrolled repo on every received
// tick, until ctx is canceled or the tick channel is closed. Each repo's
// cycle runs in its own goroutine so a slow or stuck repo never blocks
// another repo's cycle; Run itself only ever blocks on the tick source
// (and, per driveMu below, on a concurrent Tick call already in
// progress).
// A ListRepos failure is logged and the loop continues to the next tick —
// production's retry is simply trying again later, never stopping the
// process over a transient listing failure (loam-hhh). Tick, below,
// propagates that same failure instead, since a manual-tick caller has no
// "next tick" to fall back on and needs to tell it apart from an empty
// enrollment.
//
// Each call to tick is serialized against every other tick and against
// every Tick call via driveMu (see Tick's doc comment -- loam-f75): two
// received ticks can never race each other's wg.Add, and a concurrent
// Tick can never observe this call's Add racing its own Wait. The lock is
// held only around the (fast) listing-and-spawning step, never around the
// cycle goroutines' own work, so a slow cycle never delays the next
// tick's spawn beyond the time it takes to list and launch.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-s.ticks:
			if !ok {
				return
			}
			s.driveMu.Lock()
			_, err := s.tick(ctx)
			s.driveMu.Unlock()
			if err != nil {
				s.logger.Error("tick failed", "error", err)
			}
		}
	}
}

// tick lists the enrolled repos, starts a cycle for each one that is not
// already running, and returns the repos it STARTED this call -- which,
// with WithMaxConcurrentCycles in force, means "handed to a cycle
// goroutine", not "already doing work": a returned repo may still be
// waiting for a slot. waitIdle and Tick account for it either way, since
// the WaitGroup is incremented before the goroutine is launched
// alongside a ListRepos failure, if any. Listing and the per-repo
// in-flight guard both run synchronously here, before any per-repo
// goroutine is spawned, so a second tick arriving while a repo's cycle is
// still in flight reliably observes that repo as running and skips it — a
// property the return value lets callers (and tests) assert on directly,
// without inferring it from goroutine timing. It never logs the error
// itself: Run and Tick each decide what a listing failure means for their
// own caller (log-and-continue vs. propagate), so tick only reports it.
func (s *Scheduler) tick(ctx context.Context) ([]RepoID, error) {
	repos, err := s.repos.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing enrolled repos: %w", err)
	}
	var started []RepoID
	for _, repo := range repos {
		if !s.tryStart(repo) {
			continue
		}
		started = append(started, repo)
		s.wg.Add(1)
		go s.cycle(ctx, repo)
	}
	return started, nil
}

// waitIdle blocks until every cycle started so far has finished. It
// exists for tests: a hard happens-before barrier so assertions after a
// tick never race the goroutines it started, without sleeping or
// polling (docs/testing-spec.md -> Manual scheduler).
func (s *Scheduler) waitIdle() {
	s.wg.Wait()
}

// Shutdown blocks until every cycle already in flight when it is called
// finishes, or ctx is done, whichever comes first. It exists because Run
// returns the instant ctx is canceled while any cycle goroutines it
// started keep running (loam-giq.1's per-repo WaitGroup has no drain
// caller of its own) -- there was previously no seam for
// docs/server-spec.md's Shutdown contract ("let ... the current sync ...
// jobs drain, bounded by a grace period") to hook into: a caller that
// just waited for Run to return would not actually be waiting for
// anything. Callers should cancel the ctx passed to Run first (so no new
// cycle starts), then call Shutdown with a context bounded by the
// shutdown grace period; a cycle still running when that ctx's deadline
// passes is abandoned here (this method returns ctx.Err()) and follows
// the crash-recovery path on next startup, per that same Shutdown
// contract's "killed ... and re-queued" clause -- Shutdown does not kill
// or cancel the cycle itself, since Postgres reads/writes mid-cycle
// should not be torn down mid-transaction; it only stops waiting for it.
//
// Safe to call concurrently with Run (whether Run's own ctx has been
// canceled yet or not) and safe to call more than once.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.waitIdle()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Tick runs one cycle per enrolled repo and blocks until every cycle
// started so far has finished -- not just the cycles this call started,
// but any still in flight from an earlier call. This is the seam
// docs/testing-spec.md's "Manual scheduler" needs: finish (the in-flight
// guard release) is unobservable from outside this package, since it
// runs after a cycle's terminal report; cycle's deferred wg.Done, this
// method's completion signal, always runs after finish too, so waitIdle
// is a strictly stronger barrier than any external decorator built on
// the terminal report alone could construct. Tests should call Tick
// directly rather than writing to the tick channel and separately trying
// to detect completion.
//
// The returned error is a ListRepos failure, if one occurred, so a
// manual-tick caller can tell "no repo is enrolled" (nil error, empty
// slice) apart from "listing the enrolled repos failed" (non-nil error,
// empty slice) -- the distinction Run's own log-and-continue swallows,
// correctly, since production always gets a next tick to retry on
// (loam-hhh). A non-nil error here still means zero repos started this
// call; it carries no information about any cycle still in flight from an
// earlier, successful call, since waitIdle below waits for those
// regardless of whether this call's own listing succeeded.
//
// Calling Tick concurrently with another Tick, or with Run, on the same
// Scheduler is safe but SERIALIZED, never parallel: driveMu is held for
// this call's entire tick-plus-wait, so a second Tick (or Run's own next
// tick) arriving while one is in flight simply blocks on driveMu.Lock()
// until this call's cycles have finished reporting, then proceeds exactly
// as if the two had been called back to back on one goroutine. No cycle
// is ever dropped or skipped because of a concurrent caller -- the
// tryStart guard (below) is what skips a repo already mid-cycle, and that
// is unaffected by driveMu.
//
// This did not always hold: both paths drive the same sync.WaitGroup, and
// before driveMu existed a second Wait call arriving before a prior one
// had returned panicked ("sync: WaitGroup is reused before previous Wait
// has returned") -- reproducible on demand with concurrent Tick calls, or
// with Tick concurrent with Run (loam-f75). driveMu closes that hole at
// its root, inside Scheduler itself, rather than relying on every current
// and future caller to remember never to hold a Scheduler value somewhere
// both Tick and Run are reachable from. Callers that need manual,
// deterministic ticks (docs/testing-spec.md's "Manual scheduler") still
// should not also start Run on that Scheduler -- Run would keep injecting
// wall-clock cycles a test never asked for -- but doing so now serializes
// instead of corrupting scheduler state.
//
// Tick does not return early on ctx cancellation: waitIdle's wait for
// every in-flight cycle is unconditional (sync.WaitGroup.Wait takes no
// ctx), and this error return does not change that -- it surfaces a
// ListRepos failure, not a path to abort the wait. A collaborator that
// wedges and ignores ctx still hangs Tick forever, and now also blocks
// every other Tick/Run tick waiting on driveMu behind it; that trade is
// unchanged in kind by this method's signature (see internal/testsched's
// SyncHarness.Tick doc comment for why that is deliberate) but is now
// visible to concurrent callers too, not just to Tick's own caller.
func (s *Scheduler) Tick(ctx context.Context) ([]RepoID, error) {
	s.driveMu.Lock()
	defer s.driveMu.Unlock()
	started, err := s.tick(ctx)
	s.waitIdle()
	return started, err
}

// tryStart claims repo for a new cycle, returning false if a previous
// cycle for repo is still running.
func (s *Scheduler) tryStart(repo RepoID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[repo]; ok {
		return false
	}
	s.running[repo] = struct{}{}
	return true
}

// finish releases repo's in-flight guard, allowing a later tick to start
// a new cycle for it.
func (s *Scheduler) finish(repo RepoID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, repo)
}

// cycle runs one repo's Mirror Sync cycle and reports its outcome. The
// in-flight guard is released via finish only after the outcome has been
// reported, never before: releasing early would let a later tick's
// ReportSyncing for the same repo land before this cycle's ReportIdle or
// ReportError does, inverting the outcome order a reporter observes (a
// stale idle or error landing on top of a cycle that is, in reality,
// already running again). Reporting failures are logged but never abort
// or retry this cycle — retrying is the next tick's job, not a report's.
//
// If s.sem is non-nil (WithMaxConcurrentCycles was passed to New), this
// goroutine blocks here, before doing any work, until a slot is free, and
// releases it via a deferred receive so the slot frees the instant this
// cycle's terminal report has been sent — the same "queued, not skipped"
// property tick's own goroutine-per-repo spawn already gives every
// enrolled repo, just bounded now across all of them combined.
func (s *Scheduler) cycle(ctx context.Context, repo RepoID) {
	defer s.wg.Done()
	if s.sem != nil {
		s.sem <- struct{}{}
		defer func() { <-s.sem }()
	}
	if err := s.state.ReportSyncing(ctx, repo); err != nil {
		s.logger.Error("reporting sync syncing", "repo", string(repo), "error", err)
	}
	enqueuedIngest, err := s.runSteps(ctx, repo)
	if err != nil {
		s.logger.Error("sync cycle step failed", "repo", string(repo), "error", err)
		if rerr := s.state.ReportError(ctx, repo, err, enqueuedIngest); rerr != nil {
			s.logger.Error("reporting sync error", "repo", string(repo), "error", rerr)
		}
		s.finish(repo)
		return
	}
	if rerr := s.state.ReportIdle(ctx, repo, enqueuedIngest); rerr != nil {
		s.logger.Error("reporting sync idle", "repo", string(repo), "error", rerr)
	}
	s.finish(repo)
}

// runSteps runs the fixed 5-step Mirror Sync order for repo in order
// (docs/sync-spec.md -> Mirror Sync, steps 1-5), returning whether step 4
// actually enqueued an ingest job (IngestEnqueuer.EnqueueIngest's own
// enqueued return value, never synthesised here — loam-ax1) and the first
// step's wrapped error, if any. Step 4's enqueued is propagated verbatim
// even when EnqueueIngest itself errors: per IngestEnqueuer's doc comment,
// a partial multi-branch failure can legitimately enqueue one branch and
// then fail on another, and that ownership hand-off must still reach
// ReportError rather than being coerced to false just because err is
// non-nil. A failing step aborts only repo's remaining steps for this
// tick; it never retries within the cycle — the next tick is the retry,
// and the tick interval is the backoff.
func (s *Scheduler) runSteps(ctx context.Context, repo RepoID) (enqueuedIngest bool, err error) {
	fetched, err := s.fetcher.Fetch(ctx, repo)
	if err != nil {
		return false, fmt.Errorf("fetching repo %s: %w", repo, err)
	}
	advanced, err := s.advances.DetectAdvances(ctx, repo, fetched)
	if err != nil {
		return false, fmt.Errorf("detecting advances for repo %s: %w", repo, err)
	}
	if err := s.mergeability.CheckMergeability(ctx, repo, advanced); err != nil {
		return false, fmt.Errorf("checking mergeability for repo %s: %w", repo, err)
	}
	enqueued, err := s.ingest.EnqueueIngest(ctx, repo, advanced)
	if err != nil {
		return enqueued, fmt.Errorf("enqueuing ingest for repo %s: %w", repo, err)
	}
	if err := s.prPoller.PollPRs(ctx, repo); err != nil {
		return enqueued, fmt.Errorf("polling PRs for repo %s: %w", repo, err)
	}
	return enqueued, nil
}
