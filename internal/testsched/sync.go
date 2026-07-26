package testsched

import (
	"context"

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
//	repos := h.Tick(ctx)
//
// Do NOT also call scheduler.Run: Tick drives a cycle directly, so the
// injected tick channel goes unused in tests. Production wires
// time.NewTicker and calls Run as usual; the two paths are alternatives,
// not layers.
//
// Tick returns only once every cycle in flight has finished — including
// any still running from an earlier tick — and never sleeps or polls.
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
// own per-repo guard and does not appear here).
//
// It takes no lock and returns no error. Serialization is the scheduler's
// job — mirrorsync.Scheduler.tick claims each repo under its own mutex
// before spawning anything — and there is no failure this layer can
// observe: mirrorsync logs and swallows a ListRepos error, returning no
// started repos, so a listing failure is indistinguishable here from an
// empty enrollment. See loam-hhh for surfacing that distinction; until it
// lands, a step asserting on an empty result should be read as "no repo
// started", not "no repo is enrolled".
func (h *SyncHarness) Tick(ctx context.Context) []mirrorsync.RepoID {
	return h.scheduler.Tick(ctx)
}
