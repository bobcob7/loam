// Package testsched drives the sync scheduler (internal/mirrorsync) and
// the ingest worker pool (loam-c94.1) by explicit, harness-controlled
// ticks instead of timers or polling, per docs/testing-spec.md's
// "Deterministic time" principle and its "Manual scheduler" test double.
// Neither helper below sleeps or polls, but they wait differently:
// SyncHarness.Tick's wait is a sync.WaitGroup.Wait inside mirrorsync
// (see mirrorsync.Scheduler.Tick); IngestHarness.DrainIngestQueue's wait
// is a blocking call on the injected IngestDrainer, whatever primitive
// loam-c94.1 backs it with.
//
// Two independent helpers, because sync and ingest are different
// concurrency shapes:
//
//   - SyncHarness.Tick runs mirrorsync's per-repo cycle to completion,
//     in-line, for every repo currently enrolled, and only returns once
//     every cycle in flight -- this call's and any earlier one's -- has
//     finished. That is stronger than "reported a terminal outcome": see
//     SyncHarness's own doc comment for why the distinction is exactly
//     what the original design mistake in this package got wrong.
//   - IngestHarness.DrainIngestQueue blocks until the async, in-process
//     ingest worker pool has no queued or running job left for a repo.
//
// Both are safe to use from godog step definitions, which have the
// signature func(context.Context, ...) (context.Context, error) and no
// testing.TB in scope (see internal/testfixture for the precedent this
// package follows). Only IngestHarness follows testfixture's full
// error-returning-primitive-plus-NewT-sugar pattern: DrainIngestQueue
// returns a plain error, with DrainIngestQueueT as the testing.TB
// convenience layer. SyncHarness.Tick returns no error at all -- there is
// no failure this layer can observe, since mirrorsync itself swallows a
// ListRepos error (see Tick's doc comment) -- so it has no *T sugar to
// layer on.
//
// # The mirrorsync seam, and why it lives in mirrorsync
//
// SyncHarness is a thin wrapper over mirrorsync.Scheduler.Tick, which
// combines the scheduler's own unexported tick and waitIdle. That barrier
// has to live inside internal/mirrorsync; it cannot be assembled from
// outside, and an earlier version of this package that tried could hang.
//
// The reason is the ordering around the per-repo in-flight guard. A cycle
// makes its terminal report (ReportIdle or ReportError) and only then
// calls finish to release the guard. A decorator built on the exported
// RepoLister and SyncStateReporter can observe the report, but nothing
// after it -- so it can return while the repo is still in the scheduler's
// running map. The next tick then reaches tryStart first, skips the repo,
// spawns no cycle, and any waiter the decorator registered for it is
// never closed. Driving the real scheduler through back-to-back ticks
// reproduced this 4 times out of 4, at roughly one hang per 10k-50k
// ticks. A hang, not a failed assertion: in godog it surfaces as a step
// deadline or "panic: test timed out", the worst possible shape.
//
// There is no decorator-only fix. A skipped repo is indistinguishable
// from "no ReportSyncing this tick", and absence is not observable in
// bounded time without polling -- the sleep-in-disguise this package
// exists to abolish. finish has no observable hook outside wg.Wait.
//
// Scheduler.Tick is correct by construction instead: cycle's deferred
// wg.Done fires after finish, so waiting on the WaitGroup is a strictly
// stronger barrier than any external observer of the terminal report can
// build. Tests call Tick directly and leave Run to production.
//
// # The ingest seam, agreed with loam-c94.1 but not yet visible here
//
// loam-c94.1 (in flight on a sibling branch this package cannot see) owns
// the ingest_jobs worker pool: an Enqueuer seam, RequeueOrphaned for
// startup recovery, and jobs keyed by repos.id rather than a repo name.
// Without a synchronous per-repo drain/wait, a caller would have no way
// to learn "no queued or running ingest_jobs row remains for repo X"
// short of polling Postgres -- exactly the timing-guess pattern
// docs/testing-spec.md forbids. IngestDrainer below is the seam this
// package asked loam-c94.1 for and the coordinator confirmed it landed
// as DrainRepo(ctx, name string) error, matching this interface's shape;
// internal/ingest on this branch still contains only embed/, so that
// confirmation is relayed, not something this package's own tests
// exercise against the real pool. IngestDrainer's doc comment below
// records the agreed contract for when it becomes reachable.
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
// until repo has no queued or running ingest_jobs row left. This is the
// agreed contract with loam-c94.1 (see the package doc's "ingest seam"
// section for how confirmed) -- as of this package's own tests,
// IngestDrainer is only satisfied by a moq mock, not by loam-c94.1's
// pool directly.
//
// repo is a plain string (repos.name, an "owner/repo" identifier), not
// mirrorsync.RepoID, deliberately: loam-c94.1's production seam
// (Enqueue, Job) is keyed on repos.id (uuid.UUID, the ingest_jobs.repo_id
// FK column), since every real caller already holds it, with this
// harness-facing method a separate DrainRepo(ctx, name string) that
// resolves name to id internally. Typing this interface's parameter as
// mirrorsync.RepoID (or uuid.UUID) would either force internal/ingest to
// import internal/mirrorsync for a single string-newtype, or force this
// package to import a uuid dependency it otherwise has no need for;
// plain string avoids both. IngestHarness.DrainIngestQueue still takes a
// mirrorsync.RepoID from callers -- the right vocabulary for a godog step
// -- and converts at this boundary.
type IngestDrainer interface {
	// DrainRepo blocks until repo has no queued or running ingest job,
	// including any job coalesced in while a prior one was running, or
	// until ctx is done.
	DrainRepo(ctx context.Context, repo string) error
}
