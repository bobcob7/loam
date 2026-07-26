//go:build integration

package storesuite

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreSuite_WorkBranchConflict_ColumnTransitions proves
// work_branches.conflict -- the column, and the CHECK constraint the
// migration puts on it -- honors the transition chain this bead's DESIGN
// names: none -> flagged -> reset -> none (docs/git-spec.md "Target
// Advances & Catch-Up"; docs/persistence-spec.md "work_branches").
//
// SCOPE, READ BEFORE EXTENDING THIS TEST: this is a SCHEMA-level proof
// only, via raw UPDATE statements, not a store-level one. There is no
// work_branches store yet -- loam-54o.10 ("work_branches store") is still
// OPEN as of this bead, and its own NOTES say the conflict transitions
// belong to it ("SPEC CORRECTION: add conflict (none/flagged/reset)
// transitions..."). loam-li0.6 does not depend on loam-54o.10, so there is
// no Store.FlagConflict/ResetConflict/ClearConflict method to call here.
// What this test CAN and does prove: the column exists with the right
// default, the CHECK constraint accepts exactly {none, flagged, reset} and
// rejects anything else, and a raw UPDATE can walk the full chain the
// DESIGN names. What it deliberately does NOT prove: which transitions are
// legal application-level business rules (e.g. that a conflict reaching
// 'reset' must ALSO demote work_branches.state to 'draft', or that only a
// 'reset' branch -- not a 'flagged' one -- flips back to 'reviewable' on
// catch-up per git-spec). That state-machine enforcement is loam-54o.10's
// job; see DEFERRED-WIP in this package's doc comment
// (main_integration_test.go).
func TestStoreSuite_WorkBranchConflict_ColumnTransitions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := mustPool(t)
	repoID := insertRepo(ctx, t, pool, "group/demo-conflict-repo")
	var branchID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, author)
		 VALUES (gen_random_uuid(), $1, 'wb-conflict-demo', 'main', 'grace-hopper-3-author')
		 RETURNING id`,
		repoID,
	).Scan(&branchID))

	var initial string
	require.NoError(t, pool.QueryRow(ctx, `SELECT conflict FROM work_branches WHERE id = $1`, branchID).Scan(&initial))
	t.Logf("work branch created: conflict = %q (default)", initial)
	assert.Equal(t, "none", initial, "conflict defaults to 'none' per docs/persistence-spec.md")

	t.Logf("target advances and no longer merges cleanly: conflict -> 'flagged'")
	_, err := pool.Exec(ctx, `UPDATE work_branches SET conflict = 'flagged' WHERE id = $1`, branchID)
	require.NoError(t, err)
	assertConflict(ctx, t, pool, branchID, "flagged")

	t.Logf("the conflicted branch was reviewable/reviewed and gets demoted: conflict -> 'reset'")
	_, err = pool.Exec(ctx, `UPDATE work_branches SET conflict = 'reset' WHERE id = $1`, branchID)
	require.NoError(t, err)
	assertConflict(ctx, t, pool, branchID, "reset")

	t.Logf("a catch-up push brings the branch back up to date: conflict -> 'none'")
	_, err = pool.Exec(ctx, `UPDATE work_branches SET conflict = 'none' WHERE id = $1`, branchID)
	require.NoError(t, err)
	assertConflict(ctx, t, pool, branchID, "none")

	t.Logf("an arbitrary conflict value must be rejected by the real CHECK constraint")
	_, err = pool.Exec(ctx, `UPDATE work_branches SET conflict = 'bogus' WHERE id = $1`, branchID)
	require.Error(t, err, "work_branches_conflict_check must reject a value outside {none, flagged, reset}")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "work_branches_conflict_check", pgErr.ConstraintName)
	t.Logf("rejected as expected: constraint %s", pgErr.ConstraintName)
	assertConflict(ctx, t, pool, branchID, "none", "the rejected update must not have partially applied")
}

// assertConflict reads work_branches.conflict for branchID and asserts it
// equals want, logging the observed value either way.
func assertConflict(ctx context.Context, t *testing.T, pool *pgxpool.Pool, branchID string, want string, msgAndArgs ...any) {
	t.Helper()
	var got string
	require.NoError(t, pool.QueryRow(ctx, `SELECT conflict FROM work_branches WHERE id = $1`, branchID).Scan(&got))
	assert.Equal(t, want, got, msgAndArgs...)
}
