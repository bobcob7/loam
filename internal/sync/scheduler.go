package sync

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

// tick lists the enrolled repos and starts a cycle for each one that is
// not already running. Listing and the per-repo in-flight guard both run
// synchronously here, before any per-repo goroutine is spawned, so a
// second tick arriving while a repo's cycle is still in flight reliably
// observes that repo as running and skips it.
func (s *Scheduler) tick(ctx context.Context) {
	repos, err := s.repos.ListRepos(ctx)
	if err != nil {
		s.logger.Error("listing enrolled repos", "error", err)
		return
	}
	for _, repo := range repos {
		if !s.tryStart(repo) {
			continue
		}
		go s.cycle(ctx, repo)
	}
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
// in-flight guard is released via finish before the outcome is reported,
// so a later tick is always free to start a new cycle for repo by the
// time this one's outcome has been observed.
func (s *Scheduler) cycle(ctx context.Context, repo RepoID) {
	s.state.ReportSyncing(ctx, repo)
	err := s.runSteps(ctx, repo)
	s.finish(repo)
	if err != nil {
		s.logger.Error("sync cycle step failed", "repo", string(repo), "error", err)
		s.state.ReportError(ctx, repo, err)
		return
	}
	s.state.ReportIdle(ctx, repo)
}

// runSteps runs the fixed 5-step Mirror Sync order for repo in order
// (docs/sync-spec.md -> Mirror Sync, steps 1-5), returning the first
// step's wrapped error. A failing step aborts only repo's remaining
// steps for this tick; it never retries within the cycle — the next
// tick is the retry, and the tick interval is the backoff.
func (s *Scheduler) runSteps(ctx context.Context, repo RepoID) error {
	if err := s.fetcher.Fetch(ctx, repo); err != nil {
		return fmt.Errorf("fetching repo %s: %w", repo, err)
	}
	advanced, err := s.advances.DetectAdvances(ctx, repo)
	if err != nil {
		return fmt.Errorf("detecting advances for repo %s: %w", repo, err)
	}
	if err := s.mergeability.CheckMergeability(ctx, repo, advanced); err != nil {
		return fmt.Errorf("checking mergeability for repo %s: %w", repo, err)
	}
	if err := s.ingest.EnqueueIngest(ctx, repo, advanced); err != nil {
		return fmt.Errorf("enqueuing ingest for repo %s: %w", repo, err)
	}
	if err := s.prPoller.PollPRs(ctx, repo); err != nil {
		return fmt.Errorf("polling PRs for repo %s: %w", repo, err)
	}
	return nil
}
