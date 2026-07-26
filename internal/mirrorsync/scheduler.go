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
}

// New builds a Scheduler. ticks is the trigger seam: production passes
// time.NewTicker(LOAM_SYNC_INTERVAL).C; tests pass a channel they write
// to explicitly, so cycles run to completion on an explicit tick rather
// than a wall-clock timer (docs/testing-spec.md -> Manual scheduler).
func New(logger *slog.Logger, ticks <-chan time.Time, repos RepoLister, fetcher Fetcher, advances AdvanceDetector, mergeability MergeabilityChecker, ingest IngestEnqueuer, prPoller PRPoller, state SyncStateReporter) *Scheduler {
	return &Scheduler{
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
}

// Run blocks, starting one cycle per enrolled repo on every received
// tick, until ctx is canceled or the tick channel is closed. Each repo's
// cycle runs in its own goroutine so a slow or stuck repo never blocks
// another repo's cycle; Run itself only ever blocks on the tick source.
// A ListRepos failure is logged and the loop continues to the next tick —
// production's retry is simply trying again later, never stopping the
// process over a transient listing failure (loam-hhh). Tick, below,
// propagates that same failure instead, since a manual-tick caller has no
// "next tick" to fall back on and needs to tell it apart from an empty
// enrollment.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-s.ticks:
			if !ok {
				return
			}
			if _, err := s.tick(ctx); err != nil {
				s.logger.Error("tick failed", "error", err)
			}
		}
	}
}

// tick lists the enrolled repos, starts a cycle for each one that is not
// already running, and returns the repos it actually started this call
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
// Do not call Tick concurrently with another Tick, or with Run, on the
// same Scheduler: both paths drive the same sync.WaitGroup, and a second
// Wait call arriving before a prior one has returned panics ("sync:
// WaitGroup is reused before previous Wait has returned"). Callers that
// need manual, deterministic ticks (docs/testing-spec.md's "Manual
// scheduler") should call Tick on its own and never start Run on that
// Scheduler at all.
//
// Tick does not return early on ctx cancellation: waitIdle's wait for
// every in-flight cycle is unconditional (sync.WaitGroup.Wait takes no
// ctx), and this error return does not change that -- it surfaces a
// ListRepos failure, not a path to abort the wait. A collaborator that
// wedges and ignores ctx still hangs Tick forever; that trade is
// unchanged by this method's signature (see internal/testsched's
// SyncHarness.Tick doc comment for why that is deliberate).
func (s *Scheduler) Tick(ctx context.Context) ([]RepoID, error) {
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
func (s *Scheduler) cycle(ctx context.Context, repo RepoID) {
	defer s.wg.Done()
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
