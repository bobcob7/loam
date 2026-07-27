//go:build integration

// This file covers the ingest half of repos.sync_state's two-writer
// contract (loam-c94.13): the transitions internal/ingest/pool.go's
// syncStateSyncingQuery block owns, as opposed to the sync-cycle half
// internal/mirrorsync/state's own tests cover. Same container/tag caveats
// as integration_test.go, whose newTestPool/seedRepo/fetchJob helpers
// these tests reuse.
//
// EVERY test here is written against loam-4q2's single-sample trap:
// sync_state cycles syncing -> idle/error -> syncing under a live pool
// exactly the way ingest_jobs.status does, so a bare sample of it taken
// from the test goroutine cannot distinguish "never transitioned" from
// "transitioned and currently mid-cycle". Each test below therefore does
// one of two things, and says which in its own doc comment: it samples
// the column from INSIDE Orchestrator.Run (where the job is running by
// construction, for the whole duration of the call), or it drives the
// pool to a state that is genuinely at rest and proves the rest with an
// assertion on a monotonic quantity.
package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoSyncRow is the repos-side triple these tests assert on.
type repoSyncRow struct {
	state        string
	syncError    *string
	lastSyncedAt *time.Time
}

func fetchRepoSync(ctx context.Context, t *testing.T, pgPool *pgxpool.Pool, repoID uuid.UUID) repoSyncRow {
	t.Helper()
	var row repoSyncRow
	require.NoError(t, pgPool.QueryRow(ctx,
		`SELECT sync_state, sync_error, last_synced_at FROM repos WHERE id = $1`, repoID,
	).Scan(&row.state, &row.syncError, &row.lastSyncedAt))
	return row
}

// setRepoSync overwrites a seeded repo's three sync columns, so a test can
// start from a state that is NOT the value it later asserts on -- without
// it, seedRepo's schema default ('idle', both nullables NULL) would make
// "the pool wrote idle" indistinguishable from "the pool wrote nothing".
func setRepoSync(ctx context.Context, t *testing.T, pgPool *pgxpool.Pool, repoID uuid.UUID, state string, syncErr *string, lastSyncedAt *time.Time) {
	t.Helper()
	_, err := pgPool.Exec(ctx,
		`UPDATE repos SET sync_state = $2, sync_error = $3, last_synced_at = $4 WHERE id = $1`,
		repoID, state, syncErr, lastSyncedAt)
	require.NoError(t, err)
}

// syncStateObserver is an Orchestrator that records repos.sync_state as
// read from inside its own Run call -- the only sampling point at which
// "this repo's ingest job is running" is true by construction rather than
// by timing -- and then returns the configured outcome.
type syncStateObserver struct {
	pgPool *pgxpool.Pool
	repoID uuid.UUID
	runErr error
	mu     sync.Mutex
	seen   []string
}

func (o *syncStateObserver) Run(ctx context.Context, _ Job) (Stats, error) {
	var state string
	err := o.pgPool.QueryRow(ctx, `SELECT sync_state FROM repos WHERE id = $1`, o.repoID).Scan(&state)
	o.mu.Lock()
	if err != nil {
		state = "read failed: " + err.Error()
	}
	o.seen = append(o.seen, state)
	o.mu.Unlock()
	if o.runErr != nil {
		return Stats{}, o.runErr
	}
	return Stats{FilesParsed: 1, ChunksEmbedded: 1}, nil
}

func (o *syncStateObserver) observations() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seen...)
}

var _ Orchestrator = (*syncStateObserver)(nil)

// TestSyncState_QueuedJobLeavesTheColumnUntouched pins the bead's "a
// merely queued job leaves sync_state unchanged" rule: Enqueue is not a
// state transition, claim is.
//
// Cannot race, and needs no monotonic signal, because there is no second
// writer in this test at all: Run is never called, so no worker goroutine
// exists that could claim the row and move the column. The only code that
// touches Postgres here is Enqueue itself.
func TestSyncState_QueuedJobLeavesTheColumnUntouched(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoID := seedRepo(ctx, t, pgPool, "group/syncstate-queued")
	stale := "a previous cycle's error"
	setRepoSync(ctx, t, pgPool, repoID, "error", &stale, nil)
	pool := NewPool(testLogger(), pgPool, &OrchestratorMock{}, 1)
	require.NoError(t, pool.Enqueue(ctx, repoID, "main", KindFull))
	require.Equal(t, 1, queuedJobCount(ctx, t, pgPool, repoID, "main", KindFull))
	row := fetchRepoSync(ctx, t, pgPool, repoID)
	assert.Equal(t, "error", row.state, "enqueueing a job must not claim the repo is syncing")
	require.NotNil(t, row.syncError)
	assert.Equal(t, stale, *row.syncError)
}

// TestSyncState_RunningJobMarksRepoSyncing pins the claim-side
// transition: the repo reads 'syncing' for the whole time its job is
// running.
//
// The sample cannot race because it is taken from INSIDE
// Orchestrator.Run, on the worker's own goroutine, and claim commits
// sync_state='syncing' in the same transaction as
// ingest_jobs.status='running' -- which happens-before run() calls the
// orchestrator at all. There is no window in which this read could be too
// early (the claim transaction has committed) or too late (release, and
// therefore any later claim, happens strictly after Run returns). The
// repo starts at 'error', not the schema default 'idle', so a pool that
// wrote nothing on claim would be caught rather than blend into the
// starting value.
func TestSyncState_RunningJobMarksRepoSyncing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoID := seedRepo(ctx, t, pgPool, "group/syncstate-running")
	stale := "a previous cycle's error"
	setRepoSync(ctx, t, pgPool, repoID, "error", &stale, nil)
	orch := &syncStateObserver{pgPool: pgPool, repoID: repoID}
	pool := NewPool(testLogger(), pgPool, orch, 1)
	require.NoError(t, pool.Enqueue(ctx, repoID, "main", KindFull))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pool.Run(runCtx)
	require.NoError(t, pool.DrainRepoID(ctx, repoID))
	seen := orch.observations()
	require.Len(t, seen, 1, "the job must have run exactly once")
	assert.Equal(t, "syncing", seen[0], "repos.sync_state must read 'syncing' while the job is running")
}

// TestSyncState_SuccessMarksRepoIdleAndAdvancesLastSyncedAt pins the
// success-side transition: idle, last_synced_at advanced, and the stale
// error cleared.
//
// At-rest by construction rather than by timing: on success the pool
// schedules no retry (only fail does), the job's terminal status is
// 'succeeded', and nothing enqueues a follow-up, so once DrainRepoID
// returns -- which it does only after succeed's own write has committed,
// since notifyDrainWaiters is deferred behind the write and the waiter
// re-checks real Postgres state -- this repo has no live writer left in
// the process. The two positive assertions are also each monotonic in
// their own right: last_synced_at is only ever advanced by succeed and
// never reset, and sync_error is only ever cleared by a terminal success.
func TestSyncState_SuccessMarksRepoIdleAndAdvancesLastSyncedAt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoID := seedRepo(ctx, t, pgPool, "group/syncstate-success")
	stale := "a previous cycle's error"
	setRepoSync(ctx, t, pgPool, repoID, "error", &stale, nil)
	pool := NewPool(testLogger(), pgPool, &OrchestratorMock{
		RunFunc: func(context.Context, Job) (Stats, error) {
			return Stats{FilesParsed: 3, ChunksEmbedded: 7}, nil
		},
	}, 1)
	require.NoError(t, pool.Enqueue(ctx, repoID, "main", KindIncremental))
	var jobID uuid.UUID
	require.NoError(t, pgPool.QueryRow(ctx, `SELECT id FROM ingest_jobs WHERE repo_id = $1`, repoID).Scan(&jobID))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pool.Run(runCtx)
	require.NoError(t, pool.DrainRepoID(ctx, repoID))
	require.Equal(t, "succeeded", fetchJob(ctx, t, pgPool, jobID).status)
	row := fetchRepoSync(ctx, t, pgPool, repoID)
	assert.Equal(t, "idle", row.state)
	assert.Nil(t, row.syncError, "a successful ingest must clear the stale sync_error")
	require.NotNil(t, row.lastSyncedAt, "a successful ingest must advance last_synced_at")
}

// TestSyncState_FailureMarksRepoErroredAndKeepsLastSyncedAt pins the
// failure-side transition: error, the run error recorded under
// SyncErrorPrefix, and last_synced_at left exactly where it was, since
// the swap orchestrator rolled back and the previous index is still what
// this repo last genuinely synced to.
//
// At-rest by construction: the backoff is an hour, so scheduleRetry's
// timer cannot fire inside this test and the 'failed' row -- and the
// 'error' the same transaction wrote -- have no other writer. The
// falsifiable core is nonetheless anchored on a monotonic quantity,
// ingest_jobs.attempts, which only fail increments and scheduleRetry
// deliberately never resets: attempts==1 is reachable if and only if this
// job failed exactly once, which is precisely the moment sync_state was
// written.
func TestSyncState_FailureMarksRepoErroredAndKeepsLastSyncedAt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoID := seedRepo(ctx, t, pgPool, "group/syncstate-failure")
	syncedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	setRepoSync(ctx, t, pgPool, repoID, "idle", nil, &syncedAt)
	const wantErr = "embedding batch 3: embedder unreachable"
	orch := &OrchestratorMock{
		RunFunc: func(context.Context, Job) (Stats, error) {
			return Stats{}, errors.New(wantErr)
		},
	}
	pool := NewPool(testLogger(), pgPool, orch, 1, WithBackoff(time.Hour, time.Hour))
	require.NoError(t, pool.Enqueue(ctx, repoID, "main", KindIncremental))
	var jobID uuid.UUID
	require.NoError(t, pgPool.QueryRow(ctx, `SELECT id FROM ingest_jobs WHERE repo_id = $1`, repoID).Scan(&jobID))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pool.Run(runCtx)
	require.NoError(t, pool.DrainRepoID(ctx, repoID))
	job := fetchJob(ctx, t, pgPool, jobID)
	require.Equal(t, "failed", job.status)
	require.Equal(t, 1, job.attempts, "attempts is the monotonic signal this test hangs on")
	row := fetchRepoSync(ctx, t, pgPool, repoID)
	assert.Equal(t, "error", row.state)
	require.NotNil(t, row.syncError)
	assert.Equal(t, SyncErrorPrefix+wantErr, *row.syncError,
		"the ingest failure must be recorded on the repo, marked as ingest-authored")
	require.NotNil(t, row.lastSyncedAt, "a failed ingest must not clear last_synced_at")
	assert.WithinDuration(t, syncedAt, *row.lastSyncedAt, time.Second,
		"a failed ingest must not advance last_synced_at: the previous index is still what the repo last synced to")
}

// TestSyncState_RetryReturnsTheRepoToSyncing pins the retry leg of the
// bead's "keep sync_state transitions consistent with job status
// transitions": a failed job leaves the repo at 'error' for the whole
// backoff wait, and the retry's re-claim is what puts it back to
// 'syncing'.
//
// Both halves are observed from inside Orchestrator.Run, so neither is a
// sample of a cycling column taken from outside. The Nth element of the
// observation slice is what the repo read at the instant the Nth attempt
// started, which is a fact about that attempt, not about when this test
// happened to look. The wait itself is on ingest_jobs.attempts, the
// monotonic counter loam-4q2 identifies -- only fail increments it,
// nothing resets it -- so "the retry happened" is reachable exactly once
// and cannot be missed.
func TestSyncState_RetryReturnsTheRepoToSyncing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoID := seedRepo(ctx, t, pgPool, "group/syncstate-retry")
	orch := &syncStateObserver{pgPool: pgPool, repoID: repoID, runErr: errors.New("transient failure")}
	pool := NewPool(testLogger(), pgPool, orch, 1, WithBackoff(50*time.Millisecond, 200*time.Millisecond))
	require.NoError(t, pool.Enqueue(ctx, repoID, "main", KindFull))
	var jobID uuid.UUID
	require.NoError(t, pgPool.QueryRow(ctx, `SELECT id FROM ingest_jobs WHERE repo_id = $1`, repoID).Scan(&jobID))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pool.Run(runCtx)
	require.Eventually(t, func() bool {
		return fetchJob(ctx, t, pgPool, jobID).attempts >= 2
	}, 10*time.Second, 10*time.Millisecond, "expected the failed job to be retried at least once")
	seen := orch.observations()
	require.GreaterOrEqual(t, len(seen), 2, "expected at least two attempts to have started")
	assert.Equal(t, "syncing", seen[0], "the first attempt must find the repo marked syncing")
	assert.Equal(t, "syncing", seen[1],
		"the retry's re-claim must move the repo off 'error' and back to 'syncing'")
	row := fetchRepoSync(ctx, t, pgPool, repoID)
	assert.Nil(t, row.lastSyncedAt, "no attempt succeeded, so last_synced_at must never have been advanced")
}
