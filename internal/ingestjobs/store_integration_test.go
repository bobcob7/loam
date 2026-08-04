//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/ingestjobs/... -v
package ingestjobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// sharedPool is one migrated Postgres for the whole test binary, started
// once in TestMain rather than per test -- matching internal/workbranchstore
// and internal/reposstore's own integration test harnesses, and keeping
// concurrent Docker usage low while other agents share the daemon. Every
// test scopes its rows to its own freshly seeded repo id; cascading FKs
// (ingest_jobs.repo_id REFERENCES repos ON DELETE CASCADE) keep tests
// isolated without needing a container each.
var sharedPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting shared pgvector container:", err)
		os.Exit(1)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolving shared container DSN:", err)
		os.Exit(1)
	}
	if err := migrations.Migrate(ctx, dsn, logger); err != nil {
		fmt.Fprintln(os.Stderr, "migrating shared container:", err)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "opening shared pool:", err)
		os.Exit(1)
	}
	sharedPool = pool
	code := m.Run()
	pool.Close()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared pgvector container:", err)
	}
	os.Exit(code)
}

// newTestStore returns a Store wired over the package's sharedPool.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(sharedPool, testLogger())
}

// seedRepo inserts a fresh, uniquely-named repos row and returns its id --
// the FK ingest_jobs.repo_id requires, and the identity that scopes each
// test's rows away from every other test sharing sharedPool.
func seedRepo(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()
	repoID := uuid.Must(uuid.NewV7())
	_, err := sharedPool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		repoID, "group/ingestjobs-"+repoID.String(),
	)
	require.NoError(t, err)
	// Claim scans the ENTIRE ingest_jobs table -- deliberately, since the
	// single-running-per-repo guard is a global property, not a per-repo
	// query. That means a row this test enqueues and never claims/deletes
	// (e.g. a leftover 'queued' job in a test that only claims some of what
	// it enqueued) is still a live candidate for a LATER sequential test's
	// own Claim call, and being older it can win over that later test's own
	// fixtures -- corrupting an assertion that has nothing to do with this
	// test. Deleting the repo on cleanup cascades to every ingest_jobs row
	// this test created (ingest_jobs.repo_id ON DELETE CASCADE,
	// 0001_init.up.sql), regardless of status, so no test can leak a
	// candidate row into any test that runs after it.
	t.Cleanup(func() {
		_, err := sharedPool.Exec(context.Background(), `DELETE FROM repos WHERE id = $1`, repoID)
		assert.NoError(t, err)
	})
	return repoID
}

// TestEnqueue_CreatesQueuedJobWithZeroAttempts proves a freshly enqueued
// job matches the bead's DESCRIPTION exactly: status queued, attempts 0,
// no error/stats/started_at/finished_at yet.
func TestEnqueue_CreatesQueuedJobWithZeroAttempts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	job, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	assert.Equal(t, repoID, job.RepoID)
	assert.Equal(t, "main", job.TargetBranch)
	assert.Equal(t, KindFull, job.Kind)
	assert.Equal(t, StatusQueued, job.Status)
	assert.Zero(t, job.Attempts)
	assert.Nil(t, job.Error)
	assert.Nil(t, job.Stats)
	assert.Nil(t, job.StartedAt)
	assert.Nil(t, job.FinishedAt)
	assert.False(t, job.QueuedAt.IsZero())
}

// TestEnqueue_InvalidKind_RejectedByCheckConstraint proves the fixed kind
// vocabulary is enforced by the real ingest_jobs_kind_check constraint,
// not by a parallel Go-side allowlist that could silently drift from it
// (bd remember guard-design-ask-dont-model-2026-08): Store.Enqueue does
// not validate Kind itself, so this executes the actual rule instead of
// restating it.
func TestEnqueue_InvalidKind_RejectedByCheckConstraint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	_, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: Kind("partial")})
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the rejection must be the real CHECK constraint, not some other failure")
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, "ingest_jobs_kind_check", pgErr.ConstraintName)
}

// TestKindConstants_BothAcceptedByCheckConstraint is the positive half of
// the constraint-vs-Go proof above: every Kind constant this package
// defines must actually be a value the real constraint accepts, proved by
// inserting each through the real store rather than assumed from the SQL
// text.
func TestKindConstants_BothAcceptedByCheckConstraint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	for _, kind := range []Kind{KindIncremental, KindFull} {
		repoID := seedRepo(t, ctx)
		_, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: kind})
		assert.NoError(t, err, "kind %q must be accepted by ingest_jobs_kind_check", kind)
	}
}

// TestStatusValues_MatchCheckConstraint proves every status this package's
// queries ever write (queued, running, succeeded, failed) is one the real
// ingest_jobs_status_check constraint accepts, and that a fifth value is
// rejected -- exercised directly against Postgres since, unlike kind, no
// query in this package ever writes status as a caller-supplied parameter
// (every one of EnqueueIngestJob/ClaimIngestJob/CompleteIngestJob/
// FailIngestJob/RequeueIngestJob hardcodes its own status literal), so
// there is no Go-side value to test through the store at all -- the
// constraint is the only place this vocabulary is ever expressed.
func TestStatusValues_MatchCheckConstraint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repoID := seedRepo(t, ctx)
	for _, status := range []Status{StatusQueued, StatusRunning, StatusSucceeded, StatusFailed} {
		_, err := sharedPool.Exec(ctx,
			`INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status, attempts, queued_at) VALUES ($1, $2, 'main', 'full', $3, 0, now())`,
			uuid.Must(uuid.NewV7()), repoID, string(status),
		)
		assert.NoError(t, err, "status %q must be accepted by ingest_jobs_status_check", status)
	}
	_, err := sharedPool.Exec(ctx,
		`INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status, attempts, queued_at) VALUES ($1, $2, 'main', 'full', 'cancelled', 0, now())`,
		uuid.Must(uuid.NewV7()), repoID,
	)
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, "ingest_jobs_status_check", pgErr.ConstraintName)
}

// TestGet_UnknownID_ReturnsErrNotFound proves Get's absence path.
func TestGet_UnknownID_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	_, err := store.Get(ctx, uuid.Must(uuid.NewV7()))
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

// TestClaim_NoQueuedJobs_ReturnsErrNoJobAvailable proves Claim's ordinary
// empty-queue outcome against a repo with no jobs at all.
func TestClaim_NoQueuedJobs_ReturnsErrNoJobAvailable(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	seedRepo(t, ctx) // a repo exists, but has enqueued nothing
	_, err := store.Claim(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoJobAvailable)
}

// TestClaim_ClaimsOldestQueuedJobFirst seeds two DIFFERENT repos (distinct
// target branches and kinds too, so a bug that returns the wrong job is
// visible rather than masked by identical fixtures -- see this bead's
// seeding lesson) with queued jobs at deliberately different queued_at
// times and proves Claim always returns the globally oldest one, with
// status flipped to running and started_at set.
func TestClaim_ClaimsOldestQueuedJobFirst(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	repoA := seedRepo(t, ctx)
	repoB := seedRepo(t, ctx)
	older, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoA, TargetBranch: "release", Kind: KindFull})
	require.NoError(t, err)
	time.Sleep(3 * time.Millisecond)
	_, err = store.Enqueue(ctx, EnqueueParams{RepoID: repoB, TargetBranch: "main", Kind: KindIncremental})
	require.NoError(t, err)

	claimed, err := store.Claim(ctx)
	require.NoError(t, err)
	assert.Equal(t, older.ID, claimed.ID, "the globally oldest queued job must be claimed first")
	assert.Equal(t, StatusRunning, claimed.Status)
	assert.NotNil(t, claimed.StartedAt)
	assert.Zero(t, claimed.Attempts, "Claim must not touch attempts -- only Fail increments it")
}

// TestClaim_EnforcesAtMostOneRunningJobPerRepo is this bead's central
// DESIGN requirement, proved single-threaded first (the concurrent proofs
// below cover the race): a repo with a running job and a SECOND queued job
// waiting behind it must not have that second job claimed while another
// repo's own queued job sits available -- Claim must skip the blocked
// repo entirely and return the other repo's job instead of erroring.
func TestClaim_EnforcesAtMostOneRunningJobPerRepo(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	busyRepo := seedRepo(t, ctx)
	freeRepo := seedRepo(t, ctx)
	first, err := store.Enqueue(ctx, EnqueueParams{RepoID: busyRepo, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	second, err := store.Enqueue(ctx, EnqueueParams{RepoID: busyRepo, TargetBranch: "main", Kind: KindIncremental})
	require.NoError(t, err)
	other, err := store.Enqueue(ctx, EnqueueParams{RepoID: freeRepo, TargetBranch: "develop", Kind: KindIncremental})
	require.NoError(t, err)

	claimed1, err := store.Claim(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.ID, claimed1.ID)

	claimed2, err := store.Claim(ctx)
	require.NoError(t, err, "busyRepo's running job must not block freeRepo's own queued job")
	assert.Equal(t, other.ID, claimed2.ID)

	_, err = store.Claim(ctx)
	require.Error(t, err, "busyRepo's second queued job must NOT be claimable while its first job is still running")
	assert.ErrorIs(t, err, errNoJobAvailable)

	// Once the running job resolves, the previously-blocked job becomes
	// claimable -- proving the guard is a live fact about current running
	// jobs, not a one-shot lock.
	_, err = store.Complete(ctx, claimed1.ID, nil)
	require.NoError(t, err)
	claimed3, err := store.Claim(ctx)
	require.NoError(t, err)
	assert.Equal(t, second.ID, claimed3.ID)
}

// TestClaim_ConcurrentClaims_SameRepo_OnlyOneWins is the race version of
// the guard above: two goroutines call Claim at the same instant against a
// repo with two queued jobs. Exactly one must win the running slot; the
// other must see errNoJobAvailable (there is no other repo's job for it to
// fall through to), never a second job for the same repo running at once.
func TestClaim_ConcurrentClaims_SameRepo_OnlyOneWins(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	job1, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	job2, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "release", Kind: KindIncremental})
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]Job, 2)
	errs := make([]error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			results[i], errs[i] = store.Claim(ctx)
		}(i)
	}
	start.Done()
	wg.Wait()

	successes, failures := 0, 0
	var winner Job
	for i := range 2 {
		switch {
		case errs[i] == nil:
			successes++
			winner = results[i]
		case errors.Is(errs[i], errNoJobAvailable):
			failures++
		default:
			t.Fatalf("unexpected Claim error: %v", errs[i])
		}
	}
	require.Equal(t, 1, successes, "exactly one concurrent claim against the same repo must win")
	require.Equal(t, 1, failures, "the other must see errNoJobAvailable, not a second running job for the same repo")
	assert.Contains(t, []uuid.UUID{job1.ID, job2.ID}, winner.ID)

	rows, err := sharedPool.Query(ctx, `SELECT count(*) FROM ingest_jobs WHERE repo_id = $1 AND status = 'running'`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var runningCount int
	require.NoError(t, rows.Scan(&runningCount))
	assert.Equal(t, 1, runningCount, "the database itself must show exactly one running job for this repo, not this test's own bookkeeping")
}

// TestClaim_ConcurrentClaims_DifferentRepos_BothWin proves the guard is
// scoped per-repo, not a global claim lock: two goroutines racing Claim
// against two DIFFERENT repos, each with exactly one queued job, must both
// succeed -- a global lock would serialize them into one winner and one
// errNoJobAvailable, which is the wrong answer for two unrelated repos.
//
// Each goroutine RETRIES on errNoJobAvailable rather than asserting success
// on a single attempt, matching how a real caller must use Claim (e.g.
// internal/ingest.Pool's own work() loop: claim, and if nothing came back,
// wait and try again). A single attempt is not guaranteed to succeed even
// when a DIFFERENT repo's job is genuinely available: ClaimIngestJob's
// candidate CTE (internal/db/queries/ingest_jobs.sql) computes "the
// globally oldest claimable job" from its own snapshot, and under two
// truly concurrent claims that snapshot can be taken before either
// transaction's write is visible to the other, so both can legitimately
// compute the SAME candidate (the globally older of the two jobs). Only
// one of them can actually win that row -- the other's outer UPDATE
// correctly matches zero rows (the AND status = 'queued' guard this
// bead's own concurrency tests forced into the query) and reports
// errNoJobAvailable for that attempt, even though the other repo's own
// job was sitting right there unclaimed. That is a one-shot race outcome,
// not starvation: retrying immediately succeeds, because by then the
// winner's job is genuinely 'running' and excluded, leaving the loser's
// retry to correctly land on the other repo's job.
func TestClaim_ConcurrentClaims_DifferentRepos_BothWin(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	repoA := seedRepo(t, ctx)
	repoB := seedRepo(t, ctx)
	jobA, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoA, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	jobB, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoB, TargetBranch: "main", Kind: KindIncremental})
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]Job, 2)
	var start sync.WaitGroup
	start.Add(1)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			const maxAttempts = 20
			for attempt := 0; attempt < maxAttempts; attempt++ {
				job, err := store.Claim(ctx)
				if err == nil {
					results[i] = job
					return
				}
				if !errors.Is(err, errNoJobAvailable) {
					t.Errorf("unexpected Claim error on attempt %d: %v", attempt, err)
					return
				}
			}
			t.Errorf("goroutine %d never claimed a job after %d attempts", i, maxAttempts)
		}(i)
	}
	start.Done()
	wg.Wait()

	got := []uuid.UUID{results[0].ID, results[1].ID}
	assert.ElementsMatch(t, []uuid.UUID{jobA.ID, jobB.ID}, got, "each goroutine must eventually claim the OTHER repo's own job, never the same job twice")
}

// TestClaim_RunningJobWithADeadWorker_BlocksItsOwnRepoOnly is the "worker
// died" scenario the bead's own prompt calls out as worth testing. This
// package deliberately provides no orphan-recovery method of its own
// (internal/ingest.Pool.RequeueOrphaned already exists in a different
// package, using its own hand-written SQL against status='running'
// unconditionally at startup, before any worker could have (re)claimed
// anything -- docs/server-spec.md "Startup" step 4/5 ordering). Deciding
// WHEN a running job's worker is presumed dead is a policy call (crash
// detection, a heartbeat, a startup-only sweep) this store has no
// information to make on its own; it only proves the guard's actual
// behavior in that state, so a future caller integrating orphan recovery
// knows exactly what it is working around: a job stuck in 'running'
// forever blocks ITS OWN repo indefinitely and nothing but an external
// write un-sticks it, while every OTHER repo proceeds normally.
func TestClaim_RunningJobWithADeadWorker_BlocksItsOwnRepoOnly(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	stuckRepo := seedRepo(t, ctx)
	healthyRepo := seedRepo(t, ctx)
	stuck, err := store.Enqueue(ctx, EnqueueParams{RepoID: stuckRepo, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	followUp, err := store.Enqueue(ctx, EnqueueParams{RepoID: stuckRepo, TargetBranch: "main", Kind: KindIncremental})
	require.NoError(t, err)
	healthy, err := store.Enqueue(ctx, EnqueueParams{RepoID: healthyRepo, TargetBranch: "main", Kind: KindIncremental})
	require.NoError(t, err)

	claimedStuck, err := store.Claim(ctx)
	require.NoError(t, err)
	assert.Equal(t, stuck.ID, claimedStuck.ID)
	// Simulate the worker that claimed it crashing: no Complete, no Fail,
	// ever called for this job again.

	claimedHealthy, err := store.Claim(ctx)
	require.NoError(t, err, "a dead worker stuck on one repo must not starve an unrelated repo's own queued job")
	assert.Equal(t, healthy.ID, claimedHealthy.ID)

	_, err = store.Claim(ctx)
	require.Error(t, err, "with no orphan-recovery step run, the stuck repo's own follow-up job stays permanently unclaimable")
	assert.ErrorIs(t, err, errNoJobAvailable)

	stillRunning, err := store.Get(ctx, stuck.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, stillRunning.Status, "nothing in this store ever times out a running job on its own")
	_ = followUp
}

// TestComplete_SetsSucceededStatsAndFinishedAt proves the success path
// persists stats verbatim and stamps finished_at, leaving started_at
// (already set by Claim) untouched.
func TestComplete_SetsSucceededStatsAndFinishedAt(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	_, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	claimed, err := store.Claim(ctx)
	require.NoError(t, err)

	stats := []byte(`{"files_parsed":12,"chunks_embedded":34}`)
	done, err := store.Complete(ctx, claimed.ID, stats)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, done.Status)
	assert.JSONEq(t, string(stats), string(done.Stats))
	assert.NotNil(t, done.FinishedAt)
	assert.NotNil(t, done.StartedAt, "Complete must not clear the started_at Claim set")
	assert.Zero(t, done.Attempts, "a job that succeeds on its first claim must never have touched attempts")
}

// TestComplete_JobNotRunning_ReturnsErrIllegalTransition proves the
// guard: completing a still-queued job (never claimed) is rejected rather
// than silently marked succeeded.
func TestComplete_JobNotRunning_ReturnsErrIllegalTransition(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	job, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	_, err = store.Complete(ctx, job.ID, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
	assert.NotErrorIs(t, err, errNotFound)
}

// TestComplete_UnknownID_ReturnsErrNotFound distinguishes "no such job"
// from "wrong status" -- the same split
// internal/workbranchstore.Store.transitionErr draws.
func TestComplete_UnknownID_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	_, err := store.Complete(ctx, uuid.Must(uuid.NewV7()), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

// TestFail_SetsFailedErrorIncrementsAttemptsAndFinishedAt proves the
// failure path records the error, bumps attempts from 0 to 1 (not touched
// by Claim -- see TestClaim_ClaimsOldestQueuedJobFirst), and stamps
// finished_at.
func TestFail_SetsFailedErrorIncrementsAttemptsAndFinishedAt(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	_, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	claimed, err := store.Claim(ctx)
	require.NoError(t, err)

	failed, err := store.Fail(ctx, claimed.ID, "embedder unreachable")
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, failed.Status)
	require.NotNil(t, failed.Error)
	assert.Equal(t, "embedder unreachable", *failed.Error)
	assert.EqualValues(t, 1, failed.Attempts)
	assert.NotNil(t, failed.FinishedAt)
}

// TestFail_JobNotRunning_ReturnsErrIllegalTransition mirrors Complete's
// guard test.
func TestFail_JobNotRunning_ReturnsErrIllegalTransition(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	job, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	_, err = store.Fail(ctx, job.ID, "boom")
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
}

// TestRequeue_ReturnsFailedJobToQueuedAndReclaimable proves the full
// retry loop: claim, fail (attempts -> 1), requeue, claim again (attempts
// still 1 -- Claim never touches it), fail again (attempts -> 2) -- the
// "attempts incremented on retry" acceptance criterion traced across two
// full cycles, not just one.
func TestRequeue_ReturnsFailedJobToQueuedAndReclaimable(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	enqueued, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)

	claimed1, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, enqueued.ID, claimed1.ID)
	failed1, err := store.Fail(ctx, claimed1.ID, "transient error")
	require.NoError(t, err)
	require.EqualValues(t, 1, failed1.Attempts)

	requeued, err := store.Requeue(ctx, failed1.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, requeued.Status)
	assert.EqualValues(t, 1, requeued.Attempts, "Requeue must not touch attempts itself")

	claimed2, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, enqueued.ID, claimed2.ID)
	assert.EqualValues(t, 1, claimed2.Attempts, "re-claiming must not touch attempts either")

	failed2, err := store.Fail(ctx, claimed2.ID, "transient error again")
	require.NoError(t, err)
	assert.EqualValues(t, 2, failed2.Attempts, "a second failure must climb attempts to 2, proving it accumulates across retries rather than resetting")
}

// TestRequeue_JobNotFailed_ReturnsErrIllegalTransition proves requeuing a
// queued (never claimed) job is rejected, not a silent no-op.
func TestRequeue_JobNotFailed_ReturnsErrIllegalTransition(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	job, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	_, err = store.Requeue(ctx, job.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
}

// TestList_FiltersByRepoAndStatus seeds jobs across two repos with EVERY
// distinct status and target branch (the seeding lesson: an ordering or
// filtering bug must not be masked by fixtures a filter cannot tell
// apart), then proves each repo/status combination returns exactly its
// own rows.
//
// Fixture shape is deliberate, not incidental: repoA gets TWO jobs so the
// single-running-per-repo guard is live during this test too -- claiming
// repoA's first job blocks its second, so reaching a running SECOND job
// for repoA requires completing the first and claiming again, exactly the
// sequence a real worker would follow. bQueued is never claimed at all, so
// it stays the one globally 'queued' row this test can assert
// Status=StatusQueued against without an unrelated Claim call disturbing
// it -- claiming repoA's jobs never touches repoB, since Claim's guard is
// per-repo, not a shared ordering across repos once repoA's own slot is
// occupied.
func TestList_FiltersByRepoAndStatus(t *testing.T) {
	ctx := t.Context()
	store := newTestStore(t)
	repoA := seedRepo(t, ctx)
	repoB := seedRepo(t, ctx)

	aFirst, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoA, TargetBranch: "main", Kind: KindFull})
	require.NoError(t, err)
	aSecond, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoA, TargetBranch: "staging", Kind: KindIncremental})
	require.NoError(t, err)
	bQueued, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoB, TargetBranch: "develop", Kind: KindIncremental})
	require.NoError(t, err)

	claimedA, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, aFirst.ID, claimedA.ID)
	aSucceeded, err := store.Complete(ctx, claimedA.ID, nil)
	require.NoError(t, err, "freeing repoA's running slot so its second job becomes claimable")

	claimedB, err := store.Claim(ctx)
	require.NoError(t, err)
	require.Equal(t, aSecond.ID, claimedB.ID, "repoA's own second-oldest job, now that repoA is free again -- repoB's bQueued must stay untouched")

	result, err := store.List(ctx, ListFilter{RepoID: &repoA}, 0, 0)
	require.NoError(t, err)
	require.Len(t, result.Jobs, 2)
	assert.EqualValues(t, 2, result.Total)

	result, err = store.List(ctx, ListFilter{RepoID: &repoA, Status: StatusRunning}, 0, 0)
	require.NoError(t, err)
	require.Len(t, result.Jobs, 1)
	assert.Equal(t, aSecond.ID, result.Jobs[0].ID)

	result, err = store.List(ctx, ListFilter{RepoID: &repoA, Status: StatusSucceeded}, 0, 0)
	require.NoError(t, err)
	require.Len(t, result.Jobs, 1)
	assert.Equal(t, aSucceeded.ID, result.Jobs[0].ID)

	result, err = store.List(ctx, ListFilter{Status: StatusQueued}, 0, 0)
	require.NoError(t, err)
	require.Len(t, result.Jobs, 1, "bQueued must be the only globally queued row left -- repoA's jobs are now succeeded/running")
	assert.Equal(t, bQueued.ID, result.Jobs[0].ID)

	result, err = store.List(ctx, ListFilter{RepoID: &repoB}, 0, 0)
	require.NoError(t, err)
	require.Len(t, result.Jobs, 1)
	assert.Equal(t, bQueued.ID, result.Jobs[0].ID)
}

// TestList_OrdersNewestQueuedFirstAndPaginates proves List's ordering and
// LIMIT/OFFSET contract, with distinct queued_at times so the order is
// unambiguous.
func TestList_OrdersNewestQueuedFirstAndPaginates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newTestStore(t)
	repoID := seedRepo(t, ctx)
	var ids []uuid.UUID
	for i := range 3 {
		job, err := store.Enqueue(ctx, EnqueueParams{RepoID: repoID, TargetBranch: fmt.Sprintf("branch-%d", i), Kind: KindIncremental})
		require.NoError(t, err)
		ids = append(ids, job.ID)
		time.Sleep(3 * time.Millisecond)
	}

	page1, err := store.List(ctx, ListFilter{RepoID: &repoID}, 2, 0)
	require.NoError(t, err)
	require.Len(t, page1.Jobs, 2)
	assert.EqualValues(t, 3, page1.Total)
	assert.Equal(t, ids[2], page1.Jobs[0].ID, "newest-queued first")
	assert.Equal(t, ids[1], page1.Jobs[1].ID)

	page2, err := store.List(ctx, ListFilter{RepoID: &repoID}, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2.Jobs, 1)
	assert.Equal(t, ids[0], page2.Jobs[0].ID)
}
