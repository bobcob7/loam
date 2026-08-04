//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/workbranchstore/... -v
package workbranchstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// sharedPool is one migrated Postgres for the whole test binary, started
// once in TestMain rather than per test -- one container at a time,
// matching this codebase's other store packages (internal/codegraph,
// internal/chunkstore) and avoiding the concurrent-container-start
// contention their doc comments describe. Every test scopes its rows to
// its own freshly generated repoID; cascading FKs (work_branches.repo_id
// REFERENCES repos ON DELETE CASCADE) keep tests isolated without needing
// a container each.
var sharedPool *pgxpool.Pool

// TestMain starts sharedPool once for the whole package, then tears it
// down after every test has run.
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

// newTestStore returns a Store wired over the package's sharedPool plus a
// freshly seeded repo id this test alone owns.
func newTestStore(t *testing.T) (*Store, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	repoID := uuid.Must(uuid.NewV7())
	_, err := sharedPool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		repoID, "group/workbranchstore-"+repoID.String(),
	)
	require.NoError(t, err)
	return New(gen.New(sharedPool), testLogger()), repoID
}

// openReviewRound inserts a raw review_rounds row for workBranchID -- the
// awaiting-verdict filter's fixture. This package deliberately does not
// depend on internal/reviewstore (the two stores are independent at the
// store layer per this bead's DESIGN), so the fixture goes in directly,
// exactly as internal/reviewstore's own integration tests insert
// work_branches rows directly rather than depending on this package.
func openReviewRound(ctx context.Context, t *testing.T, workBranchID uuid.UUID, number int32) uuid.UUID {
	t.Helper()
	roundID := uuid.Must(uuid.NewV7())
	_, err := sharedPool.Exec(ctx,
		`INSERT INTO review_rounds (id, work_branch_id, number, requested_by) VALUES ($1, $2, $3, $4)`,
		roundID, workBranchID, number, "grace-hopper-3-author")
	require.NoError(t, err)
	return roundID
}

// submitVerdict inserts a raw verdicts row for roundID.
func submitVerdict(ctx context.Context, t *testing.T, roundID uuid.UUID, reviewer, outcome string) {
	t.Helper()
	_, err := sharedPool.Exec(ctx,
		`INSERT INTO verdicts (id, round_id, reviewer, outcome) VALUES ($1, $2, $3, $4)`,
		uuid.Must(uuid.NewV7()), roundID, reviewer, outcome)
	require.NoError(t, err)
}

// TestCreate_UniqueRepoIDName_EnforcedByRealSchema proves
// work_branches_repo_id_name_key (UNIQUE(repo_id, name),
// docs/persistence-spec.md "work_branches") is a real constraint the
// applied migration creates, and that Create maps a hit to
// errDuplicateName -- through the real store, not a raw SQL insert.
func TestCreate_UniqueRepoIDName_EnforcedByRealSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	_, err := store.Create(ctx, repoID, "wb-dup", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.Create(ctx, repoID, "wb-dup", "main", "alan-turing-4-author")
	require.Error(t, err, "a second Create for the same (repo_id, name) must be rejected by the real schema")
	assert.ErrorIs(t, err, errDuplicateName)
}

// TestWorkBranchesUniqueConstraint_RawSQL bypasses the store entirely and
// inserts two work_branches rows for the same (repo_id, name) directly,
// proving work_branches_repo_id_name_key is a real constraint the applied
// migration creates -- not an assumption Create's error-mapping test above
// could pass against a schema that silently dropped it.
func TestWorkBranchesUniqueConstraint_RawSQL(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, repoID := newTestStore(t)
	_, err := sharedPool.Exec(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, author) VALUES (gen_random_uuid(), $1, 'wb-raw-dup', 'main', 'grace-hopper-3-author')`,
		repoID)
	require.NoError(t, err)
	_, err = sharedPool.Exec(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, author) VALUES (gen_random_uuid(), $1, 'wb-raw-dup', 'main', 'alan-turing-4-author')`,
		repoID)
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Equal(t, "work_branches_repo_id_name_key", pgErr.ConstraintName)
}

// TestCreate_SameName_DifferentRepo_Allowed proves identity is (repo_id,
// name) together, not name alone: the same name in two different repos is
// not a conflict.
func TestCreate_SameName_DifferentRepo_Allowed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoA := newTestStore(t)
	_, repoB := newTestStore(t)
	_, err := store.Create(ctx, repoA, "wb-shared-name", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.Create(ctx, repoB, "wb-shared-name", "main", "grace-hopper-3-author")
	assert.NoError(t, err, "the same name in a different repo must not collide")
}

// TestGetByName_ResolvesIdentity proves GetByName resolves the (repo_id,
// name) identity to the row Create just wrote.
func TestGetByName_ResolvesIdentity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	created, err := store.Create(ctx, repoID, "wb-lookup", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	got, err := store.GetByName(ctx, repoID, "wb-lookup")
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
}

// TestLifecycle_DraftToReviewableToReviewedToComplete narrates the
// ordinary, agent-facing lifecycle (docs/cli-spec.md "Its lifecycle") end
// to end through the real store and the real schema: draft ->
// (request-review) -> reviewable -> (first verdict) -> reviewed ->
// (server merges the PR) -> complete.
func TestLifecycle_DraftToReviewableToReviewedToComplete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-lifecycle", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, StateDraft, wb.State)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err, "request-review requires a title and description to already be set")
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err, "request-review: draft -> reviewable")
	assert.Equal(t, StateReviewable, wb.State)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewed)
	require.NoError(t, err, "first verdict: reviewable -> reviewed")
	assert.Equal(t, StateReviewed, wb.State)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err, "re-review: reviewed -> reviewable")
	assert.Equal(t, StateReviewable, wb.State)
	wb, err = store.Complete(ctx, wb.ID)
	require.NoError(t, err, "the server sets complete when the upstream PR merges")
	assert.Equal(t, StateComplete, wb.State)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.Error(t, err, "complete is terminal: no transition out")
	assert.ErrorIs(t, err, ErrIllegalTransition)
}

// TestUpdateState_SkippingReviewable_Rejected proves draft cannot jump
// straight to reviewed, skipping the request-review step -- exactly the
// class of illegal transition this bead's state machine exists to reject.
func TestUpdateState_SkippingReviewable_Rejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-skip", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewed)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIllegalTransition)
	unchanged, err := store.Get(ctx, wb.ID)
	require.NoError(t, err)
	assert.Equal(t, StateDraft, unchanged.State, "a rejected transition must not partially apply")
}

// TestUpdateState_RequestReview_RequiresTitleAndDescription proves
// draft -> reviewable is rejected until both title and description are
// set (docs/cli-spec.md "request-review": "Requires a title and
// description to already be set (via set)") -- a freshly created branch
// has neither (both start NULL), so this is the ordinary case, not an
// edge case.
func TestUpdateState_RequestReview_RequiresTitleAndDescription(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-no-title", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.Error(t, err, "neither title nor description is set yet")
	assert.ErrorIs(t, err, ErrIllegalTransition)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "", "Adds a login form")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.Error(t, err, "description alone is not enough -- title is still empty")
	assert.ErrorIs(t, err, ErrIllegalTransition)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.Error(t, err, "title alone is not enough -- description is now empty")
	assert.ErrorIs(t, err, ErrIllegalTransition)
	_, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err, "both are non-empty now -- request-review must succeed")
}

// TestUpdateState_DemotionNotReachableThroughGenericMethod proves neither
// reviewable -> draft NOR reviewed -> draft is a legal UpdateState
// transition: that demotion only happens through MarkConflicted, which
// also stamps conflict = reset in the same statement.
func TestUpdateState_DemotionNotReachableThroughGenericMethod(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-no-demote", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateDraft)
	require.Error(t, err, "reviewable -> draft must not be reachable through the generic UpdateState")
	assert.ErrorIs(t, err, ErrIllegalTransition)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewed)
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateDraft)
	require.Error(t, err, "reviewed -> draft must not be reachable through the generic UpdateState either")
	assert.ErrorIs(t, err, ErrIllegalTransition)
}

// TestClose_FromDraft_RecordsReason proves the admin-only close path works
// from a non-terminal state and records close_reason.
func TestClose_FromDraft_RecordsReason(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-close", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	closed, err := store.Close(ctx, wb.ID, "superseded by wb-close-v2")
	require.NoError(t, err)
	assert.Equal(t, StateClosed, closed.State)
	require.NotNil(t, closed.CloseReason)
	assert.Equal(t, "superseded by wb-close-v2", *closed.CloseReason)
}

// TestClose_AlreadyClosed_Rejected proves closed is terminal: closing an
// already-closed branch again is rejected, not a silent no-op success.
func TestClose_AlreadyClosed_Rejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-double-close", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.Close(ctx, wb.ID, "first close")
	require.NoError(t, err)
	_, err = store.Close(ctx, wb.ID, "second close")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIllegalTransition)
}

// TestSetTitleDescription_RejectedOnceComplete proves the State gates
// table (docs/cli-spec.md: `set` allowed draft/reviewable/reviewed,
// rejected complete/closed) is enforced by the real guarded UPDATE, not
// just documented.
func TestSetTitleDescription_RejectedOnceComplete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-set-gate", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err, "set must work while draft")
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	wb, err = store.Complete(ctx, wb.ID)
	require.NoError(t, err)
	_, err = store.SetTitleDescription(ctx, wb.ID, "New title", "New description")
	require.Error(t, err, "set must be rejected once the branch is complete")
	assert.ErrorIs(t, err, ErrIllegalTransition)
}

// TestComplete_FromConflictResetDraft_Allowed pins the widened
// CompleteWorkBranch guard (loam-giq.8): a reviewed proposal whose target
// advanced with conflicts is reset all the way back to 'draft'
// (docs/git-spec.md "Target Advances & Catch-Up") while its upstream PR is
// left untouched and open. If that PR then merges, the forge merge is
// authoritative and the branch MUST reach 'complete' from draft -- under
// the original state IN ('reviewable','reviewed') guard it could not, and
// internal/mirrorsync's PR poller would have re-reported the same
// ErrIllegalTransition on every sync tick forever.
//
// This test replaces TestComplete_FromDraft_Rejected, which asserted the
// opposite of this and was the only thing pinning the narrower guard.
func TestComplete_FromConflictResetDraft_Allowed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-complete-draft", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.SetTitleDescription(ctx, wb.ID, "Title", "Description")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewed)
	require.NoError(t, err)
	reset, err := store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err)
	require.Equal(t, StateDraft, reset.State, "precondition: a conflicting advance resets a reviewed branch to draft")
	completed, err := store.Complete(ctx, wb.ID)
	require.NoError(t, err, "a merged upstream PR must complete the branch whatever conflict state it was reset into")
	assert.Equal(t, StateComplete, completed.State)
}

// TestComplete_FromComplete_Rejected proves the widened guard still stops
// at the terminal states: it is state NOT IN ('complete','closed'), not
// "anything goes." Without this, widening the guard could have gone all
// the way to unguarded and nothing here would have noticed.
func TestComplete_FromComplete_Rejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-complete-twice", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.SetTitleDescription(ctx, wb.ID, "Title", "Description")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	_, err = store.Complete(ctx, wb.ID)
	require.NoError(t, err)
	_, err = store.Complete(ctx, wb.ID)
	require.Error(t, err, "a completed branch must not complete again")
	assert.ErrorIs(t, err, ErrIllegalTransition)
}

// TestComplete_FromClosed_Rejected is the other half of the terminal
// guard: a closed branch never becomes complete, so a late-arriving
// "merged" poll can never resurrect a branch the admin already closed.
func TestComplete_FromClosed_Rejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-complete-closed", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.Close(ctx, wb.ID, "admin closed it")
	require.NoError(t, err)
	_, err = store.Complete(ctx, wb.ID)
	require.Error(t, err, "a closed branch must not complete")
	assert.ErrorIs(t, err, ErrIllegalTransition)
	unchanged, err := store.Get(ctx, wb.ID)
	require.NoError(t, err)
	assert.Equal(t, StateClosed, unchanged.State, "a rejected Complete must not partially apply")
}

// TestConflictNarrative_FlaggedDraftRecoversByCatchUp narrates the "a
// draft branch just gains the flag" half of docs/git-spec.md "Target
// Advances & Catch-Up": a draft branch is flagged when the target no
// longer merges cleanly, and a catch-up push clears the flag WITHOUT
// promoting state (it was never demoted -- it stayed draft the whole
// time).
func TestConflictNarrative_FlaggedDraftRecoversByCatchUp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-flag-recover", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	assert.Equal(t, ConflictNone, wb.Conflict)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err, "target advance no longer merges cleanly into this draft branch")
	assert.Equal(t, ConflictFlagged, wb.Conflict)
	assert.Equal(t, StateDraft, wb.State, "a draft branch just gains the flag -- state does not move")
	wb, err = store.ClearConflict(ctx, wb.ID)
	require.NoError(t, err, "a catch-up push brings the branch up to date")
	assert.Equal(t, ConflictNone, wb.Conflict)
	assert.Equal(t, StateDraft, wb.State, "a merely-flagged branch's state is untouched by catch-up")
}

// TestConflictNarrative_DemotedReviewableRestoresDirectly narrates the
// other half of docs/git-spec.md "Target Advances & Catch-Up": a
// reviewable branch hit by a conflicting target advance is "reset to
// draft and flagged as conflicted" in one statement, and a catch-up push
// "flips directly back to reviewable -- no request-review needed".
func TestConflictNarrative_DemotedReviewableRestoresDirectly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-demote-restore", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err, "target advance no longer merges cleanly into this reviewable branch")
	assert.Equal(t, ConflictReset, wb.Conflict)
	assert.Equal(t, StateDraft, wb.State, "reviewable is reset to draft in the same statement")
	wb, err = store.ClearConflict(ctx, wb.ID)
	require.NoError(t, err, "a catch-up push brings the branch up to date")
	assert.Equal(t, ConflictNone, wb.Conflict)
	assert.Equal(t, StateReviewable, wb.State, "a conflict-reset branch flips DIRECTLY back to reviewable -- no request-review")
}

// TestMarkConflicted_FromReviewed_DemotesToResetDraft proves the
// reviewed-branch mirror of the above: an accepted proposal that has
// already collected its first verdict is demoted exactly the same way a
// merely-reviewable one is.
func TestMarkConflicted_FromReviewed_DemotesToResetDraft(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-reviewed-demote", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewed)
	require.NoError(t, err)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err)
	assert.Equal(t, ConflictReset, wb.Conflict)
	assert.Equal(t, StateDraft, wb.State, "reviewed is ALSO reset to draft, same as reviewable")
}

// TestMarkConflicted_Idempotent_OnAlreadyFlaggedDraft proves calling
// MarkConflicted twice on a still-conflicting draft branch is a benign
// no-op the second time, never ErrIllegalTransition -- the mergeability
// checker re-evaluates every open work branch on EVERY target-branch
// advance (docs/git-spec.md "Target Advances & Catch-Up"), so finding the
// same branch still conflicting on a later advance is routine, not
// exotic.
func TestMarkConflicted_Idempotent_OnAlreadyFlaggedDraft(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-reflag", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err)
	assert.Equal(t, ConflictFlagged, wb.Conflict)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err, "a second failed re-check must not error just because the branch was already flagged")
	assert.Equal(t, StateDraft, wb.State)
	assert.Equal(t, ConflictFlagged, wb.Conflict)
}

// TestMarkConflicted_PreservesResetOverFlagged proves the fine-grained
// idempotency rule this bead's fix depends on: re-marking an
// ALREADY-demoted (draft, reset) branch conflicted again (it is still
// draft, and still does not merge) must NOT downgrade conflict from
// 'reset' to 'flagged' -- 'reset' is what tells ClearConflict this branch
// gets restored DIRECTLY to reviewable once it catches up ("the round was
// interrupted, not abandoned"); silently downgrading it here would strand
// the branch as merely-flagged, with no restore-to-reviewable path left.
func TestMarkConflicted_PreservesResetOverFlagged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-preserve-reset", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err)
	require.Equal(t, ConflictReset, wb.Conflict, "precondition: demoted once already")
	require.Equal(t, StateDraft, wb.State)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err, "still draft, still conflicting -- must be a benign no-op")
	assert.Equal(t, StateDraft, wb.State)
	assert.Equal(t, ConflictReset, wb.Conflict, "must NOT be downgraded to 'flagged' -- that would lose the direct-restore-to-reviewable eligibility")
}

// TestMarkConflicted_RejectedFromTerminalState proves complete/closed
// branches -- outside the mergeability check's scope ("each open
// (non-terminal) work branch", docs/git-spec.md) -- reject MarkConflicted
// rather than silently accepting it.
func TestMarkConflicted_RejectedFromTerminalState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-conflict-terminal", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.Close(ctx, wb.ID, "abandoned")
	require.NoError(t, err)
	_, err = store.MarkConflicted(ctx, wb.ID)
	require.Error(t, err, "a closed branch is outside the mergeability check's scope")
	assert.ErrorIs(t, err, ErrIllegalTransition)
}

// TestMarkConflictedSequence_ReconflictWhileReviewable_DemotesInsteadOfError
// is the MUST-FIX regression test: FlagConflict -> UpdateState(reviewable)
// -> a second conflict-mark call must DEMOTE the branch, not error.
//
// Before this bead's fix, FlagWorkBranchConflict/DemoteWorkBranchOnConflict
// were two separate methods, each guarded on conflict = 'none'. That guard
// created a reachable dead end using only this package's own API: (1) mark
// a draft branch conflicted -> (draft, flagged); (2) UpdateState(reviewable)
// succeeds -- its guard carries no conflict predicate at all -- ->
// (reviewable, flagged); (3) the target advances again and the branch still
// does not merge, which docs/git-spec.md ":165" says is routine ("the
// server tests each open (non-terminal) work branch" on EVERY advance, not
// just the first) -- the old DemoteWorkBranchOnConflict required
// conflict = 'none', so this call hit zero rows and returned
// ErrIllegalTransition, leaving the branch STUCK reviewable while
// unmergeable, with no path back to draft.
//
// The fix folds both old methods into one idempotent, level-triggered
// MarkConflicted: its guard is state IN ('draft','reviewable','reviewed')
// only, with NO conflict predicate, so step 3 above now succeeds and
// demotes the branch exactly as it would have from a fresh (reviewable,
// none) branch.
func TestMarkConflictedSequence_ReconflictWhileReviewable_DemotesInsteadOfError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-reconflict-sequence", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err, "step 1: target advance no longer merges cleanly into this draft branch")
	assert.Equal(t, StateDraft, wb.State)
	assert.Equal(t, ConflictFlagged, wb.Conflict)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err, "step 2: request-review has no conflict precondition -- a still-flagged branch may be promoted")
	assert.Equal(t, StateReviewable, wb.State)
	assert.Equal(t, ConflictFlagged, wb.Conflict, "promotion alone does not clear an existing flag")
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err, "step 3 (MUST-FIX): re-checked on the NEXT advance while reviewable and still conflicting -- must demote, not error, even though conflict was 'flagged' rather than 'none'")
	assert.Equal(t, StateDraft, wb.State, "reviewable is reset to draft")
	assert.Equal(t, ConflictReset, wb.Conflict, "reset -- not left at flagged -- so ClearConflict can restore it directly to reviewable")
	wb, err = store.ClearConflict(ctx, wb.ID)
	require.NoError(t, err, "a catch-up push brings the branch up to date")
	assert.Equal(t, StateReviewable, wb.State, "the interrupted round is restored directly -- no request-review needed")
	assert.Equal(t, ConflictNone, wb.Conflict)
}

// TestClearConflict_NoConflictToClear_Rejected proves ClearConflict on an
// already conflict-free branch is rejected, not a silent no-op success --
// there is nothing for a catch-up push to have caught up FROM.
func TestClearConflict_NoConflictToClear_Rejected(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-nothing-to-clear", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.ClearConflict(ctx, wb.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIllegalTransition)
}

// TestList_FiltersByRepoTargetAuthorState proves every plain List filter
// narrows correctly, singly and combined, against real seeded rows.
// Deliberately seeds a THIRD row in this same repo that is NOT draft
// (reviewable): without it, every row in the repo would be draft and a
// State filter's Count-side param going missing (Column4 silently
// mutated to "") would be invisible -- an unfiltered count and a
// state=draft-filtered count would coincidentally both be the same
// number. With a non-draft row present, State: StateDraft's total must be
// LESS than the repo's unfiltered total, so dropping that filter on
// either side is caught by assertion.
func TestList_FiltersByRepoTargetAuthorState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	_, otherRepoID := newTestStore(t)
	wbA, err := store.Create(ctx, repoID, "wb-filter-a", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.Create(ctx, repoID, "wb-filter-b", "develop", "alan-turing-4-author")
	require.NoError(t, err)
	wbC, err := store.Create(ctx, repoID, "wb-filter-c", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wbC, err = store.SetTitleDescription(ctx, wbC.ID, "Add logout", "Adds a logout form")
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wbC.ID, StateReviewable)
	require.NoError(t, err)
	otherStore := New(gen.New(sharedPool), testLogger())
	_, err = otherStore.Create(ctx, otherRepoID, "wb-filter-a", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	rows, total, err := store.List(ctx, ListFilter{RepoID: &repoID}, 100, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total, "repo_id filter narrows to this repo's three branches")
	assert.Len(t, rows, 3)
	rows, total, err = store.List(ctx, ListFilter{RepoID: &repoID, Target: "develop"}, 100, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, rows, 1)
	assert.Equal(t, "wb-filter-b", rows[0].Name)
	rows, total, err = store.List(ctx, ListFilter{RepoID: &repoID, Author: "alan-turing-4-author"}, 100, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, rows, 1)
	assert.Equal(t, "alan-turing-4-author", rows[0].Author)
	rows, total, err = store.List(ctx, ListFilter{RepoID: &repoID, State: StateDraft}, 100, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 2, total, "only wb-filter-a and wb-filter-b are still draft -- wb-filter-c is reviewable")
	assert.Len(t, rows, 2)
	rows, total, err = store.List(ctx, ListFilter{RepoID: &repoID, State: StateReviewable}, 100, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, rows, 1)
	assert.Equal(t, "wb-filter-c", rows[0].Name)
	_ = wbA
}

// TestList_AwaitingVerdictFilter proves the awaiting-caller-verdict filter
// joins review_rounds/verdicts on the CURRENT round only: a reviewable
// branch whose current round has no live verdict yet from the named
// reviewer matches; one that already has a live verdict from that
// reviewer, or one that is not reviewable at all, does not.
func TestList_AwaitingVerdictFilter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	const reviewer = "ada-lovelace-7-reviewer"
	awaiting, err := store.Create(ctx, repoID, "wb-awaiting", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	awaiting, err = store.SetTitleDescription(ctx, awaiting.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	awaiting, err = store.UpdateState(ctx, awaiting.ID, StateReviewable)
	require.NoError(t, err)
	openReviewRound(ctx, t, awaiting.ID, 1)
	voted, err := store.Create(ctx, repoID, "wb-voted", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	voted, err = store.SetTitleDescription(ctx, voted.ID, "Add logout", "Adds a logout form")
	require.NoError(t, err)
	voted, err = store.UpdateState(ctx, voted.ID, StateReviewable)
	require.NoError(t, err)
	votedRound := openReviewRound(ctx, t, voted.ID, 1)
	submitVerdict(ctx, t, votedRound, reviewer, "approve")
	stale, err := store.Create(ctx, repoID, "wb-stale-vote", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	stale, err = store.SetTitleDescription(ctx, stale.ID, "Add signup", "Adds a signup form")
	require.NoError(t, err)
	stale, err = store.UpdateState(ctx, stale.ID, StateReviewable)
	require.NoError(t, err)
	staleRound := openReviewRound(ctx, t, stale.ID, 1)
	submitVerdict(ctx, t, staleRound, reviewer, "approve")
	stale, err = store.UpdateState(ctx, stale.ID, StateReviewed)
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, stale.ID, StateReviewable)
	require.NoError(t, err, "re-review opens round 2, leaving round 1's verdict stale")
	openReviewRound(ctx, t, stale.ID, 2)
	draft, err := store.Create(ctx, repoID, "wb-still-draft", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	rows, total, err := store.List(ctx, ListFilter{RepoID: &repoID, AwaitingVerdictReviewer: reviewer}, 100, 0)
	require.NoError(t, err)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	assert.EqualValues(t, 2, total)
	assert.ElementsMatch(t, []string{"wb-awaiting", "wb-stale-vote"}, names,
		"awaiting: never voted; stale-vote: current round's verdict is a re-review away from ada's old vote")
	assert.NotContains(t, names, "wb-voted", "ada already voted in the current round")
	assert.NotContains(t, names, "wb-still-draft", "a draft branch is never awaiting review")
	_ = draft
}

// TestList_Pagination_ReturnsTotal proves LIMIT/OFFSET pagination and
// CountWorkBranches' total agree (total reflects every matching row, not
// just the page returned), that the three pages are DISJOINT and together
// cover every seeded row exactly once, and that the pages actually follow
// ORDER BY created_at DESC: page1 holds the two most-recently-created
// rows and page3 holds the earliest one. Asserting only totals/lengths
// (this test's original shape) would survive flipping DESC to ASC in
// ListWorkBranches -- the set of rows returned across all three pages
// would be identical, only their distribution across pages would
// silently reverse.
func TestList_Pagination_ReturnsTotal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	created := make([]WorkBranch, 0, 5)
	for i := 0; i < 5; i++ {
		wb, err := store.Create(ctx, repoID, fmt.Sprintf("wb-page-%d", i), "main", "grace-hopper-3-author")
		require.NoError(t, err)
		created = append(created, wb)
	}
	page1, total, err := store.List(ctx, ListFilter{RepoID: &repoID}, 2, 0)
	require.NoError(t, err)
	assert.Len(t, page1, 2)
	assert.EqualValues(t, 5, total)
	page2, total, err := store.List(ctx, ListFilter{RepoID: &repoID}, 2, 2)
	require.NoError(t, err)
	assert.Len(t, page2, 2)
	assert.EqualValues(t, 5, total)
	page3, total, err := store.List(ctx, ListFilter{RepoID: &repoID}, 2, 4)
	require.NoError(t, err)
	assert.Len(t, page3, 1, "the last page holds the remainder")
	assert.EqualValues(t, 5, total)
	seen := make(map[uuid.UUID]int)
	for _, page := range [][]WorkBranch{page1, page2, page3} {
		for _, wb := range page {
			seen[wb.ID]++
		}
	}
	assert.Len(t, seen, 5, "the three pages together must cover every seeded row exactly once")
	for id, count := range seen {
		assert.Equal(t, 1, count, "row %s appeared on more than one page -- pages must be disjoint", id)
	}
	assert.Equal(t, created[4].ID, page1[0].ID, "ORDER BY created_at DESC: the most recently created row is first")
	assert.Equal(t, created[0].ID, page3[0].ID, "the earliest created row must land on the last page")
}

// TestRecordUpstreamPR_WritesTheColumnsAgainstTheRealSchema proves the
// column pair proposal acceptance depends on -- the only input to
// internal/mirrorsync's PR poller -- is really written by the applied
// migration's schema, read back through a fresh Get rather than off the
// RETURNING row the write itself produced.
func TestRecordUpstreamPR_WritesTheColumnsAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-accept-1", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	require.Nil(t, wb.UpstreamPRNumber, "precondition: a fresh work branch has no recorded PR")
	require.Nil(t, wb.AcceptedTip, "precondition: a fresh work branch has no recorded accepted tip")
	_, err = store.RecordUpstreamPR(ctx, wb.ID, "https://example.com/g/r/pulls/7", 7, "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b")
	require.NoError(t, err)
	reread, err := store.Get(ctx, wb.ID)
	require.NoError(t, err)
	require.NotNil(t, reread.UpstreamPRNumber)
	assert.Equal(t, int32(7), *reread.UpstreamPRNumber)
	require.NotNil(t, reread.UpstreamPRURL)
	assert.Equal(t, "https://example.com/g/r/pulls/7", *reread.UpstreamPRURL)
	require.NotNil(t, reread.AcceptedTip, "accepted_tip must round-trip through the real 0004 column, not stay NULL")
	assert.Equal(t, "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", *reread.AcceptedTip)
}

// TestRecordUpstreamPR_SecondWriteIsRefusedByTheRealGuard is proposal
// acceptance's idempotency proven at the layer that actually arbitrates
// it: the NULL predicate in the guarded UPDATE, running against the real
// schema. A second record -- a concurrent accept, or a re-accept whose
// in-memory null-check somehow did not fire -- is refused, and the number
// the FIRST write recorded is still the one on the row.
func TestRecordUpstreamPR_SecondWriteIsRefusedByTheRealGuard(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-accept-2", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.RecordUpstreamPR(ctx, wb.ID, "https://example.com/g/r/pulls/7", 7, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	_, err = store.RecordUpstreamPR(ctx, wb.ID, "https://example.com/g/r/pulls/99", 99, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.ErrorIs(t, err, ErrPRAlreadyRecorded)
	reread, err := store.Get(ctx, wb.ID)
	require.NoError(t, err)
	require.NotNil(t, reread.UpstreamPRNumber)
	assert.Equal(t, int32(7), *reread.UpstreamPRNumber, "the second write must not overwrite the recorded PR")
	require.NotNil(t, reread.UpstreamPRURL)
	assert.Equal(t, "https://example.com/g/r/pulls/7", *reread.UpstreamPRURL)
	require.NotNil(t, reread.AcceptedTip)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", *reread.AcceptedTip, "accepted_tip rides the same guarded UPDATE, so the refused second write must not advance it either")
}

// TestRecordAcceptedTip_RefreshesTheTipAgainstTheRealSchema covers the
// re-accept leg (loam-cgg), the one accept path RecordUpstreamPR's
// upstream_pr_number IS NULL guard refuses by design. Its UPDATE is
// unconditional, so unlike the guarded write above it MUST advance the
// tip on a branch that already carries a PR -- and must leave the
// recorded url/number pair alone while doing it.
func TestRecordAcceptedTip_RefreshesTheTipAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-accept-4", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.RecordUpstreamPR(ctx, wb.ID, "https://example.com/g/r/pulls/7", 7, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	_, err = store.RecordAcceptedTip(ctx, wb.ID, "cccccccccccccccccccccccccccccccccccccccc")
	require.NoError(t, err)
	reread, err := store.Get(ctx, wb.ID)
	require.NoError(t, err)
	require.NotNil(t, reread.AcceptedTip)
	assert.Equal(t, "cccccccccccccccccccccccccccccccccccccccc", *reread.AcceptedTip, "a re-accept must advance the tip, or the branch stays wrongly excluded from the proposal queue")
	require.NotNil(t, reread.UpstreamPRNumber)
	assert.Equal(t, int32(7), *reread.UpstreamPRNumber, "refreshing the tip must not disturb the recorded PR")
}

// TestRecordAcceptedTip_UnknownIDIsNotFound keeps the unconditional
// UPDATE's zero-row case classified, matching the guarded write's.
func TestRecordAcceptedTip_UnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.RecordAcceptedTip(t.Context(), uuid.Must(uuid.NewV7()), "dddddddddddddddddddddddddddddddddddddddd")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRecordUpstreamPR_UnknownIDIsNotFound proves the other zero-row cause
// stays distinguishable against the real schema too.
func TestRecordUpstreamPR_UnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	store, _ := newTestStore(t)
	_, err := store.RecordUpstreamPR(t.Context(), uuid.Must(uuid.NewV7()), "https://example.com/g/r/pulls/7", 7, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NotErrorIs(t, err, ErrPRAlreadyRecorded)
}

// TestUpstreamDrift_DefaultsToNoneAndRoundTripsBothWays proves the three
// halves of loam-giq.11's column that only a real database can show: the
// 0007 migration's NOT NULL DEFAULT 'none' (so no row anywhere carries a
// NULL or the Go zero value), and that the value survives a write and a
// re-read in BOTH directions.
//
// The clear direction is the one worth having a live test for. There is no
// "clear drift" command and no push that clears it -- the operator fixes
// the branch on the forge, which Loam never sees -- so if the column could
// only ever be set, a flagged branch would be unacceptable forever.
func TestUpstreamDrift_DefaultsToNoneAndRoundTripsBothWays(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-drift-roundtrip", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	require.Equal(t, DriftNone, wb.UpstreamDrift, "the column's DEFAULT must make every new row 'none', not NULL or empty")

	flagged, err := store.SetUpstreamDrift(ctx, wb.ID, DriftDiverged)
	require.NoError(t, err)
	assert.Equal(t, DriftDiverged, flagged.UpstreamDrift)
	reread, err := store.Get(ctx, wb.ID)
	require.NoError(t, err)
	assert.Equal(t, DriftDiverged, reread.UpstreamDrift, "the flag must survive a round trip through the database")
	assert.Equal(t, ConflictNone, reread.Conflict, "drift must not touch the conflict column -- they are independent facts")

	cleared, err := store.SetUpstreamDrift(ctx, wb.ID, DriftNone)
	require.NoError(t, err)
	assert.Equal(t, DriftNone, cleared.UpstreamDrift, "an operator who reconciled the branch upstream must be able to get back to 'none'")
}

// TestUpstreamDrift_CoexistsWithAConflict is the whole reason this is its
// own column rather than a fourth `conflict` value: a target can advance
// into a merge conflict while the loam/ branch is separately rewritten.
// With one column, whichever happened second would erase the first and the
// operator would fix one problem while never learning about the other.
func TestUpstreamDrift_CoexistsWithAConflict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-drift-and-conflict", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.SetTitleDescription(ctx, wb.ID, "Add login", "Adds a login form")
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	wb, err = store.MarkConflicted(ctx, wb.ID)
	require.NoError(t, err)
	require.Equal(t, ConflictReset, wb.Conflict)

	drifted, err := store.SetUpstreamDrift(ctx, wb.ID, DriftDiverged)
	require.NoError(t, err)
	assert.Equal(t, DriftDiverged, drifted.UpstreamDrift)
	assert.Equal(t, ConflictReset, drifted.Conflict, "recording drift must not clear the conflict the target advance recorded")
	assert.Equal(t, StateDraft, drifted.State, "nor undo the demotion that came with it")

	restored, err := store.ClearConflict(ctx, wb.ID)
	require.NoError(t, err)
	assert.Equal(t, ConflictNone, restored.Conflict)
	assert.Equal(t, DriftDiverged, restored.UpstreamDrift, "a catch-up push answers the target advance; it says nothing about the branch somebody rewrote upstream")
}

// TestSetUpstreamDrift_IllegalValueNeverReachesTheCheckConstraint proves
// the Go guard and the CHECK constraint agree, by asserting the Go guard
// fires FIRST: a third value comes back as this package's own sentinel,
// not as SQLSTATE 23514. The constraint is still the durable backstop --
// the second half of this test writes past the store, straight to
// Postgres, and shows the database refusing the same value.
func TestSetUpstreamDrift_IllegalValueNeverReachesTheCheckConstraint(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-drift-illegal", "main", "grace-hopper-3-author")
	require.NoError(t, err)

	_, err = store.SetUpstreamDrift(ctx, wb.ID, UpstreamDrift("fast_forward"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errInvalidUpstreamDrift)
	var pgErr *pgconn.PgError
	assert.False(t, errors.As(err, &pgErr), "the value must be rejected in Go, before the statement, not as a constraint violation")

	_, err = sharedPool.Exec(ctx, `UPDATE work_branches SET upstream_drift = 'fast_forward' WHERE id = $1`, wb.ID)
	require.Error(t, err, "the CHECK constraint is the durable backstop under that guard")
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Equal(t, "work_branches_upstream_drift_check", pgErr.ConstraintName)
}
