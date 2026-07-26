//go:build integration

package storesuite

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/db/gen"
)

// insertSymbol seeds one symbols row directly, bypassing name resolution
// (tested separately by internal/codegraph's own suite), so this demo's
// graph shape is exact and unambiguous.
func insertSymbol(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID, file, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx,
		`INSERT INTO symbols (id, repo_id, target_branch, file, line, name, kind) VALUES ($1, $2, 'main', $3, 1, $4, 'function')`,
		id, repoID, file, name,
	)
	require.NoError(t, err)
	return id
}

// insertEdge seeds one graph_edges row directly: from depends on to.
func insertEdge(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID, from, to uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO graph_edges (id, repo_id, target_branch, from_symbol_id, to_symbol_id, kind) VALUES ($1, $2, 'main', $3, $4, 'dependency')`,
		uuid.Must(uuid.NewV7()), repoID, from, to,
	)
	require.NoError(t, err)
}

// TestStoreSuite_DependentsCTE_TerminatesOnMutualRecursionCycle is Demo
// M1's third live proof: the dependents recursive CTE terminates on a
// mutual-recursion cycle instead of looping forever. It calls
// internal/codegraph's real Store.Dependents -- the actual CYCLE-clause-
// guarded query (`CYCLE symbol_id SET is_cycle USING visited_path`,
// internal/db/queries/code_graph.sql) production runs -- against the
// is_even/is_odd fixture shape docs/testing-spec.md's "Fixtures" section
// names. The mutation proof that the CYCLE clause is load-bearing (not
// decorative) already lives in internal/codegraph's own
// TestDependentsCTE_GuardRemovedHangs and is not repeated here; this test
// reuses the real store, narrated, for the cross-store demo.
//
// A bounded context is given so that if the guard were ever silently
// broken, this test fails by timeout rather than hanging the whole suite
// forever -- consistent with every other cycle-safety test in this repo.
func TestStoreSuite_DependentsCTE_TerminatesOnMutualRecursionCycle(t *testing.T) {
	t.Parallel()
	pool := mustPool(t)
	store := codegraph.New(gen.New(pool), testLogger())
	repoID := insertRepo(t.Context(), t, pool, "group/demo-cte-repo")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	t.Logf("seeding a mutual-recursion cycle: is_even depends on is_odd, is_odd depends on is_even")
	isEven := insertSymbol(ctx, t, pool, repoID, "parity.py", "is_even")
	isOdd := insertSymbol(ctx, t, pool, repoID, "parity.py", "is_odd")
	insertEdge(ctx, t, pool, repoID, isEven, isOdd)
	insertEdge(ctx, t, pool, repoID, isOdd, isEven)

	start := time.Now()
	deps, truncated, err := store.Dependents(ctx, repoID, "main", isOdd, 0)
	elapsed := time.Since(start)
	require.NoError(t, err, "the CYCLE-guarded recursive CTE must terminate on a mutual-recursion cycle, not hang or error")
	t.Logf("Dependents(is_odd) returned in %s, terminated=true, truncated=%v", elapsed, truncated)

	names := symbolNames(deps)
	t.Logf("dependents of is_odd, transitively: %v", names)
	assert.ElementsMatch(t, []string{"is_even", "is_odd"}, names, "everything depending on is_odd, transitively, is exactly {is_even, is_odd} -- not an infinite walk")
	assert.False(t, truncated)
}

// symbolNames extracts symbol names from a Dependency slice for
// order-independent assertions.
func symbolNames(deps []codegraph.Dependency) []string {
	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.Symbol.Name
	}
	return names
}
