//go:build integration

package storesuite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/db/migrations"
)

// TestStoreSuite_MigrationsUpDownUp_Idempotent is the demo's first beat:
// migrations apply cleanly up AND down and are idempotent
// (docs/testing-spec.md Layer 2 Store). It deliberately does NOT call
// t.Parallel(): it runs a real Down() against sharedDSN, and every other
// test in this package depends on the shared schema being present AND on
// sharedPool being freshly built against the post-cycle schema (see
// main_integration_test.go's sharedPool doc comment for why rebuilding the
// pool after this cycle, not before, matters). Go's test scheduler runs
// every non-parallel top-level test to completion, in file order, before
// any t.Parallel() test's body actually executes past its t.Parallel()
// call -- this file sorts first alphabetically among this package's test
// files, so this is the first test that runs, and it fully finishes
// (including building sharedPool) before TestStoreSuite_HNSW..., ...CTE...,
// ...RoundReviewer..., or ...WorkBranchConflict... read anything.
//
// NOTE ON SCOPE: migrations.Migrate has no "stop after migration N" mode
// (that is loam-maq, a separate follow-up bead) -- it always applies the
// full chain. So this proves the FULL up/down/up cycle is idempotent and
// reversible, not that each individual migration's up/down pair is
// independently idempotent. Per-migration granularity is explicitly out of
// this bead's scope.
func TestStoreSuite_MigrationsUpDownUp_Idempotent(t *testing.T) {
	ctx := t.Context()
	logger := testLogger()
	t.Logf("beat 1/4: schema is already migrated up (TestMain's initial Migrate); running Migrate again to prove the up path is idempotent")
	require.NoError(t, migrations.Migrate(ctx, sharedDSN, logger), "a second Migrate against an already-current schema must be a no-op (migrate.ErrNoChange), not an error")
	assertCoreTablesExist(ctx, t, sharedDSN, "after the idempotent second Migrate")

	t.Logf("beat 2/4: reverting every migration with Down")
	require.NoError(t, migrations.Down(ctx, sharedDSN, logger))
	assertCoreTablesAbsent(ctx, t, sharedDSN, "after Down")

	t.Logf("beat 3/4: re-applying Migrate against the now-empty schema")
	require.NoError(t, migrations.Migrate(ctx, sharedDSN, logger))
	assertCoreTablesExist(ctx, t, sharedDSN, "after the re-applied Migrate")

	t.Logf("beat 4/4: running Down then Migrate a second time, proving the up/down/up cycle itself is repeatable, not a one-shot fluke")
	require.NoError(t, migrations.Down(ctx, sharedDSN, logger))
	require.NoError(t, migrations.Migrate(ctx, sharedDSN, logger))
	assertCoreTablesExist(ctx, t, sharedDSN, "after the second up/down/up cycle")

	pool, err := buildRegisteredPool(ctx, sharedDSN, logger)
	require.NoError(t, err, "building the pgvector-registered pool the rest of this suite shares")
	sharedPool = pool
	t.Logf("schema is migrated up and stable; sharedPool is now built and ready for the rest of this suite")
}

// coreTables is the exact set of tables that must exist after a full
// migrate up (metadata + code-intel, docs/persistence-spec.md), checked
// directly against information_schema so this proves what Migrate actually
// left behind rather than trusting its own bookkeeping.
var coreTables = []string{
	"repos", "repo_target_branches", "credentials", "roles", "role_operations",
	"work_branches", "review_rounds", "verdicts", "threads", "comments", "ingest_jobs",
	"symbols", "symbol_references", "graph_edges", "symbol_history", "chunks",
}

// assertCoreTablesExist proves every metadata + code-intel table exists.
func assertCoreTablesExist(ctx context.Context, t *testing.T, dsn, when string) {
	t.Helper()
	pool := connectPlain(ctx, t, dsn)
	defer pool.Close()
	for _, table := range coreTables {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table,
		).Scan(&exists))
		assert.Truef(t, exists, "%s: expected table %q to exist", when, table)
	}
	var vectorExists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`).Scan(&vectorExists))
	assert.Truef(t, vectorExists, "%s: expected the vector extension to exist", when)
}

// assertCoreTablesAbsent is the down-migration mirror.
func assertCoreTablesAbsent(ctx context.Context, t *testing.T, dsn, when string) {
	t.Helper()
	pool := connectPlain(ctx, t, dsn)
	defer pool.Close()
	for _, table := range coreTables {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table,
		).Scan(&exists))
		assert.Falsef(t, exists, "%s: expected table %q to be dropped", when, table)
	}
}
