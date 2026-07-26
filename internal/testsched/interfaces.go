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
// until repo has no queued or running ingest_jobs row left.
//
// repo is a plain string (repos.name, an "owner/repo" identifier), not
// mirrorsync.RepoID, deliberately: internal/ingest keys its production
// seam (Enqueue, Job) on repos.id (uuid.UUID, the ingest_jobs.repo_id FK
// column) since every real caller already holds it, and exposes this
// harness-facing method as a separate DrainRepo(ctx, name string) that
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
