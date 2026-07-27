//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon. Run with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/reviewstore/... -v
//
// (see internal/db/migrations/integration_test.go for why TESTCONTAINERS_RYUK_DISABLED
// is a podman-only workaround, not a CI setting).
//
// These tests apply the REAL migration set (migrations.Migrate against
// 0001_init.up.sql), so review_rounds_work_branch_id_number_key and
// verdicts_round_id_reviewer_key are the actual constraints Postgres
// enforces -- not a hand-rolled test schema that could silently drift
// from what ships.
//
// DEFERRED-WIP: reviewing.feature: Verdicts and comments record their review
// round -> TestReviewRounds_DerivedStaleness_Narrative,
// TestList_MultipleReviewersSameRound_OneRecordEach (covers the VERDICT half
// only: a verdict carries its round_id and List surfaces RoundNumber. Does
// NOT cover "the published comment is recorded against the second round" --
// threads/comments is loam-54o.12's table group, and publication is
// loam-ofg.9's handler. The godog scenario stays @wip pending loam-li0.5.)
//
// DEFERRED-WIP: replies.feature: A reply records the round it was made in ->
// no test in this bead covers it. This bead contributes only the
// review_rounds row a reply's round_id would be stamped against
// (RoundStore.CurrentRound); the reply write path itself belongs to
// loam-54o.12 (threads/comments store) and loam-ofg.9 (handler). The godog
// scenario stays @wip pending loam-li0.5.)
package reviewstore

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
)

// newTestPool spins up a real Postgres via testcontainers-go, applies the
// production migration set, and returns a connected pool with cleanup
// registered.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, "pgvector/pgvector:pg16",
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.Background()))
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// newWorkBranch inserts a minimal repos row and a work_branches row under
// it, returning the work branch's id -- the only fixture review_rounds
// and verdicts need, since neither table cares about the work branch's
// state.
func newWorkBranch(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		repoID, "bobcob7/"+name)
	require.NoError(t, err)
	workBranchID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, author)
		 VALUES ($1, $2, $3, 'main', 'grace-hopper-3-author')`,
		workBranchID, repoID, name)
	require.NoError(t, err)
	return workBranchID
}

// TestReviewRounds_DerivedStaleness_Narrative is this bead's central
// acceptance, told as a narrative rather than split into micro-assertions,
// per the brief: a reviewer approves round 1; the work branch reopens for
// a second round; the round-1 verdict is no longer current -- and this is
// computed fresh on every read, never flipped by anyone writing a stale
// flag (there is none).
func TestReviewRounds_DerivedStaleness_Narrative(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	rounds := NewRoundStore(pool, testLogger())
	verdicts := NewVerdictStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "narrative-repo")

	// Round 1 opens (author requests review); Ada approves it.
	round1, err := rounds.OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, int32(1), round1.Number)
	_, err = verdicts.Submit(ctx, round1.ID, "ada-lovelace-7-reviewer", OutcomeApprove)
	require.NoError(t, err)

	current, err := rounds.CurrentRound(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, round1.ID, current.ID, "round 1 is current: it is the branch's only round")
	count, err := verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "round 1's approve counts while it is current")

	// The admin sends the branch back, or the author requests review
	// again: round 2 opens.
	round2, err := rounds.OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, int32(2), round2.Number)

	// Nothing wrote a stale flag on Ada's round-1 verdict. Yet it is no
	// longer current -- because the current round moved.
	current, err = rounds.CurrentRound(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, round2.ID, current.ID, "round 2 is now current")

	records, err := verdicts.List(ctx, workBranchID)
	require.NoError(t, err)
	require.Len(t, records, 1, "Ada's round-1 verdict is still the only verdict recorded")
	assert.Equal(t, round1.ID, records[0].RoundID)
	assert.Equal(t, int32(1), records[0].RoundNumber)
	assert.False(t, records[0].Current, "Ada's verdict belongs to round 1, which is no longer current")

	// And it no longer counts toward the approval bar: the proposal queue
	// must not credit a stale approval as current.
	count, err = verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "round 1's approve no longer counts once round 2 is current")

	// Alan approves the new round; now the count reflects round 2 only.
	_, err = verdicts.Submit(ctx, round2.ID, "alan-turing-4-reviewer", OutcomeApprove)
	require.NoError(t, err)
	count, err = verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "only the current round's approve counts")
}

// TestSubmit_Resubmission_ReplacesNotDuplicates proves
// UNIQUE(round_id, reviewer) is honored the way Demo M1 shows it off:
// re-submitting for the same round and reviewer replaces the prior
// verdict in place, never creating a second row. It also proves a
// retracted approval actually retracts: changing the same reviewer's
// verdict from approve to disapprove within one round must drop
// CurrentRoundApproveCount back to 0, not leave a stale approve
// counted -- a mutation that widens the count's outcome filter (e.g.
// `<> 'neutral'` instead of `= 'approve'`) would let a disapprove keep
// counting and this assertion is what catches it.
func TestSubmit_Resubmission_ReplacesNotDuplicates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	rounds := NewRoundStore(pool, testLogger())
	verdicts := NewVerdictStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "resubmit-repo")
	round, err := rounds.OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)

	first, err := verdicts.Submit(ctx, round.ID, "ada-lovelace-7-reviewer", OutcomeDisapprove)
	require.NoError(t, err)
	second, err := verdicts.Submit(ctx, round.ID, "ada-lovelace-7-reviewer", OutcomeApprove)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "re-submission replaces the same row rather than inserting a new one")

	records, err := verdicts.List(ctx, workBranchID)
	require.NoError(t, err)
	require.Len(t, records, 1, "exactly one verdict row exists for this reviewer+round")
	assert.Equal(t, OutcomeApprove, records[0].Outcome, "the latest submission's outcome is what is recorded")

	var rowCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM verdicts WHERE round_id = $1 AND reviewer = $2`,
		pgUUID(round.ID), "ada-lovelace-7-reviewer",
	).Scan(&rowCount))
	assert.Equal(t, 1, rowCount, "the real table has exactly one row -- proves the upsert path, not just the store's view of it")

	count, err := verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "Ada's approve counts toward the approval bar")

	_, err = verdicts.Submit(ctx, round.ID, "ada-lovelace-7-reviewer", OutcomeDisapprove)
	require.NoError(t, err)
	count, err = verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "retracting the approval (same reviewer, same round, now disapprove) must drop the count -- a stale approve must not keep counting")
}

// TestVerdictsUniqueConstraint_EnforcedByRealSchema bypasses the store
// entirely and inserts two verdicts rows for the same (round_id,
// reviewer) directly, proving verdicts_round_id_reviewer_key is a real
// constraint the applied migration creates -- not an assumption this
// package's tests could pass against a schema that silently dropped it.
func TestVerdictsUniqueConstraint_EnforcedByRealSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	rounds := NewRoundStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "raw-constraint-repo")
	round, err := rounds.OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO verdicts (id, round_id, reviewer, outcome) VALUES ($1, $2, $3, 'approve')`,
		pgUUID(uuid.New()), pgUUID(round.ID), "ada-lovelace-7-reviewer")
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO verdicts (id, round_id, reviewer, outcome) VALUES ($1, $2, $3, 'disapprove')`,
		pgUUID(uuid.New()), pgUUID(round.ID), "ada-lovelace-7-reviewer")
	require.Error(t, err, "a second raw insert for the same (round_id, reviewer) must be rejected by the real schema")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, "verdicts_round_id_reviewer_key", pgErr.ConstraintName)
}

// TestReviewRoundsUniqueConstraint_EnforcedByRealSchema is the
// review_rounds mirror of the above: two rounds for the same work branch
// sharing a number is rejected by review_rounds_work_branch_id_number_key.
func TestReviewRoundsUniqueConstraint_EnforcedByRealSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	workBranchID := newWorkBranch(ctx, t, pool, "raw-round-constraint-repo")

	_, err := pool.Exec(ctx,
		`INSERT INTO review_rounds (id, work_branch_id, number, requested_by) VALUES ($1, $2, 1, $3)`,
		pgUUID(uuid.New()), pgUUID(workBranchID), "grace-hopper-3-author")
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO review_rounds (id, work_branch_id, number, requested_by) VALUES ($1, $2, 1, $3)`,
		pgUUID(uuid.New()), pgUUID(workBranchID), "grace-hopper-3-author")
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, "review_rounds_work_branch_id_number_key", pgErr.ConstraintName)
}

// TestOpenRound_SequentialNumbering proves OpenRound assigns 1, 2, 3, ...
// in the order rounds are opened for one work branch, and that a second
// work branch's numbering starts independently at 1.
func TestOpenRound_SequentialNumbering(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	rounds := NewRoundStore(pool, testLogger())
	branchA := newWorkBranch(ctx, t, pool, "sequential-repo-a")
	branchB := newWorkBranch(ctx, t, pool, "sequential-repo-b")

	r1, err := rounds.OpenRound(ctx, branchA, "grace-hopper-3-author")
	require.NoError(t, err)
	r2, err := rounds.OpenRound(ctx, branchA, "grace-hopper-3-author")
	require.NoError(t, err)
	r3, err := rounds.OpenRound(ctx, branchA, "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, []int32{1, 2, 3}, []int32{r1.Number, r2.Number, r3.Number})

	rB, err := rounds.OpenRound(ctx, branchB, "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, int32(1), rB.Number, "a different work branch's numbering is independent")
}

// TestCurrentRound_NoRoundsYet_ReturnsErrNoCurrentRound proves a work
// branch that has never had review requested reports the distinguishable
// ErrNoCurrentRound against the real schema, not a bare pgx.ErrNoRows.
func TestCurrentRound_NoRoundsYet_ReturnsErrNoCurrentRound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	rounds := NewRoundStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "no-rounds-repo")

	_, err := rounds.CurrentRound(ctx, workBranchID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoCurrentRound)
}

// TestCurrentRoundApproveCount_NoRoundsYet_IsZero proves the approve
// count is simply 0 for a work branch with no rounds, not an error --
// CurrentRoundApproveCount backs the proposal queue, which must not fail
// just because a branch has never been reviewed.
func TestCurrentRoundApproveCount_NoRoundsYet_IsZero(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	verdicts := NewVerdictStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "no-rounds-count-repo")

	count, err := verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

// TestList_MultipleReviewersSameRound_OneRecordEach proves List surfaces
// each reviewer's verdict once, not duplicated, and that a disapprove or
// neutral outcome in the current round is excluded from
// CurrentRoundApproveCount even though it is listed.
func TestList_MultipleReviewersSameRound_OneRecordEach(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	rounds := NewRoundStore(pool, testLogger())
	verdicts := NewVerdictStore(pool, testLogger())
	workBranchID := newWorkBranch(ctx, t, pool, "multi-reviewer-repo")
	round, err := rounds.OpenRound(ctx, workBranchID, "grace-hopper-3-author")
	require.NoError(t, err)

	_, err = verdicts.Submit(ctx, round.ID, "ada-lovelace-7-reviewer", OutcomeApprove)
	require.NoError(t, err)
	_, err = verdicts.Submit(ctx, round.ID, "alan-turing-4-reviewer", OutcomeNeutral)
	require.NoError(t, err)

	records, err := verdicts.List(ctx, workBranchID)
	require.NoError(t, err)
	require.Len(t, records, 2)
	for _, r := range records {
		assert.True(t, r.Current)
	}
	count, err := verdicts.CurrentRoundApproveCount(ctx, workBranchID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "only Ada's approve counts; Alan's neutral does not")
}
