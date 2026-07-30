package testsched

import (
	"context"
	"testing"

	"github.com/bobcob7/loam/internal/mirrorsync"
)

// SyncHarness drives a mirrorsync.Scheduler by explicit ticks, per
// docs/testing-spec.md's "Manual scheduler". It is a thin, honest wrapper
// over mirrorsync.Scheduler.Tick: the scheduler itself owns the
// happens-before, and this type exists to give step definitions a name for
// it rather than to add behaviour.
//
// Construct one around an already-wired scheduler:
//
//	scheduler := mirrorsync.New(logger, ticks, repoLister, fetcher,
//		advanceDetector, mergeabilityChecker, ingestEnqueuer, prPoller,
//		syncStateReporter)
//	h := testsched.NewSyncHarness(scheduler)
//
// Then, once per "the next sync runs" / "the upstream PR merges" step:
//
//	repos, err := h.Tick(ctx)
//
// or, from a Go test with a testing.TB in scope:
//
//	repos := h.TickT(ctx, t)
//
// Do NOT also call scheduler.Run: Tick drives a cycle directly, so the
// injected tick channel goes unused in tests. Production wires
// time.NewTicker and calls Run as usual; the two paths are alternatives,
// not layers.
//
// Tick returns only once every cycle in flight has finished — including
// any still running from an earlier tick — and never sleeps or polls.
//
// Calling Tick concurrently with another Tick, or with Run, on the same
// Scheduler no longer panics (mirrorsync.Scheduler serializes the two via
// an internal driveMu, added for loam-f75): a second call simply blocks
// until the first's cycles have finished reporting. Godog steps and Go
// tests both call Tick sequentially regardless, so this costs nothing in
// practice, but a caller that got it wrong now sees a stall, not a crash.
//
// Tick also never returns early on ctx cancellation: it passes ctx to
// the scheduler's collaborators, but the wait for every in-flight cycle
// to finish is unconditional (sync.WaitGroup.Wait takes no ctx). A
// collaborator that wedges and ignores ctx -- a stuck fake forge, for
// instance -- hangs Tick forever, with no per-call deadline able to
// unstick it. That is a deliberate trade, not an oversight: a hang from
// a wedged collaborator is a real bug worth surfacing loudly, not one to
// paper over with a timeout that would just turn it into a slow, flaky
// pass. See docs/testing-spec.md's "Manual scheduler" for why a bounded
// wait would be the wrong instinct here.
//
// One SyncHarness drives one Scheduler for one scenario's lifetime, per
// docs/testing-spec.md ("no shared state between scenarios"). Unlike the
// decorator-based harness this replaced, a canceled Tick leaves no stale
// internal state, so a harness is not spent by one: mirrorsync.Scheduler
// holds the only state that matters.
type SyncHarness struct {
	scheduler *mirrorsync.Scheduler
}

// NewSyncHarness builds a SyncHarness driving scheduler.
func NewSyncHarness(scheduler *mirrorsync.Scheduler) *SyncHarness {
	return &SyncHarness{scheduler: scheduler}
}

// Tick runs one cycle per enrolled repo and blocks until every cycle in
// flight has finished, returning the repos this call actually started (a
// repo already mid-cycle from a prior tick is skipped by the scheduler's
// own per-repo guard and does not appear here) alongside a ListRepos
// failure, if one occurred. See the SyncHarness doc comment above for the
// two constraints that come with this: no concurrent Tick/Run on the same
// Scheduler, and no early return on ctx cancellation.
//
// It takes no lock of its own; the error is mirrorsync.Scheduler.Tick's
// own, propagated unchanged (loam-hhh), so a step that ticks and gets an
// empty, nil-error result can now trust "no repo is enrolled" instead of
// having to treat it as "no repo started, cause unknown". Before loam-hhh,
// mirrorsync logged and swallowed a ListRepos error, returning no started
// repos with nothing to distinguish that from an empty enrollment; that
// gap is why TickT was dropped in wave 4 (there was no error to fail a
// testing.TB on) and is restored below now that there is one.
func (h *SyncHarness) Tick(ctx context.Context) ([]mirrorsync.RepoID, error) {
	return h.scheduler.Tick(ctx)
}

// TickT is Tick for callers with a testing.TB in scope: a ListRepos
// failure fails tb directly instead of returning an error a Go test would
// otherwise have to check itself.
func (h *SyncHarness) TickT(ctx context.Context, tb testing.TB) []mirrorsync.RepoID {
	tb.Helper()
	repos, err := h.Tick(ctx)
	if err != nil {
		tb.Fatalf("%v", err)
	}
	return repos
}
