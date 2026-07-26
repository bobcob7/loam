//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/workbranchstore/... -v
package workbranchstore

import (
	"context"
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
	assert.ErrorIs(t, err, errIllegalTransition)
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
	assert.ErrorIs(t, err, errIllegalTransition)
	unchanged, err := store.Get(ctx, wb.ID)
	require.NoError(t, err)
	assert.Equal(t, StateDraft, unchanged.State, "a rejected transition must not partially apply")
}

// TestUpdateState_DemotionNotReachableThroughGenericMethod proves
// reviewable -> draft is NOT a legal UpdateState transition: that
// demotion only happens through DemoteOnConflict, which also stamps
// conflict = reset in the same statement.
func TestUpdateState_DemotionNotReachableThroughGenericMethod(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-no-demote", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	_, err = store.UpdateState(ctx, wb.ID, StateDraft)
	require.Error(t, err, "reviewable -> draft must not be reachable through the generic UpdateState")
	assert.ErrorIs(t, err, errIllegalTransition)
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
	assert.ErrorIs(t, err, errIllegalTransition)
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
	assert.ErrorIs(t, err, errIllegalTransition)
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
	wb, err = store.FlagConflict(ctx, wb.ID)
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
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	wb, err = store.DemoteOnConflict(ctx, wb.ID)
	require.NoError(t, err, "target advance no longer merges cleanly into this reviewable branch")
	assert.Equal(t, ConflictReset, wb.Conflict)
	assert.Equal(t, StateDraft, wb.State, "reviewable is reset to draft in the same statement")
	wb, err = store.ClearConflict(ctx, wb.ID)
	require.NoError(t, err, "a catch-up push brings the branch up to date")
	assert.Equal(t, ConflictNone, wb.Conflict)
	assert.Equal(t, StateReviewable, wb.State, "a conflict-reset branch flips DIRECTLY back to reviewable -- no request-review")
}

// TestFlagConflict_RejectedFromReviewable proves FlagConflict is only
// legal from a draft branch: a reviewable branch hit by a conflicting
// advance must be DEMOTED (DemoteOnConflict), never merely flagged in
// place, so a caller cannot leave a reviewable branch with a conflict flag
// but no demotion.
func TestFlagConflict_RejectedFromReviewable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-flag-wrong-state", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	wb, err = store.UpdateState(ctx, wb.ID, StateReviewable)
	require.NoError(t, err)
	_, err = store.FlagConflict(ctx, wb.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
}

// TestDemoteOnConflict_RejectedFromDraft proves the inverse: a draft
// branch cannot be "demoted" (it has nothing to demote from) -- only
// FlagConflict applies to a draft branch.
func TestDemoteOnConflict_RejectedFromDraft(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	wb, err := store.Create(ctx, repoID, "wb-demote-wrong-state", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.DemoteOnConflict(ctx, wb.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errIllegalTransition)
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
	assert.ErrorIs(t, err, errIllegalTransition)
}

// TestList_FiltersByRepoTargetAuthorState proves every plain List filter
// narrows correctly, singly and combined, against real seeded rows.
func TestList_FiltersByRepoTargetAuthorState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	_, otherRepoID := newTestStore(t)
	wbA, err := store.Create(ctx, repoID, "wb-filter-a", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	_, err = store.Create(ctx, repoID, "wb-filter-b", "develop", "alan-turing-4-author")
	require.NoError(t, err)
	otherStore := New(gen.New(sharedPool), testLogger())
	_, err = otherStore.Create(ctx, otherRepoID, "wb-filter-a", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	rows, total, err := store.List(ctx, ListFilter{RepoID: &repoID}, 100, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 2, total, "repo_id filter narrows to this repo's two branches")
	assert.Len(t, rows, 2)
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
	assert.EqualValues(t, 2, total, "both seeded rows are still draft")
	assert.Len(t, rows, 2)
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
	awaiting, err = store.UpdateState(ctx, awaiting.ID, StateReviewable)
	require.NoError(t, err)
	openReviewRound(ctx, t, awaiting.ID, 1)
	voted, err := store.Create(ctx, repoID, "wb-voted", "main", "grace-hopper-3-author")
	require.NoError(t, err)
	voted, err = store.UpdateState(ctx, voted.ID, StateReviewable)
	require.NoError(t, err)
	votedRound := openReviewRound(ctx, t, voted.ID, 1)
	submitVerdict(ctx, t, votedRound, reviewer, "approve")
	stale, err := store.Create(ctx, repoID, "wb-stale-vote", "main", "grace-hopper-3-author")
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
// CountWorkBranches' total agree: total reflects every matching row, not
// just the page returned.
func TestList_Pagination_ReturnsTotal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, repoID := newTestStore(t)
	for i := 0; i < 5; i++ {
		_, err := store.Create(ctx, repoID, fmt.Sprintf("wb-page-%d", i), "main", "grace-hopper-3-author")
		require.NoError(t, err)
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
}
