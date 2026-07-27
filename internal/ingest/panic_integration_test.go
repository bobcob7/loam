//go:build integration

// This file covers loam-337: the per-job panic boundary in pool.go's run /
// runOrchestrator / recoverOutcomeRecording. Same container and build-tag
// caveats as integration_test.go, whose newTestPool / seedRepo /
// insertQueuedJob / fetchJob / fetchRepoSync helpers these tests reuse.
//
// Both tests here are written against loam-4q2's single-sample trap.
// ingest_jobs.status CYCLES under a live pool -- a correctly-failed job
// goes failed -> queued -> running -> failed forever via scheduleRetry --
// so one sample of it cannot tell "never ran" from "mid-retry". Every
// status assertion below is therefore taken only after a WithBackoff of
// one minute has made the retry unreachable within the test's lifetime,
// and is gated on ingest_jobs.attempts, which only fail() ever increments
// and which scheduleRetry deliberately never resets.
package ingest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// panicTestBackoff is long enough that scheduleRetry cannot possibly
// requeue a failed job before a test finishes asserting on it. Without it
// the row would not sit still and every status assertion in this file
// would be racing a retry (loam-4q2).
const panicTestBackoff = 1 * time.Minute

// TestIngestJob_OrchestratorPanicFailsThatJobAndTheWorkerSurvives is the
// bead's (a)/(b)/(c) in one test, on a pool with exactly ONE worker --
// which is what makes (c) load-bearing. Before loam-337 a panicking
// Orchestrator unwound out of Pool.run, out of Pool.work, and terminated
// the process; with a single worker it is also the only goroutine that can
// ever claim another job, so "a job for a different repo runs afterwards"
// is provable only if that exact goroutine survived the panic.
//
// The healthy repo's job is enqueued strictly AFTER the poisoned job's
// failure has been observed, so "subsequent" is a happens-after fact
// rather than a scheduling coincidence -- two jobs seeded up front could
// have been claimed in either order, or concurrently on a wider pool, and
// would prove nothing about surviving the panic.
//
// Why the status/error/sync_state samples cannot race: the wait is on
// attempts >= 1, which is monotonic (only fail() writes it, scheduleRetry
// never resets it), and the backoff is panicTestBackoff, so once attempts
// reaches 1 the row is pinned at 'failed' -- and repos.sync_state at
// 'error' -- for a minute, far longer than the assertions below take.
func TestIngestJob_OrchestratorPanicFailsThatJobAndTheWorkerSurvives(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	poisoned := seedRepo(ctx, t, pgPool, "group/panic-poisoned")
	healthy := seedRepo(ctx, t, pgPool, "group/panic-healthy")
	// The loam-c94.11 shape: a length disagreement in the vector write
	// path, the class of defect that motivated this bead.
	const panicValue = "runtime error: index out of range [3] with length 0"
	orch := &OrchestratorMock{
		RunFunc: func(ctx context.Context, job Job) (Stats, error) {
			if job.RepoID == poisoned {
				panic(panicValue)
			}
			return Stats{FilesParsed: 2, ChunksEmbedded: 5}, nil
		},
	}
	pool := NewPool(testLogger(), pgPool, orch, 1, WithBackoff(panicTestBackoff, panicTestBackoff))
	poisonedJob := insertQueuedJob(ctx, t, pgPool, poisoned, "main", KindFull)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pool.Run(runCtx)

	require.Eventually(t, func() bool {
		return fetchJob(ctx, t, pgPool, poisonedJob).attempts >= 1
	}, 15*time.Second, 10*time.Millisecond,
		"the panicking job must be recorded as an attempt through the ordinary fail() path -- attempts is the monotonic signal, status cycles")
	row := fetchJob(ctx, t, pgPool, poisonedJob)
	assert.Equal(t, "failed", row.status)
	assert.Equal(t, 1, row.attempts, "exactly one attempt: a minute of backoff makes a second one unreachable here")
	require.NotNil(t, row.errText, "a recovered panic must be recorded in the error column, not merely logged")
	assert.Contains(t, *row.errText, panicValue, "the recovered value itself must reach the error column")
	assert.Contains(t, *row.errText, errJobPanicked.Error(), "the error column must mark this as a panic, not an ordinary pipeline error")

	// The repos half of fail()'s transaction: routing the panic through
	// fail() rather than a parallel path is what gets these written at all.
	sync := fetchRepoSync(ctx, t, pgPool, poisoned)
	assert.Equal(t, "error", sync.state)
	require.NotNil(t, sync.syncError)
	assert.Equal(t, SyncErrorPrefix+*row.errText, *sync.syncError)
	assert.Nil(t, sync.lastSyncedAt, "a panicked job must not claim a successful sync")

	// (c): the one worker goroutine is still alive and still claiming.
	require.NoError(t, pool.Enqueue(ctx, healthy, "main", KindFull))
	var healthyJob uuid.UUID
	require.NoError(t, pgPool.QueryRow(ctx, `SELECT id FROM ingest_jobs WHERE repo_id = $1`, healthy).Scan(&healthyJob))
	require.Eventually(t, func() bool {
		return fetchJob(ctx, t, pgPool, healthyJob).status == "succeeded"
	}, 15*time.Second, 10*time.Millisecond,
		"a job for a different repo, enqueued after the panic, must still run -- one poisoned repo must not halt ingestion for every other repo")
	assert.Equal(t, "idle", fetchRepoSync(ctx, t, pgPool, healthy).state)
}

// TestIngestJob_OrchestratorPanicWakesDrainWaitersAndReleasesTheRepoSlot
// is the bead's (d) plus the leak check behind it. A recovered panic that
// leaves the pool wedged is barely better than the crash, and there are
// two distinct ways to wedge it: leave a DrainRepoID/Shutdown caller
// parked on a channel nobody closes, or never delete the repo from
// Pool.busy so claim() skips it forever.
//
// Both are checked from a state that cannot race. The orchestrator blocks
// on gate before panicking, so the job is in status 'running' by
// construction -- not by timing -- for as long as the test wants, which is
// what lets DrainRepoID register a real waiter rather than taking
// checkOrRegisterDrainWaiter's already-drained early return. After the
// gate is released the job fails, and panicTestBackoff keeps it in
// 'failed' (neither queued nor running) so the woken drain has a stable
// answer to re-check against.
func TestIngestJob_OrchestratorPanicWakesDrainWaitersAndReleasesTheRepoSlot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoID := seedRepo(ctx, t, pgPool, "group/panic-drain")
	var calls atomic.Int32
	running := make(chan struct{})
	gate := make(chan struct{})
	orch := &OrchestratorMock{
		RunFunc: func(ctx context.Context, job Job) (Stats, error) {
			if calls.Add(1) == 1 {
				close(running)
				<-gate
				panic("embedder returned a 384-wide vector for a 768-wide column")
			}
			return Stats{FilesParsed: 1, ChunksEmbedded: 1}, nil
		},
	}
	pool := NewPool(testLogger(), pgPool, orch, 1, WithBackoff(panicTestBackoff, panicTestBackoff))
	firstJob := insertQueuedJob(ctx, t, pgPool, repoID, "main", KindFull)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pool.Run(runCtx)
	<-running

	drained := make(chan error, 1)
	go func() {
		drainCtx, cancelDrain := context.WithTimeout(ctx, 30*time.Second)
		defer cancelDrain()
		drained <- pool.DrainRepoID(drainCtx, repoID)
	}()
	select {
	case err := <-drained:
		t.Fatalf("DrainRepoID returned (err=%v) while the job was still running -- it must register a waiter here, or the rest of this test is vacuous", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	select {
	case err := <-drained:
		require.NoError(t, err, "the drain waiter must be woken and find the repo settled, not time out")
	case <-time.After(30 * time.Second):
		t.Fatal("DrainRepoID never returned after the panicking job failed -- a recovered panic that leaves a Drain/Shutdown caller blocked forever has just traded a crash for a hang")
	}
	require.Equal(t, 1, fetchJob(ctx, t, pgPool, firstJob).attempts, "the panicking job must have gone through fail()")

	// The per-repo serialization slot: if the panic path leaked it, claim()
	// would skip this repo for the lifetime of the process and no follow-up
	// for it could ever be claimed again.
	require.NoError(t, pool.Enqueue(ctx, repoID, "main", KindIncremental))
	var secondJob uuid.UUID
	require.NoError(t, pgPool.QueryRow(ctx,
		`SELECT id FROM ingest_jobs WHERE repo_id = $1 AND kind = $2`, repoID, KindIncremental).Scan(&secondJob))
	require.Eventually(t, func() bool {
		return fetchJob(ctx, t, pgPool, secondJob).status == "succeeded"
	}, 15*time.Second, 10*time.Millisecond,
		"a follow-up job for the SAME repo must still be claimable after a panic -- otherwise the panic leaked the per-repo slot")
	assert.Equal(t, int32(2), calls.Load())
}
