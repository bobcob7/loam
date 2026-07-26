// Package testsched drives the sync scheduler (internal/mirrorsync) and
// the ingest worker pool (loam-c94.1) by explicit, harness-controlled
// ticks instead of timers or polling, per docs/testing-spec.md's
// "Deterministic time" principle and its "Manual scheduler" test double.
// Every wait this package performs is a blocking receive on a channel
// closed by the real event it is waiting for -- never a sleep, never an
// Eventually-style retry loop.
//
// Two independent helpers, because sync and ingest are different
// concurrency shapes:
//
//   - SyncHarness.Tick runs mirrorsync's per-repo cycle to completion,
//     in-line, for every repo the tick actually started, and only
//     returns once each has reported a terminal outcome (idle or error).
//   - IngestHarness.DrainIngestQueue blocks until the async, in-process
//     ingest worker pool has no queued or running job left for a repo.
//
// Both are safe to use from godog step definitions, which have the
// signature func(context.Context, ...) (context.Context, error) and no
// testing.TB in scope (see internal/testfixture for the precedent this
// package follows): the primitives below return a plain error. NewT-style
// sugar wraps them for callers that do have a testing.TB.
//
// # The mirrorsync seam this package works around
//
// mirrorsync.Scheduler exposes only New and Run; the unexported tick and
// waitIdle methods that give the scheduler itself a sleep-free
// happens-before are not reachable from another package. Run's loop reads
// one value from the injected tick channel, calls tick (which
// synchronously lists repos, claims each with tryStart, and spawns one
// goroutine per started repo), and returns to select without reporting
// anything back to the sender -- so a caller that only writes to the tick
// channel has no synchronous signal that the resulting cycles, let alone
// their outcomes, have landed. Naively pairing "write a tick" with "wait
// on some counter incremented inside a cycle's own goroutine" is exactly
// the sleep-in-disguise this package exists to avoid: the writer's send
// returning says only that Run's goroutine received the value, not that
// it has reached (or finished) the code that increments the counter.
//
// SyncHarness solves this without touching internal/mirrorsync, using two
// of the exported collaborator interfaces mirrorsync.New already accepts
// as constructor parameters -- RepoLister and SyncStateReporter -- which
// exist precisely so orchestration is decoupled from persistence. Wrap
// both with the harness's decorator (RepoLister/SyncStateReporter
// accessors below) before passing them into mirrorsync.New, alongside
// Ticks() as the tick channel:
//
//   - Scheduler.tick calls RepoLister.ListRepos exactly once, synchronously,
//     on Run's own goroutine, before spawning any per-repo goroutine. The
//     harness's wrapped ListRepos registers a pending waiter for every repo
//     in the result INSIDE that same synchronous call, before returning
//     control to tick -- so registration always happens-before any cycle
//     goroutine could possibly report, no matter how fast a fake
//     collaborator makes that cycle run.
//   - Each cycle's terminal report (ReportIdle or ReportError) closes that
//     repo's waiter channel.
//   - Tick sends one value on the tick channel, blocks for the RepoLister
//     wrapper's snapshot of the repos this tick started, then blocks on
//     each of their waiter channels in turn -- a real synchronization
//     primitive, not a poll.
//
// This assumes a fresh Scheduler driven by exactly one SyncHarness, ticked
// serially (never two Tick calls in flight at once) -- true for the
// godog "fresh instances per scenario" rule this package's consumers
// already follow. A cheaper, sturdier alternative for the same happens-
// before is an exported method on Scheduler itself combining the existing
// unexported tick and waitIdle, e.g. `func (s *Scheduler) Tick(ctx
// context.Context) []RepoID`; see the package's implementation report for
// the precise ask if that seam is ever added.
//
// # The ingest seam this package is missing
//
// loam-c94.1 (in flight; its DESIGN is the only visibility this package
// has into it) exposes an Enqueuer seam and RequeueOrphaned for startup
// recovery, but no synchronous per-repo drain/wait: nothing a caller can
// block on to learn "no queued or running ingest_jobs row remains for
// repo X" without polling Postgres, which is exactly the timing-guess
// pattern docs/testing-spec.md forbids. IngestDrainer below names the
// seam this package needs; it is not satisfied by anything in the tree
// today. See the implementation report for the precise ask.
package testsched

import (
	"context"

	"github.com/bobcob7/loam/internal/mirrorsync"
)

//go:generate go tool moq -out moq_test.go . repoLister syncStateReporter IngestDrainer

// repoLister mirrors mirrorsync.RepoLister's method set. It is declared
// locally, per this repo's "interfaces at the consumer" convention, so
// SyncHarness's own unit tests can drive it with a moq mock without
// depending on mirrorsync's (unexported, test-file-only) mocks. Any
// mirrorsync.RepoLister value satisfies it structurally.
type repoLister interface {
	ListRepos(ctx context.Context) ([]mirrorsync.RepoID, error)
}

// syncStateReporter mirrors mirrorsync.SyncStateReporter's method set, for
// the same reason as repoLister above.
type syncStateReporter interface {
	ReportSyncing(ctx context.Context, repo mirrorsync.RepoID) error
	ReportIdle(ctx context.Context, repo mirrorsync.RepoID, enqueuedIngest bool) error
	ReportError(ctx context.Context, repo mirrorsync.RepoID, err error, enqueuedIngest bool) error
}

// IngestDrainer is the synchronous per-repo drain primitive
// IngestHarness needs from the ingest worker pool (loam-c94.1): block
// until repo has no queued or running ingest_jobs row left. Nothing in
// the tree implements this today -- see this package's doc comment and
// the implementation report for what is being requested and why.
type IngestDrainer interface {
	// DrainRepo blocks until repo has no queued or running ingest job,
	// including any job coalesced in while a prior one was running, or
	// until ctx is done.
	DrainRepo(ctx context.Context, repo mirrorsync.RepoID) error
}
