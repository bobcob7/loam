//go:build integration

// This file covers loam-54o.17: what Pool.claim does when migration
// 0008_ingest_jobs_running_guard's partial unique index
// (ingest_jobs_one_running_per_repo) REJECTS its write.
//
// Every test here needs a real Postgres, and not merely because Pool has
// no querier seam: the behaviour under test is Postgres enforcing a
// unique index against committed state while a second transaction holds
// an uncommitted conflicting row. There is no mock of that. See
// integration_test.go's header for the build tag, the podman
// TESTCONTAINERS_RYUK_DISABLED note, and newTestPool/seedRepo/fetchJob,
// which this file reuses rather than duplicating.
//
// TWO SHAPES OF TEST LIVE HERE, and the split is deliberate:
//
//   - The holdUncommittedRunningJob tests are DETERMINISTIC. They force
//     the rejection every single run by parking an uncommitted 'running'
//     row on its own connection, letting claim pick that repo (its
//     snapshot cannot see the uncommitted row), and only then committing
//     -- at which point claim's blocked UPDATE resumes into a guaranteed
//     unique violation. Without this, the rejection path would be
//     exercised only by luck.
//   - TestClaim_ConcurrentPoolsRacingOneRepo is the emergent-property
//     test, run under internal repetition, which asserts the invariant
//     holds across whichever interleaving actually occurred.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runningJobHold is an UNCOMMITTED ingest_jobs row in status 'running',
// parked on a connection of its own so it is invisible to every other
// transaction's snapshot until commit is called. It is the lever that
// makes the guard constraint's rejection deterministic instead of a race
// a test can only hope to hit: a claim that picks this repo cannot see
// the row when it chooses its candidate, but ingest_jobs_one_running_per_repo
// makes its UPDATE block on this transaction's id, so the rejection
// happens exactly when the test says so.
type runningJobHold struct {
	pid  int32
	tx   pgx.Tx
	conn *pgxpool.Conn
}

// holdUncommittedRunningJob opens the hold described above for repoID.
// The backend pid is captured so waitUntilBlockedBy can identify claims
// blocked by THIS hold specifically, rather than counting lock waiters
// globally -- which would be ambiguous the moment a test drives more than
// one hold in sequence, since a just-released waiter and a not-yet-blocked
// one are indistinguishable by count.
func holdUncommittedRunningJob(ctx context.Context, t *testing.T, pgPool *pgxpool.Pool, repoID uuid.UUID) *runningJobHold {
	t.Helper()
	conn, err := pgPool.Acquire(ctx)
	require.NoError(t, err)
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	var pid int32
	require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	id, err := uuid.NewV7()
	require.NoError(t, err)
	_, err = tx.Exec(ctx,
		`INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status, attempts, queued_at, started_at) VALUES ($1, $2, 'main', 'full', 'running', 0, now(), now())`,
		id, repoID,
	)
	require.NoError(t, err)
	hold := &runningJobHold{pid: pid, tx: tx, conn: conn}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
		conn.Release()
	})
	return hold
}

// commit publishes the held running row, releasing any claim blocked on
// it into a unique violation.
func (h *runningJobHold) commit(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, h.tx.Commit(ctx))
	h.conn.Release()
}

// waitUntilBlockedBy blocks until at least one backend is waiting on a
// lock held by hold's backend -- i.e. a claim has reached its UPDATE and
// is parked on ingest_jobs_one_running_per_repo. pg_blocking_pids is
// asked rather than a bare waiter count for the reason in
// holdUncommittedRunningJob's comment: this answers "blocked by THIS
// hold", which stays unambiguous across a sequence of holds.
func waitUntilBlockedBy(ctx context.Context, t *testing.T, pgPool *pgxpool.Pool, hold *runningJobHold) {
	t.Helper()
	require.Eventually(t, func() bool {
		var blocked int
		err := pgPool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity a WHERE a.wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(a.pid))`,
			hold.pid,
		).Scan(&blocked)
		return err == nil && blocked > 0
	}, 20*time.Second, 10*time.Millisecond,
		"expected a claim to be blocked by the held running row, waiting on ingest_jobs_one_running_per_repo")
}

// repoSyncState reads repos.sync_state, the column a claim moves to
// 'syncing' in the SAME transaction as the ingest_jobs write.
func repoSyncState(ctx context.Context, t *testing.T, pgPool *pgxpool.Pool, repoID uuid.UUID) string {
	t.Helper()
	var state string
	require.NoError(t, pgPool.QueryRow(ctx, `SELECT sync_state FROM repos WHERE id = $1`, repoID).Scan(&state))
	return state
}

// runningJobIDs returns every ingest_jobs row currently in status
// 'running' for repoID. The guard constraint makes "more than one" a
// state Postgres refuses to hold, so a length assertion on this is a
// direct check that the constraint did its job -- and, when the length is
// one, gives the test the winner's identity to compare against what claim
// actually returned.
func runningJobIDs(ctx context.Context, t *testing.T, pgPool *pgxpool.Pool, repoID uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := pgPool.Query(ctx, `SELECT id FROM ingest_jobs WHERE repo_id = $1 AND status = 'running' ORDER BY id`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// insertQueuedJobAt seeds a queued row with an EXPLICIT id and queued_at,
// which insertQueuedJob (integration_test.go) deliberately does not allow
// -- it stamps now() and a fresh UUIDv7, so it can never produce the
// exact queued_at tie TestClaim_QueuedAtTie_BreaksOnID needs.
func insertQueuedJobAt(ctx context.Context, t *testing.T, pgPool *pgxpool.Pool, id, repoID uuid.UUID, queuedAt time.Time, kind Kind) {
	t.Helper()
	_, err := pgPool.Exec(ctx,
		`INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status, attempts, queued_at) VALUES ($1, $2, 'main', $3, 'queued', 0, $4)`,
		id, repoID, kind, queuedAt,
	)
	require.NoError(t, err)
}

// TestClaim_SkipsARepoWhoseRunningJobItDidNotStart proves claimQuery's
// NOT EXISTS filter, the cheap half of loam-54o.17: a committed 'running'
// row that this Pool knows nothing about -- another replica's, or one
// this process stranded via a recovered panic in outcome recording, which
// releases the busy slot but leaves the row -- must take that repo out of
// the running before any write is attempted.
//
// The repo's queued job here is the OLDEST, so without the filter it
// would be selected, written, and rejected by the constraint.
//
// THE RETURNED JOB ALONE CANNOT SHOW THAT. With the filter gone, claim's
// retry loop recovers -- it eats the unique violation, excludes repoA,
// and returns repoB's job anyway -- so every assertion about WHAT was
// claimed passes either way. What separates "skipped it" from "collided
// with it and recovered" is that no rejection was ever logged, which is
// the assertion that makes this test able to fail at all.
func TestClaim_SkipsARepoWhoseRunningJobItDidNotStart(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoA := seedRepo(ctx, t, pgPool, "group/skip-running-a")
	repoB := seedRepo(ctx, t, pgPool, "group/skip-running-b")
	insertRunningJob(ctx, t, pgPool, repoA, "main", KindFull)
	orphanedQueued := insertQueuedJob(ctx, t, pgPool, repoA, "main", KindIncremental)
	wantedJob := insertQueuedJob(ctx, t, pgPool, repoB, "main", KindIncremental)
	handler, logger := newCapturingHandler()
	pool := NewPool(logger, pgPool, &OrchestratorMock{}, 1)
	job, claimed, err := pool.claim(ctx)
	require.NoError(t, err)
	require.True(t, claimed, "repoB's job is claimable and must be claimed")
	assert.Equal(t, wantedJob, job.ID, "the claim must skip repoA (already running elsewhere) and take repoB's job")
	assert.Equal(t, repoB, job.RepoID)
	assert.Equal(t, "queued", fetchJob(ctx, t, pgPool, orphanedQueued).status,
		"repoA's queued job must be left alone, not claimed and not failed")
	assert.Equal(t, "idle", repoSyncState(ctx, t, pgPool, repoA),
		"a repo the claim skipped must not have been moved to syncing")
	assert.False(t, handler.findLevel(claimRejectedMsg, slog.LevelDebug),
		"a repo whose running job is already committed must be filtered out by the query, not discovered by eating a unique violation and retrying")
}

// TestClaim_RejectedByTheGuardThenClaimsADifferentRepo is the core
// deterministic proof for this bead. A claim picks repoA (its snapshot
// cannot see the uncommitted running row), blocks on the guard, and is
// rejected with SQLSTATE 23505 the instant the hold commits. The bead's
// whole question is what happens next, and this asserts all four halves
// of the answer:
//
//  1. no error reaches the caller -- work() would otherwise log it at
//     ERROR, which is the production symptom this bead exists to remove;
//  2. the worker does NOT idle: it excludes repoA and claims repoB, so a
//     lost race costs one round trip, not a whole poll interval of a
//     worker doing nothing while other repos' jobs sit queued;
//  3. repoA's queued job is untouched -- still 'queued', still claimable
//     by whoever finishes the repo's running job, never 'failed';
//  4. repoA's sync_state is untouched, because the rejected attempt's
//     whole transaction rolls back and claimOnce's two writes share it.
//     Note what this does NOT establish: the ORDER of those two writes is
//     irrelevant to this property, and an earlier draft of this comment
//     claimed otherwise. A mutation that swaps them keeps the suite green
//     (loam-54o.17's M8), which is correct -- 'syncing' is unreachable
//     from a rejected claim either way;
//  5. the rejection is logged at DEBUG and at no other level. This is the
//     bead's headline symptom: the pre-change code surfaced ordinary
//     contention on work()'s ERROR path, and nothing about the returned
//     values distinguishes "handled quietly" from "handled, but still
//     shouting about it every time two replicas race".
func TestClaim_RejectedByTheGuardThenClaimsADifferentRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoA := seedRepo(ctx, t, pgPool, "group/rejected-a")
	repoB := seedRepo(ctx, t, pgPool, "group/rejected-b")
	losingJob := insertQueuedJob(ctx, t, pgPool, repoA, "main", KindIncremental)
	wantedJob := insertQueuedJob(ctx, t, pgPool, repoB, "main", KindFull)
	hold := holdUncommittedRunningJob(ctx, t, pgPool, repoA)
	handler, logger := newCapturingHandler()
	pool := NewPool(logger, pgPool, &OrchestratorMock{}, 1)
	type result struct {
		job     Job
		claimed bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		job, claimed, err := pool.claim(ctx)
		done <- result{job: job, claimed: claimed, err: err}
	}()
	waitUntilBlockedBy(ctx, t, pgPool, hold)
	hold.commit(ctx, t)
	got := <-done
	require.NoError(t, got.err, "losing to ingest_jobs_one_running_per_repo is contention, not a failure to report")
	require.True(t, got.claimed, "the worker must move on to another repo's job, not idle for a whole poll interval")
	assert.Equal(t, wantedJob, got.job.ID)
	assert.Equal(t, repoB, got.job.RepoID)
	assert.Equal(t, "queued", fetchJob(ctx, t, pgPool, losingJob).status,
		"the rejected candidate must stay claimable, not be consumed or failed")
	assert.Equal(t, "idle", repoSyncState(ctx, t, pgPool, repoA),
		"a rejected claim must roll back before touching repos.sync_state")
	assert.Equal(t, "syncing", repoSyncState(ctx, t, pgPool, repoB),
		"the claim that DID succeed still owns its repo's sync_state handover")
	assert.Equal(t, map[uuid.UUID]struct{}{repoB: {}}, pool.busy,
		"only the repo actually claimed may hold a serialization slot -- a slot leaked on the rejection path would wedge repoA forever")
	assert.True(t, handler.findLevel(claimRejectedMsg, slog.LevelDebug),
		"the rejection must be observable at DEBUG for diagnosis")
	assert.False(t, handler.findLevel(claimRejectedMsg, slog.LevelError),
		"ordinary contention logged at ERROR is this bead's headline symptom -- a repeated ERROR for expected behaviour trains operators to ignore the log")
	assert.False(t, handler.findLevel(claimRejectedMsg, slog.LevelWarn),
		"nor at WARN: one lost race per busy repo is routine, not something an operator must look at")
}

// TestClaim_EveryCandidateRejected_ReportsNothingToClaimNotAnError walks
// claim's bound all the way to exhaustion: maxClaimAttempts repos, each
// with a queued job and each with an uncommitted running row released
// only once the claim is actually blocked on it, so every single attempt
// is rejected and none can be skipped by claimQuery's snapshot filter.
//
// What it pins is that the bound EXISTS and that hitting it is not an
// error. Sustained contention on every visible candidate means every repo
// this worker can see is being handled by someone else, which is
// materially "nothing to claim": the worker falls back to the wake/poll
// path with a fresh snapshot rather than spinning under mu, and nothing
// is lost -- every rejected job is still queued at the end.
//
// The per-iteration handshake (block, then commit) is what makes this
// deterministic rather than a hope. Committing all the holds up front
// would instead let attempt 2's NOT EXISTS filter see every remaining
// running row and return "nothing to claim" after ONE rejection, testing
// the opposite of what this test is for.
func TestClaim_EveryCandidateRejected_ReportsNothingToClaimNotAnError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repos := make([]uuid.UUID, maxClaimAttempts)
	jobs := make([]uuid.UUID, maxClaimAttempts)
	holds := make([]*runningJobHold, maxClaimAttempts)
	for i := range repos {
		repos[i] = seedRepo(ctx, t, pgPool, fmt.Sprintf("group/exhaust-%d", i))
		jobs[i] = insertQueuedJob(ctx, t, pgPool, repos[i], "main", KindIncremental)
	}
	for i := range repos {
		holds[i] = holdUncommittedRunningJob(ctx, t, pgPool, repos[i])
	}
	handler, logger := newCapturingHandler()
	mp, reader := claimMeter(t)
	pool := NewPool(logger, pgPool, &OrchestratorMock{}, 1, WithMeterProvider(mp))
	type result struct {
		claimed bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		_, claimed, err := pool.claim(ctx)
		done <- result{claimed: claimed, err: err}
	}()
	for i := range holds {
		waitUntilBlockedBy(ctx, t, pgPool, holds[i])
		holds[i].commit(ctx, t)
	}
	got := <-done
	require.NoError(t, got.err, "exhausting the retry bound is contention, not a failure")
	assert.False(t, got.claimed, "with every visible repo already running elsewhere there is genuinely nothing to claim")
	assert.Empty(t, pool.busy, "no serialization slot may be held after a claim that claimed nothing")
	for i, jobID := range jobs {
		assert.Equalf(t, "queued", fetchJob(ctx, t, pgPool, jobID).status,
			"repo %d's rejected job must still be queued and claimable", i)
	}
	assert.True(t, handler.findLevel(claimExhaustedMsg, slog.LevelWarn),
		"exhausting the bound is rare enough to be worth a WARN -- unlike the per-rejection DEBUG, which is ordinary contention")
	assert.False(t, handler.findLevel(claimExhaustedMsg, slog.LevelDebug),
		"nor is it DEBUG: every visible repo running elsewhere is a standing symptom (fleet-wide contention, or a stranded 'running' row) an operator should see")
	// loam-gp7m: exhaustion is the ONLY producer of the 'contended' claim
	// outcome, and it is the one outcome that is indistinguishable from
	// 'empty' in claim's return values -- no job, no error -- while meaning
	// the opposite thing. This test already owns the machinery that forces
	// it deterministically, so the metric assertion belongs here rather than
	// duplicating five holds elsewhere.
	assert.Equal(t, map[string]int64{claimOutcomeContended: 1}, claimCounts(ctx, t, reader),
		"a claim that exhausted every candidate must count as contended, never as empty")
}

// TestClaim_NonGuardErrorStillSurfaces is the other side of matching by
// constraint NAME. A claim failure that is NOT
// ingest_jobs_one_running_per_repo must keep reaching the caller, or the
// retry loop would silently swallow real defects as contention and a
// worker would idle forever with nothing in the log to say why.
//
// Dropping started_at makes claimOnce's UPDATE fail with 42703
// (undefined_column) AFTER a candidate has been selected, which is
// exactly where the guard violation would have arrived -- so this reaches
// the same branch with a different error, rather than failing earlier on
// the SELECT. It is safe because newTestPool gives every test its own
// container.
func TestClaim_NonGuardErrorStillSurfaces(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoID := seedRepo(ctx, t, pgPool, "group/non-guard-error")
	jobID := insertQueuedJob(ctx, t, pgPool, repoID, "main", KindFull)
	_, err := pgPool.Exec(ctx, `ALTER TABLE ingest_jobs DROP COLUMN started_at`)
	require.NoError(t, err)
	pool := NewPool(testLogger(), pgPool, &OrchestratorMock{}, 1)
	job, claimed, err := pool.claim(ctx)
	require.Error(t, err, "an error that is not the guard constraint must not be classified as contention")
	assert.ErrorContains(t, err, jobID.String(), "the error must still name the job it failed on")
	assert.False(t, claimed)
	assert.Zero(t, job.ID, "a failed claim returns no job")
	assert.Empty(t, pool.busy, "a failed claim must not mark the repo busy")
}

// claimRaceRounds is how many independent races
// TestClaim_ConcurrentPoolsRacingOneRepo runs inside ONE container.
//
// The repetition belongs here rather than in `go test -count=N` because
// the expensive part is the Postgres container, not the race: newTestPool
// starts one per test function, so -count=200 would start 200 containers
// to run 200 races. Rounds are independent (a fresh repo and fresh Pools
// each time, so no round can mask another's failure by leaving state
// behind) and cost a handful of round trips each, which is what makes a
// count this size affordable at all. loam-54o.13 needed 200+ repetitions
// to surface the cross-row window this index closes; 300 keeps margin
// above that.
const claimRaceRounds = 300

// TestClaim_ConcurrentPoolsRacingOneRepo is the emergent-property test:
// two Pools with SEPARATE busy maps -- the second replica the bead names
// as the real exposure, since one Pool's mutex already serializes it
// against itself -- racing for one repo that has two queued jobs.
//
// Two queued jobs, not one, because one is the wrong fixture: with a
// single row, FOR UPDATE SKIP LOCKED alone decides it and the guard
// constraint is never consulted. Two rows is the case that defeated the
// single-statement claim and forced migration 0008 -- each racer can pick
// a DIFFERENT row from a snapshot that has not seen the other, so nothing
// but the index can stop both writes.
//
// The two jobs also differ in KIND, so the assertions can distinguish
// "the winner reported the job it actually claimed" from "some job won":
// with identical rows, a claim that returned the wrong row's identity
// would be indistinguishable from a correct one. Every round therefore
// checks that the winner's returned ID and Kind match the single row
// Postgres actually holds in 'running'.
func TestClaim_ConcurrentPoolsRacingOneRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	handler, logger := newCapturingHandler()
	for round := range claimRaceRounds {
		repoID := seedRepo(ctx, t, pgPool, fmt.Sprintf("group/race-%d", round))
		first := insertQueuedJob(ctx, t, pgPool, repoID, "main", KindIncremental)
		second := insertQueuedJob(ctx, t, pgPool, repoID, "main", KindFull)
		kinds := map[uuid.UUID]Kind{first: KindIncremental, second: KindFull}
		poolA := NewPool(logger, pgPool, &OrchestratorMock{}, 1)
		poolB := NewPool(logger, pgPool, &OrchestratorMock{}, 1)
		type result struct {
			job     Job
			claimed bool
			err     error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for _, p := range []*Pool{poolA, poolB} {
			go func() {
				<-start
				job, claimed, err := p.claim(ctx)
				results <- result{job: job, claimed: claimed, err: err}
			}()
		}
		close(start)
		winners := 0
		var winner Job
		for range 2 {
			got := <-results
			require.NoErrorf(t, got.err, "round %d: a claim that loses this race must report contention, never an error", round)
			if got.claimed {
				winners++
				winner = got.job
			}
		}
		require.Equalf(t, 1, winners, "round %d: exactly one of two racing pools may claim a job for a repo", round)
		running := runningJobIDs(ctx, t, pgPool, repoID)
		require.Lenf(t, running, 1, "round %d: the repo must end with exactly one running job", round)
		assert.Equalf(t, running[0], winner.ID, "round %d: the winner must report the job Postgres actually holds running", round)
		assert.Equalf(t, kinds[running[0]], winner.Kind, "round %d: the winner must report the running row's own kind, not the other candidate's", round)
		loser := first
		if running[0] == first {
			loser = second
		}
		assert.Equalf(t, "queued", fetchJob(ctx, t, pgPool, loser).status,
			"round %d: the job that did not win must stay queued and claimable", round)
		busyCount := len(poolA.busy) + len(poolB.busy)
		assert.Equalf(t, 1, busyCount, "round %d: exactly one pool may hold the repo's serialization slot", round)
	}
	t.Logf("guard-rejection path exercised across %d rounds: %v",
		claimRaceRounds, handler.findLevel(claimRejectedMsg, slog.LevelDebug))
}

// TestClaim_QueuedAtTie_BreaksOnID closes the repo-wide gap loam-c94.18
// tracks, on this query: queued_at's stored precision is microseconds, so
// two rows genuinely can share one, and a bare ORDER BY queued_at leaves
// Postgres free to return either -- making "the oldest queued job first"
// silently untrue for exactly the rows a burst of triggers produces. The
// tie is forced with an explicit identical timestamp rather than hoped
// for from back-to-back inserts, which in practice never collide.
//
// The two jobs are on DIFFERENT repos so the guard constraint plays no
// part here: this is purely about ordering. It mirrors
// internal/ingestjobs' TestClaim_QueuedAtTie_BreaksOnID, since both
// queries now carry the same (queued_at, id) convention.
func TestClaim_QueuedAtTie_BreaksOnID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pgPool := newTestPool(ctx, t)
	repoLow := seedRepo(ctx, t, pgPool, "group/tie-low")
	repoHigh := seedRepo(ctx, t, pgPool, "group/tie-high")
	tie := time.Now().UTC().Truncate(time.Microsecond)
	lowID := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	highID := uuid.MustParse("ffffffff-0000-7000-8000-000000000002")
	insertQueuedJobAt(ctx, t, pgPool, highID, repoHigh, tie, KindFull)
	insertQueuedJobAt(ctx, t, pgPool, lowID, repoLow, tie, KindIncremental)
	pool := NewPool(testLogger(), pgPool, &OrchestratorMock{}, 1)
	job, claimed, err := pool.claim(ctx)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, lowID, job.ID,
		"two jobs sharing a queued_at must be claimed in id order, not in whatever order Postgres happens to return")
}
