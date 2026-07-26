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
func (s *Scheduler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-s.ticks:
			if !ok {
				return
			}
			s.tick(ctx)
		}
	}
}

// tick lists the enrolled repos, starts a cycle for each one that is not
// already running, and returns the repos it actually started this call.
// Listing and the per-repo in-flight guard both run synchronously here,
// before any per-repo goroutine is spawned, so a second tick arriving
// while a repo's cycle is still in flight reliably observes that repo as
// running and skips it — a property the return value lets callers (and
// tests) assert on directly, without inferring it from goroutine timing.
func (s *Scheduler) tick(ctx context.Context) []RepoID {
	repos, err := s.repos.ListRepos(ctx)
	if err != nil {
		s.logger.Error("listing enrolled repos", "error", err)
		return nil
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
	return started
}

// waitIdle blocks until every cycle started so far has finished. It
// exists for tests: a hard happens-before barrier so assertions after a
// tick never race the goroutines it started, without sleeping or
// polling (docs/testing-spec.md -> Manual scheduler).
func (s *Scheduler) waitIdle() {
	s.wg.Wait()
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
func (s *Scheduler) Tick(ctx context.Context) []RepoID {
	started := s.tick(ctx)
	s.waitIdle()
	return started
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
// enqueued an ingest job and the first step's wrapped error, if any. A
// failing step aborts only repo's remaining steps for this tick; it
// never retries within the cycle — the next tick is the retry, and the
// tick interval is the backoff.
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
	if err := s.ingest.EnqueueIngest(ctx, repo, advanced); err != nil {
		return false, fmt.Errorf("enqueuing ingest for repo %s: %w", repo, err)
	}
	if err := s.prPoller.PollPRs(ctx, repo); err != nil {
		return true, fmt.Errorf("polling PRs for repo %s: %w", repo, err)
	}
	return true, nil
}
