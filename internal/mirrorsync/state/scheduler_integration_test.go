//go:build integration

// This file proves the composition loam-ax1 was filed over: that a real
// internal/mirrorsync.Scheduler wired to a real state.Reporter (this
// package, backed by real Postgres) actually reaches sync_state='idle' with
// last_synced_at set when nothing was enqueued, and correctly leaves the row
// at 'syncing' when ownership passed to the ingest worker. Without this
// file, the fix was only ever proven in halves that never meet: this
// package's own reporter_test.go/integration_test.go call Reporter's methods
// directly with a hand-picked enqueuedIngest bool, and
// internal/mirrorsync/scheduler_test.go proves the scheduler forwards
// IngestEnqueuer's answer to a moq SyncStateReporterMock -- neither test
// wires the real Scheduler to the real Reporter, so a regression in either
// seam (e.g. runSteps synthesising a value again, or Reporter's guard
// condition inverting) could pass both suites and still leave
// repos.sync_state stuck, exactly as loam-ax1's original bug did.
//
// No import cycle: internal/mirrorsync does not import
// internal/mirrorsync/state (state imports mirrorsync, not the reverse), so
// constructing a real mirrorsync.Scheduler here is safe.
package state

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorsync"
)

// testLoggerForScheduler returns a discard logger for the real
// mirrorsync.Scheduler this file constructs -- reporter.go itself takes no
// logger, so this package has no existing test helper for one.
func testLoggerForScheduler() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// noopFetcher, noopAdvanceDetector, noopMergeabilityChecker, and
// noopPRPoller are trivial, inert stand-ins for the four Scheduler
// collaborators this test does not exercise -- they exist only because
// mirrorsync.New needs a concrete value for each, mirroring
// internal/testsched/sync_realscheduler_test.go's rationale for
// hand-writing rather than moq-generating them (source and destination
// packages differ, and this package's own logic does not depend on them).
type noopFetcher struct{}

func (noopFetcher) Fetch(context.Context, mirrorsync.RepoID) (mirrorsync.FetchResult, error) {
	return mirrorsync.FetchResult{}, nil
}

type noopAdvanceDetector struct{}

func (noopAdvanceDetector) DetectAdvances(context.Context, mirrorsync.RepoID, mirrorsync.FetchResult) ([]mirrorsync.Advance, error) {
	return nil, nil
}

type noopMergeabilityChecker struct{}

func (noopMergeabilityChecker) CheckMergeability(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) error {
	return nil
}

type noopPRPoller struct{}

func (noopPRPoller) PollPRs(context.Context, mirrorsync.RepoID) error { return nil }

// stubIngestEnqueuer answers every EnqueueIngest call with the fixed
// (enqueued, err) this test configures, so each test can drive the
// scheduler down the "nothing enqueued" or "genuinely enqueued" path
// deterministically without a real internal/ingest.Pool.
type stubIngestEnqueuer struct {
	enqueued bool
}

func (s stubIngestEnqueuer) EnqueueIngest(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) (bool, error) {
	return s.enqueued, nil
}

// staticRepoLister always lists the fixed set of repos it was built with.
type staticRepoLister struct {
	repos []mirrorsync.RepoID
}

func (s staticRepoLister) ListRepos(context.Context) ([]mirrorsync.RepoID, error) {
	return s.repos, nil
}

// newIntegrationScheduler builds a real mirrorsync.Scheduler over repo,
// backed by this package's real Reporter (pool), with every collaborator
// but IngestEnqueuer wired to an inert no-op -- enqueued controls the one
// signal this test suite actually varies.
func newIntegrationScheduler(reporter mirrorsync.SyncStateReporter, repo mirrorsync.RepoID, enqueued bool) *mirrorsync.Scheduler {
	lister := staticRepoLister{repos: []mirrorsync.RepoID{repo}}
	return mirrorsync.New(
		testLoggerForScheduler(),
		nil, // Tick never reads the tick channel; only Run does.
		lister,
		noopFetcher{},
		noopAdvanceDetector{},
		noopMergeabilityChecker{},
		stubIngestEnqueuer{enqueued: enqueued},
		noopPRPoller{},
		reporter,
	)
}

// TestSchedulerIntegration_NoAdvancesReachesIdleWithLastSyncedAt is loam-
// ax1's acceptance criterion proven end to end: a healthy repo with nothing
// to enqueue must reach sync_state='idle' with last_synced_at set, driven
// through the real Scheduler.Tick -> real Reporter.ReportIdle path against
// real Postgres, not a mock on either side.
func TestSchedulerIntegration_NoAdvancesReachesIdleWithLastSyncedAt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/integration-idle-repo"
	insertRepo(ctx, t, pool, name)
	reporter := New(pool)
	scheduler := newIntegrationScheduler(reporter, mirrorsync.RepoID(name), false)
	repos := scheduler.Tick(ctx)
	require.Equal(t, []mirrorsync.RepoID{mirrorsync.RepoID(name)}, repos)
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "idle", row.state, "nothing was enqueued, so the scheduler's own ReportIdle must have written the terminal state")
	require.NotNil(t, row.lastSyncedAt)
	assert.NotEmpty(t, *row.lastSyncedAt)
}

// TestSchedulerIntegration_EnqueuedIngestLeavesRowSyncing is the mirror
// case: when IngestEnqueuer reports a genuine enqueue, the real Scheduler
// must still call ReportIdle (it always reports a terminal outcome), but
// the real Reporter's ownership-transfer guard must leave the row exactly
// where ReportSyncing put it -- proving the composed pair does not clobber
// the column the (still-hypothetical, in this test) ingest worker now
// owns.
func TestSchedulerIntegration_EnqueuedIngestLeavesRowSyncing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/integration-handoff-repo"
	insertRepo(ctx, t, pool, name)
	reporter := New(pool)
	scheduler := newIntegrationScheduler(reporter, mirrorsync.RepoID(name), true)
	repos := scheduler.Tick(ctx)
	require.Equal(t, []mirrorsync.RepoID{mirrorsync.RepoID(name)}, repos)
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "syncing", row.state, "ownership passed to the ingest worker, so the composed pair must not have written idle")
	assert.Nil(t, row.lastSyncedAt)
}

// TestSchedulerIntegration_EnqueueErrorLeavesRowInErrorState proves the
// composed error path: a real step-4 failure with enqueued=false must
// reach the real Reporter's ReportError and land the row in 'error' with
// the message, exactly as a scheduler-only mock test can only simulate.
func TestSchedulerIntegration_EnqueueErrorLeavesRowInErrorState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const name = "group/integration-error-repo"
	insertRepo(ctx, t, pool, name)
	reporter := New(pool)
	wantErr := errors.New("enqueuing ingest job: connection refused")
	lister := staticRepoLister{repos: []mirrorsync.RepoID{mirrorsync.RepoID(name)}}
	scheduler := mirrorsync.New(
		testLoggerForScheduler(),
		nil,
		lister,
		noopFetcher{},
		noopAdvanceDetector{},
		noopMergeabilityChecker{},
		erroringIngestEnqueuer{err: wantErr},
		noopPRPoller{},
		reporter,
	)
	repos := scheduler.Tick(ctx)
	require.Equal(t, []mirrorsync.RepoID{mirrorsync.RepoID(name)}, repos)
	row := readSyncRow(ctx, t, pool, name)
	assert.Equal(t, "error", row.state)
	require.NotNil(t, row.syncError)
	assert.Contains(t, *row.syncError, wantErr.Error())
}

// erroringIngestEnqueuer always fails with err and reports enqueued=false,
// so TestSchedulerIntegration_EnqueueErrorLeavesRowInErrorState can drive
// the scheduler down step 4's error path deterministically.
type erroringIngestEnqueuer struct{ err error }

func (e erroringIngestEnqueuer) EnqueueIngest(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) (bool, error) {
	return false, e.err
}
