//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon. Run with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/reviewpublish/... -v
//
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround, not a CI
// setting). One shared pgvector container for the whole binary, started in
// TestMain -- not one per test.
//
// This package's whole reason to exist is a property no mock can establish:
// that a verdict publishes its comments ATOMICALLY. Everything here is
// therefore run against a real Postgres, and the two central tests
// (TestPublish_InvisibleToConcurrentReadersUntilCommit and
// TestPublish_RejectedResolve_PublishesNothing) are written so they fail if
// the publish were ever rewritten as a sequence of autocommitted writes.
//
// DEFERRED-WIP: reviewing.feature: "Submitting a verdict publishes staged
// comments atomically with an outcome" and "Staged comments are not visible
// until submitted" -> the SERVER half of both is proved here. Their CLI half
// (local .loam staging, loam-0pj.12/.13) does not exist yet, so the godog
// scenarios stay @wip.
//
// DEFERRED-WIP: reviewing.feature / work-branch-lifecycle.feature: "The
// first verdict marks the work branch reviewed" ->
// TestPublish_FirstVerdictFlipsReviewableToReviewed.
package reviewpublish

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// sharedDSN is the one migrated Postgres this whole test binary uses,
// started once in TestMain. Tests open their own pools against it, so a
// test that needs a genuinely separate connection (the concurrent reader
// below) gets one.
var sharedDSN string

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
	sharedDSN = dsn
	code := m.Run()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared pgvector container:", err)
	}
	os.Exit(code)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newPool opens an independent pool against the shared container.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), sharedDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// seedWorkBranch inserts a repo and a titled work branch already
// in the given state with one open review round, returning the work branch
// id and that round. Titles/descriptions are set because the reviewable
// transitions guard on them, and the publish's own reviewable -> reviewed
// flip goes through the same guarded UPDATE.
func seedWorkBranch(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, state workbranchstore.State, withRound bool) (uuid.UUID, reviewstore.Round) {
	t.Helper()
	repoID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		repoID, "bobcob7/"+name)
	require.NoError(t, err)
	workBranchID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, title, description, author, state)
		 VALUES ($1, $2, $3, 'main', 'Add login', 'Adds a login flow.', 'grace-hopper-3-author', $4)`,
		workBranchID, repoID, name, string(state))
	require.NoError(t, err)
	if !withRound {
		return workBranchID, reviewstore.Round{}
	}
	round, err := reviewstore.NewRoundStore(pool, testLogger()).OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)
	return workBranchID, round
}

// countThreads reads the number of threads on a work branch through pool --
// deliberately a raw count, not a store call, so the assertion is about
// what is COMMITTED and visible, with no store logic in between.
func countThreads(ctx context.Context, t *testing.T, pool *pgxpool.Pool, workBranchID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM threads WHERE work_branch_id = $1`, workBranchID).Scan(&n))
	return n
}

// countVerdicts reads the number of verdicts across all of a work branch's
// rounds through pool.
func countVerdicts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, workBranchID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM verdicts v JOIN review_rounds r ON r.id = v.round_id WHERE r.work_branch_id = $1`,
		workBranchID).Scan(&n))
	return n
}

// readState reads a work branch's state through pool.
func readState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, workBranchID uuid.UUID) string {
	t.Helper()
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM work_branches WHERE id = $1`, workBranchID).Scan(&state))
	return state
}

// TestPublish_InvisibleToConcurrentReadersUntilCommit is the central
// acceptance for reviewing.feature's "Staged comments are not visible until
// submitted" and "publishes staged comments atomically", at the only
// boundary where the server can enforce it: nothing a verdict writes may be
// visible to anyone until the whole verdict commits.
//
// It is arranged so the publisher is DETERMINISTICALLY caught mid-
// transaction rather than sampled and hoped-for. A blocker transaction on
// its own connection takes SELECT ... FOR NO KEY UPDATE on the
// work_branches row first. The publish then runs in a goroutine: it inserts
// its threads, comments and verdict, and parks on the final
// reviewable -> reviewed UPDATE, which needs that same FOR NO KEY UPDATE
// lock. The test waits -- via pg_blocking_pids() naming the blocker's own
// backend, a condition that only becomes true once the publisher is
// genuinely parked, never merely "not there yet" -- and only then reads. So
// a reader seeing zero threads at that instant is observing MVCC
// invisibility of ALREADY-WRITTEN rows, not a writer that has not started.
// Releasing the blocker lets the publish commit, and the same reader then
// sees everything.
//
// FOR NO KEY UPDATE, not FOR UPDATE, is load-bearing and was found by
// mutation-testing this test: threads.work_branch_id's foreign-key check
// takes a FOR KEY SHARE lock on the same row, which FOR UPDATE conflicts
// with and FOR NO KEY UPDATE does not. Under FOR UPDATE the publisher
// parked on its very FIRST comment INSERT, so the "reader sees nothing"
// assertion held even when the comments were deliberately moved OUT of the
// transaction -- the test passed against an implementation it exists to
// reject. FOR NO KEY UPDATE lets every insert through and stops only the
// state flip, which is the only arrangement under which reading zero
// threads actually means anything.
//
// Nothing here single-samples a value that could legitimately be either
// way: before the release the rows exist-but-are-invisible by construction,
// and after it the publish's own returned error (checked) plus a second
// read establish visibility.
func TestPublish_InvisibleToConcurrentReadersUntilCommit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	writerPool, blockerPool, readerPool := newPool(t), newPool(t), newPool(t)
	workBranchID, _ := seedWorkBranch(ctx, t, writerPool, "invisible-repo", workbranchstore.StateReviewable, true)

	blocker, err := blockerPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback(ctx) }()
	var blockerPID int32
	require.NoError(t, blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	var locked uuid.UUID
	require.NoError(t, blocker.QueryRow(ctx, `SELECT id FROM work_branches WHERE id = $1 FOR NO KEY UPDATE`, workBranchID).Scan(&locked))

	done := make(chan error, 1)
	go func() {
		_, err := New(writerPool, testLogger()).Publish(context.WithoutCancel(ctx), Request{
			WorkBranchID: workBranchID,
			Reviewer:     "ada-lovelace-7-reviewer",
			Outcome:      reviewstore.OutcomeApprove,
			Comments: []NewComment{
				{Body: "needs a guard"},
				{Body: "and a test"},
			},
		})
		done <- err
	}()

	// Waiting on "some backend is blocked BY OUR BLOCKER specifically", not
	// on "some backend somewhere is blocked": these tests share one Postgres
	// with every other test in this binary, so an unqualified
	// pg_blocking_pids() check could be satisfied by an unrelated waiter and
	// would let the assertions below run before the publish had written
	// anything -- which is exactly the unfalsifiable shape loam-4q2 is about.
	require.Eventually(t, func() bool {
		var blocked int
		if err := readerPool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&blocked); err != nil {
			return false
		}
		return blocked > 0
	}, 30*time.Second, 20*time.Millisecond, "the publish must reach its final work_branches UPDATE and park on the blocker's row lock")

	assert.Equal(t, 0, countThreads(ctx, t, readerPool, workBranchID), "the publish has already INSERTed both threads inside its transaction, and a concurrent reader must still see none of them")
	assert.Equal(t, 0, countVerdicts(ctx, t, readerPool, workBranchID), "nor the verdict")

	require.NoError(t, blocker.Rollback(ctx))
	require.NoError(t, <-done, "once the lock is released the publish commits cleanly")
	assert.Equal(t, 2, countThreads(ctx, t, readerPool, workBranchID), "and only now are the comments visible")
	assert.Equal(t, 1, countVerdicts(ctx, t, readerPool, workBranchID))
	assert.Equal(t, "reviewed", readState(ctx, t, readerPool, workBranchID))
}

// TestPublish_RejectedResolve_PublishesNothing is reviewing.feature's "Only
// the thread's author may resolve it", proved as a ROLLBACK rather than as
// an error message: the request carries two comments and a resolve of
// somebody else's thread. The comments are written first (see Publish's
// documented step order), the resolve then fails, and the assertion is that
// the database contains NEITHER comment and no verdict afterwards.
//
// If the publish were a sequence of autocommitted store calls -- the
// implementation this package exists to rule out -- both comments would be
// permanently visible here and this test would fail on the thread count,
// not on the error.
func TestPublish_RejectedResolve_PublishesNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newPool(t)
	workBranchID, round := seedWorkBranch(ctx, t, pool, "rollback-repo", workbranchstore.StateReviewable, true)
	threads := reviewstore.NewThreadStore(pool, testLogger())
	theirs, err := threads.OpenThread(ctx, workBranchID, round.ID, round.Number, "alan-turing-4-reviewer", nil, nil, "raised by someone else")
	require.NoError(t, err)
	require.Equal(t, 1, countThreads(ctx, t, pool, workBranchID))

	_, err = New(pool, testLogger()).Publish(ctx, Request{
		WorkBranchID:     workBranchID,
		Reviewer:         "ada-lovelace-7-reviewer",
		Outcome:          reviewstore.OutcomeApprove,
		Comments:         []NewComment{{Body: "needs a guard"}, {Body: "and a test"}},
		ResolveThreadIDs: []uuid.UUID{theirs.ID},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, reviewstore.ErrNotThreadAuthor)
	assert.Equal(t, 1, countThreads(ctx, t, pool, workBranchID), "the two comments written before the rejected resolve must have been rolled back")
	assert.Equal(t, 0, countVerdicts(ctx, t, pool, workBranchID), "and no verdict recorded")
	assert.Equal(t, "reviewable", readState(ctx, t, pool, workBranchID), "and the work branch not flipped to reviewed")
	var resolved bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT resolved FROM threads WHERE id = $1`, theirs.ID).Scan(&resolved))
	assert.False(t, resolved)
}

// TestPublish_FirstVerdictFlipsReviewableToReviewed is
// work-branch-lifecycle.feature's and reviewing.feature's "The first verdict
// marks the work branch reviewed", plus the follow-on the spec's wording
// invites getting wrong: a SECOND reviewer's verdict in the same round must
// succeed against the now-reviewed branch, not fail an illegal transition.
func TestPublish_FirstVerdictFlipsReviewableToReviewed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newPool(t)
	workBranchID, _ := seedWorkBranch(ctx, t, pool, "flip-repo", workbranchstore.StateReviewable, true)
	publisher := New(pool, testLogger())

	first, err := publisher.Publish(ctx, Request{WorkBranchID: workBranchID, Reviewer: "ada-lovelace-7-reviewer", Outcome: reviewstore.OutcomeApprove})
	require.NoError(t, err)
	assert.Equal(t, workbranchstore.StateReviewed, first.State)
	assert.Equal(t, "reviewed", readState(ctx, t, pool, workBranchID))

	second, err := publisher.Publish(ctx, Request{WorkBranchID: workBranchID, Reviewer: "alan-turing-4-reviewer", Outcome: reviewstore.OutcomeDisapprove})
	require.NoError(t, err, "a second verdict in the same round lands on an already-reviewed branch and must not be rejected")
	assert.Equal(t, workbranchstore.StateReviewed, second.State)
	assert.Equal(t, 2, countVerdicts(ctx, t, pool, workBranchID))
}

// TestPublish_ResubmissionReplacesInPlace is reviewing.feature's
// "Re-submitting replaces my verdict for the round": the same reviewer
// submitting twice in one round leaves ONE verdict row carrying the later
// outcome, never two.
func TestPublish_ResubmissionReplacesInPlace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newPool(t)
	workBranchID, _ := seedWorkBranch(ctx, t, pool, "resubmit-repo", workbranchstore.StateReviewable, true)
	publisher := New(pool, testLogger())
	_, err := publisher.Publish(ctx, Request{WorkBranchID: workBranchID, Reviewer: "ada-lovelace-7-reviewer", Outcome: reviewstore.OutcomeDisapprove})
	require.NoError(t, err)
	_, err = publisher.Publish(ctx, Request{WorkBranchID: workBranchID, Reviewer: "ada-lovelace-7-reviewer", Outcome: reviewstore.OutcomeApprove})
	require.NoError(t, err)
	assert.Equal(t, 1, countVerdicts(ctx, t, pool, workBranchID))
	records, err := reviewstore.NewVerdictStore(pool, testLogger()).List(ctx, workBranchID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, reviewstore.OutcomeApprove, records[0].Outcome)
}

// TestPublish_SecondRoundMakesPriorVerdictsStale is
// work-branch-lifecycle.feature's "Requesting review again starts a fresh
// round and marks prior verdicts stale" and reviewing.feature's "Verdicts
// and comments record their review round": after a second round opens, the
// round-1 verdict reads stale and the round-2 publish's verdict AND its
// comment both record round 2. Nothing writes a stale flag anywhere -- there
// is none; staleness is the round-number comparison, recomputed on read.
func TestPublish_SecondRoundMakesPriorVerdictsStale(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newPool(t)
	workBranchID, round1 := seedWorkBranch(ctx, t, pool, "stale-repo", workbranchstore.StateReviewable, true)
	publisher := New(pool, testLogger())
	_, err := publisher.Publish(ctx, Request{WorkBranchID: workBranchID, Reviewer: "alan-turing-4-reviewer", Outcome: reviewstore.OutcomeApprove})
	require.NoError(t, err)

	// Requesting review again: reviewed -> reviewable plus a fresh round,
	// exactly what the RequestReview handler does.
	_, err = pool.Exec(ctx, `UPDATE work_branches SET state = 'reviewable' WHERE id = $1`, workBranchID)
	require.NoError(t, err)
	round2, err := reviewstore.NewRoundStore(pool, testLogger()).OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)
	require.Equal(t, int32(2), round2.Number)

	result, err := publisher.Publish(ctx, Request{
		WorkBranchID: workBranchID,
		Reviewer:     "ada-lovelace-7-reviewer",
		Outcome:      reviewstore.OutcomeApprove,
		Comments:     []NewComment{{Body: "raised in the second round"}},
	})
	require.NoError(t, err)
	assert.Equal(t, round2.ID, result.Round.ID, "the verdict lands in the CURRENT round")
	assert.Equal(t, int32(2), result.Round.Number)

	records, err := reviewstore.NewVerdictStore(pool, testLogger()).List(ctx, workBranchID)
	require.NoError(t, err)
	require.Len(t, records, 2)
	byReviewer := map[string]reviewstore.VerdictRecord{}
	for _, record := range records {
		byReviewer[record.Reviewer] = record
	}
	assert.True(t, byReviewer["ada-lovelace-7-reviewer"].Current, "the round-2 verdict is current")
	assert.False(t, byReviewer["alan-turing-4-reviewer"].Current, "the round-1 verdict is now stale, without anything having written a flag")
	assert.Equal(t, round1.ID, byReviewer["alan-turing-4-reviewer"].RoundID)

	threads, _, err := reviewstore.NewThreadStore(pool, testLogger()).List(ctx, workBranchID, 100, 0)
	require.NoError(t, err)
	require.Len(t, threads, 1)
	assert.Equal(t, int32(2), threads[0].RoundNumber, "the published comment is recorded against the second round")
	require.Len(t, threads[0].Comments, 1)
	assert.Equal(t, int32(2), threads[0].Comments[0].RoundNumber)
}

// TestPublish_StateGate covers docs/cli-spec.md's State gates row for
// `verdict`: allowed in reviewable and reviewed, rejected in draft (which
// has no round at all) and in the terminal complete/closed. The gate lives
// inside the publish transaction, so each rejection must also leave nothing
// behind -- asserted here via the comment that accompanies every attempt.
func TestPublish_StateGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		state     workbranchstore.State
		withRound bool
		wantErr   error
	}{
		{name: "draft has no round yet", state: workbranchstore.StateDraft, withRound: false, wantErr: ErrNotOpenForReview},
		{name: "complete is terminal", state: workbranchstore.StateComplete, withRound: true, wantErr: ErrNotOpenForReview},
		{name: "closed is terminal", state: workbranchstore.StateClosed, withRound: true, wantErr: ErrNotOpenForReview},
		{name: "reviewable accepts a verdict", state: workbranchstore.StateReviewable, withRound: true},
		{name: "reviewed accepts a verdict", state: workbranchstore.StateReviewed, withRound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			pool := newPool(t)
			workBranchID, _ := seedWorkBranch(ctx, t, pool, "gate-"+uuid.NewString()[:8], tc.state, tc.withRound)
			_, err := New(pool, testLogger()).Publish(ctx, Request{
				WorkBranchID: workBranchID,
				Reviewer:     "ada-lovelace-7-reviewer",
				Outcome:      reviewstore.OutcomeApprove,
				Comments:     []NewComment{{Body: "a comment that must not survive a rejection"}},
			})
			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, 1, countThreads(ctx, t, pool, workBranchID))
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, 0, countThreads(ctx, t, pool, workBranchID), "a rejected verdict publishes nothing")
		})
	}
}

// TestPublish_ReviewableWithNoRound_ReportsNoCurrentRound covers the
// interrupted-RequestReview shape internal/handler/workbranch's RoundStore
// doc comment describes: a reviewable branch whose OpenRound never landed.
// The publish must report reviewstore.ErrNoCurrentRound (which the handler
// maps to a failed precondition) rather than inventing a round of its own.
func TestPublish_ReviewableWithNoRound_ReportsNoCurrentRound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newPool(t)
	workBranchID, _ := seedWorkBranch(ctx, t, pool, "roundless-repo", workbranchstore.StateReviewable, false)
	_, err := New(pool, testLogger()).Publish(ctx, Request{
		WorkBranchID: workBranchID,
		Reviewer:     "ada-lovelace-7-reviewer",
		Outcome:      reviewstore.OutcomeApprove,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, reviewstore.ErrNoCurrentRound)
	var rounds int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM review_rounds WHERE work_branch_id = $1`, workBranchID).Scan(&rounds))
	assert.Equal(t, 0, rounds, "the publish must not open a round of its own -- only a transition into reviewable does that")
}

// TestPublish_UnknownWorkBranch_ReportsNotFound proves an id naming no work
// branch surfaces workbranchstore.ErrNotFound, which the handler maps to
// CodeNotFound, rather than a raw pgx error collapsing to CodeInternal.
func TestPublish_UnknownWorkBranch_ReportsNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newPool(t)
	_, err := New(pool, testLogger()).Publish(ctx, Request{
		WorkBranchID: uuid.New(),
		Reviewer:     "ada-lovelace-7-reviewer",
		Outcome:      reviewstore.OutcomeApprove,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, workbranchstore.ErrNotFound)
}
