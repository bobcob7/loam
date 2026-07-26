package testsched

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bobcob7/loam/internal/mirrorsync"
)

// SyncHarness drives a mirrorsync.Scheduler by explicit ticks and gives a
// test a hard happens-before on the result, per docs/testing-spec.md's
// "Manual scheduler" -- see the package doc for how, and why the
// scheduler's own exported surface (New, Run) is not enough on its own.
//
// Construct one with NewSyncHarness, wrapping the same RepoLister and
// SyncStateReporter the real scheduler wiring would otherwise use
// directly:
//
//	h := testsched.NewSyncHarness(repoLister, syncStateReporter)
//	scheduler := mirrorsync.New(logger, h.Ticks(), h.RepoLister(), fetcher,
//		advanceDetector, mergeabilityChecker, ingestEnqueuer, prPoller,
//		h.SyncStateReporter())
//	go scheduler.Run(ctx) // as production's main() does
//
// Then, once per "the next sync runs" / "the upstream PR merges" step:
//
//	repos, err := h.Tick(ctx)
//
// Tick returns only once every repo the tick actually started has
// reported a terminal outcome (idle or error) through the wrapped
// SyncStateReporter -- never a loop, never Eventually.
//
// One SyncHarness drives one Scheduler for one scenario's lifetime; per
// docs/testing-spec.md ("no shared state between scenarios"), construct a
// fresh SyncHarness (and Scheduler) per scenario, and never call Tick
// concurrently on the same SyncHarness -- Tick serializes internally, but
// a concurrent second call simply queues behind the first rather than
// observing its own tick. If a Tick call's ctx is canceled mid-flight
// (after the tick was sent but before every repo's outcome was observed),
// treat the harness as spent and construct a fresh one rather than
// issuing another Tick on it: the in-flight cycle's eventual report can
// still land against this harness's internal state after Tick has
// already returned.
type SyncHarness struct {
	mu       sync.Mutex
	ticks    chan time.Time
	repos    *observingRepoLister
	reporter *observingStateReporter
}

// NewSyncHarness builds a SyncHarness wrapping repos and state. Pass the
// harness's RepoLister, SyncStateReporter, and Ticks -- not repos and
// state directly -- into mirrorsync.New.
func NewSyncHarness(repos repoLister, state syncStateReporter) *SyncHarness {
	shared := &pendingSet{waiters: make(map[mirrorsync.RepoID]chan struct{})}
	return &SyncHarness{
		ticks:    make(chan time.Time),
		repos:    &observingRepoLister{inner: repos, pending: shared, snapshots: make(chan []mirrorsync.RepoID, 1)},
		reporter: &observingStateReporter{inner: state, pending: shared},
	}
}

// RepoLister returns the wrapped RepoLister to pass into mirrorsync.New.
func (h *SyncHarness) RepoLister() mirrorsync.RepoLister { return h.repos }

// SyncStateReporter returns the wrapped SyncStateReporter to pass into
// mirrorsync.New.
func (h *SyncHarness) SyncStateReporter() mirrorsync.SyncStateReporter { return h.reporter }

// Ticks returns the channel to pass into mirrorsync.New as its tick
// source. Only Tick writes to it; do not write to it directly, or Tick
// loses its ability to correlate a send with the resulting repo set.
func (h *SyncHarness) Ticks() <-chan time.Time { return h.ticks }

// Tick sends one tick to the scheduler and blocks until every repo that
// tick started has reported a terminal outcome, returning the repos the
// tick actually started (a repo already mid-cycle from a prior tick is
// skipped by the scheduler's own per-repo guard and will not appear
// here). It returns ctx's error if ctx is done before that happens, e.g.
// because Scheduler.Run was never started or exited early.
func (h *SyncHarness) Tick(ctx context.Context) ([]mirrorsync.RepoID, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case h.ticks <- time.Now():
	case <-ctx.Done():
		return nil, fmt.Errorf("sending tick: %w", ctx.Err())
	}
	var repos []mirrorsync.RepoID
	select {
	case repos = <-h.repos.snapshots:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the scheduler to list repos: %w", ctx.Err())
	}
	for _, repo := range repos {
		waiter := h.reporter.pending.waiterFor(repo)
		select {
		case <-waiter:
		case <-ctx.Done():
			return repos, fmt.Errorf("waiting for repo %s to report a terminal outcome: %w", repo, ctx.Err())
		}
	}
	return repos, nil
}

// TickT is Tick for callers with a testing.TB in scope: any failure fails
// tb directly instead of returning an error.
func (h *SyncHarness) TickT(ctx context.Context, tb testing.TB) []mirrorsync.RepoID {
	tb.Helper()
	repos, err := h.Tick(ctx)
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return repos
}

// pendingSet tracks, per repo, a channel closed exactly once by that
// repo's next terminal report. observingRepoLister creates a repo's entry
// synchronously inside ListRepos, before the scheduler can spawn that
// repo's cycle goroutine; observingStateReporter closes and removes it
// when the matching terminal report arrives.
type pendingSet struct {
	mu      sync.Mutex
	waiters map[mirrorsync.RepoID]chan struct{}
}

// register creates a fresh waiter for repo, replacing any leftover entry
// from an earlier tick.
func (p *pendingSet) register(repo mirrorsync.RepoID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waiters[repo] = make(chan struct{})
}

// waiterFor returns repo's current waiter channel, or an already-closed
// one if repo was never registered (defensive: a repo the scheduler
// reports on outside of any tick this harness sent should never block a
// caller forever).
func (p *pendingSet) waiterFor(repo mirrorsync.RepoID) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch, ok := p.waiters[repo]; ok {
		return ch
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

// signal closes and removes repo's current waiter, if any.
func (p *pendingSet) signal(repo mirrorsync.RepoID) {
	p.mu.Lock()
	ch, ok := p.waiters[repo]
	if ok {
		delete(p.waiters, repo)
	}
	p.mu.Unlock()
	if ok {
		close(ch)
	}
}

// observingRepoLister wraps a repoLister, registering a pending waiter
// for every repo in each ListRepos result before returning it -- the
// synchronous hook SyncHarness.Tick relies on, since it runs on the
// scheduler's own goroutine, strictly before any per-repo cycle can start.
type observingRepoLister struct {
	inner     repoLister
	pending   *pendingSet
	snapshots chan []mirrorsync.RepoID
}

func (o *observingRepoLister) ListRepos(ctx context.Context) ([]mirrorsync.RepoID, error) {
	repos, err := o.inner.ListRepos(ctx)
	if err != nil {
		return repos, err
	}
	for _, repo := range repos {
		o.pending.register(repo)
	}
	snapshot := make([]mirrorsync.RepoID, len(repos))
	copy(snapshot, repos)
	select {
	case o.snapshots <- snapshot:
	case <-ctx.Done():
	}
	return repos, nil
}

// observingStateReporter wraps a syncStateReporter, forwarding every call
// to inner unchanged and additionally signaling pending on a terminal
// report (ReportIdle or ReportError). ReportSyncing is forwarded only --
// it is not the signal SyncHarness.Tick waits on, since a cycle's outcome
// (idle or error), not merely its start, is what "one tick, run to
// completion" means.
type observingStateReporter struct {
	inner   syncStateReporter
	pending *pendingSet
}

func (o *observingStateReporter) ReportSyncing(ctx context.Context, repo mirrorsync.RepoID) error {
	return o.inner.ReportSyncing(ctx, repo)
}

func (o *observingStateReporter) ReportIdle(ctx context.Context, repo mirrorsync.RepoID, enqueuedIngest bool) error {
	err := o.inner.ReportIdle(ctx, repo, enqueuedIngest)
	o.pending.signal(repo)
	return err
}

func (o *observingStateReporter) ReportError(ctx context.Context, repo mirrorsync.RepoID, cycleErr error, enqueuedIngest bool) error {
	err := o.inner.ReportError(ctx, repo, cycleErr, enqueuedIngest)
	o.pending.signal(repo)
	return err
}
