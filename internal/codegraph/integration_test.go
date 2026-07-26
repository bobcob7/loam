//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/codegraph/... -v
package codegraph

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
)

// pgvectorImage matches internal/db/migrations's pinned image
// (docs/persistence-spec.md "Deployment"): plain postgres:16-alpine has no
// `vector` extension, and this package's Store methods rely on the real
// 0002_code_intel schema, not a mock, to prove the FKs, CHECK constraints,
// and the CYCLE-clause recursive CTEs actually work.
const pgvectorImage = "pgvector/pgvector:pg16"

// newTestStore spins up a real pgvector-enabled Postgres, applies every
// migration, and returns a Store wired over it plus a seeded repo id every
// test can hang symbols off of. The pool and container are torn down via
// t.Cleanup.
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, pgvectorImage,
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

	repoID := uuid.Must(uuid.NewV7())
	_, err = pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		repoID, "group/codegraph-"+repoID.String(),
	)
	require.NoError(t, err)

	store := New(gen.New(pool), logger)
	return store, pool, repoID
}

// insertSymbol seeds one symbols row directly (bypassing ReplaceFileSymbols)
// so the CTE cycle-safety tests can build an exact graph shape without
// depending on ReplaceFileSymbols/RecomputeGraphEdges' name-resolution
// behavior, which is exercised separately.
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

// TestReplaceFileSymbols_DeleteAndReplace proves the bulk-upsert contract
// against the real schema: a second ReplaceFileSymbols call for the same
// file replaces the first call's rows rather than accumulating them, and a
// different file's symbols are left untouched.
func TestReplaceFileSymbols_DeleteAndReplace(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()

	line1 := int32(1)
	_, err := store.ReplaceFileSymbols(ctx, repoID, "main", "other.go", []SymbolInput{{Line: &line1, Name: "Untouched", Kind: "function"}})
	require.NoError(t, err)

	first, err := store.ReplaceFileSymbols(ctx, repoID, "main", "a.go", []SymbolInput{
		{Line: &line1, Name: "Old", Kind: "function"},
	})
	require.NoError(t, err)
	require.Len(t, first, 1)

	line2 := int32(2)
	second, err := store.ReplaceFileSymbols(ctx, repoID, "main", "a.go", []SymbolInput{
		{Line: &line2, Name: "New", Kind: "function"},
	})
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "New", second[0].Name)
	assert.NotEqual(t, first[0].ID, second[0].ID, "replace must assign a fresh id, not reuse the old row")

	var names []string
	rows, err := pool.Query(ctx, `SELECT name FROM symbols WHERE repo_id = $1 AND file = $2`, repoID, "a.go")
	require.NoError(t, err)
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	rows.Close()
	assert.Equal(t, []string{"New"}, names, "the stale row must be gone, only the replacement remains")

	var untouchedCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM symbols WHERE repo_id = $1 AND file = 'other.go'`, repoID).Scan(&untouchedCount))
	assert.Equal(t, 1, untouchedCount, "replacing a.go must not touch other.go's symbols")
}

// TestReplaceFileReferences_DeleteAndReplace mirrors the symbols case for
// symbol_references.
func TestReplaceFileReferences_DeleteAndReplace(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()

	count, err := store.ReplaceFileReferences(ctx, repoID, "main", "a.go", []ReferenceInput{
		{Name: "Old", Kind: "function", Line: 5},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	count, err = store.ReplaceFileReferences(ctx, repoID, "main", "a.go", []ReferenceInput{
		{Name: "New", Kind: "function", Line: 6},
		{Name: "New2", Kind: "function", Line: 7},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	var names []string
	rows, err := pool.Query(ctx, `SELECT name FROM symbol_references WHERE repo_id = $1 AND file = 'a.go' ORDER BY name`, repoID)
	require.NoError(t, err)
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	rows.Close()
	assert.Equal(t, []string{"New", "New2"}, names)
}

// TestRecomputeGraphEdges_ResolvesByNameAcrossFiles proves the end-to-end
// name-resolution path production ingest will drive: two files' symbols and
// one file's reference to the other file's symbol produce a real
// graph_edges row, with the "from" side approximated as the nearest
// preceding symbol in the referencing file (docs/db/queries/code_graph.sql
// ResolveGraphEdgeCandidates comment).
func TestRecomputeGraphEdges_ResolvesByNameAcrossFiles(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()

	// b.go declares Callee; a.go declares Caller and, at line 10 (inside
	// Caller's approximated body), references "Callee".
	_, err := store.ReplaceFileSymbols(ctx, repoID, "main", "b.go", []SymbolInput{
		{Line: int32Ptr(1), Name: "Callee", Kind: "function"},
	})
	require.NoError(t, err)
	_, err = store.ReplaceFileSymbols(ctx, repoID, "main", "a.go", []SymbolInput{
		{Line: int32Ptr(5), Name: "Caller", Kind: "function"},
	})
	require.NoError(t, err)
	_, err = store.ReplaceFileReferences(ctx, repoID, "main", "a.go", []ReferenceInput{
		{Name: "Callee", Kind: "function", Line: 10},
	})
	require.NoError(t, err)

	inserted, err := store.RecomputeGraphEdges(ctx, repoID, "main")
	require.NoError(t, err)
	assert.Equal(t, int64(1), inserted)

	var fromName, toName string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT sf.name, st.name FROM graph_edges ge
		 JOIN symbols sf ON sf.id = ge.from_symbol_id
		 JOIN symbols st ON st.id = ge.to_symbol_id
		 WHERE ge.repo_id = $1`,
		repoID,
	).Scan(&fromName, &toName))
	assert.Equal(t, "Caller", fromName)
	assert.Equal(t, "Callee", toName)

	// Recomputing again (as every ingest does) must not duplicate the edge.
	insertedAgain, err := store.RecomputeGraphEdges(ctx, repoID, "main")
	require.NoError(t, err)
	assert.Equal(t, int64(1), insertedAgain)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM graph_edges WHERE repo_id = $1`, repoID).Scan(&count))
	assert.Equal(t, 1, count, "recompute must delete stale edges before re-resolving, never accumulate duplicates")
}

func int32Ptr(v int32) *int32 { return &v }

// --- Cycle safety: the hard part. ---
//
// Each subtest below seeds graph_edges directly (bypassing name
// resolution, which is tested separately above) so the graph shape is
// exact, then calls the real Store.Dependents/Deps -- the actual
// CYCLE-clause-guarded queries production and loam-li0.6/loam-ejr will
// run, not a hand-rolled stand-in. Every call is given a bounded context:
// a correctly-guarded query returns well within it, and if the guard were
// ever silently broken, the test fails by timeout/context-deadline rather
// than hanging the suite forever.

const cycleTestTimeout = 15 * time.Second

// TestDependentsCycleSafety_SelfEdge seeds A -> A (a symbol that
// references itself, e.g. straightforward recursion) and proves Dependents
// and Deps both terminate and return exactly {A}.
func TestDependentsCycleSafety_SelfEdge(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), cycleTestTimeout)
	defer cancel()

	a := insertSymbol(ctx, t, pool, repoID, "a.go", "A")
	insertEdge(ctx, t, pool, repoID, a, a)

	deps, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err, "Dependents must terminate on a self-edge, not hang or error")
	assert.ElementsMatch(t, []uuid.UUID{a}, symbolIDs(deps))

	fwd, err := store.Deps(ctx, repoID, "main", a, 0)
	require.NoError(t, err, "Deps must terminate on a self-edge, not hang or error")
	assert.ElementsMatch(t, []uuid.UUID{a}, symbolIDs(fwd))
}

// TestDependentsCycleSafety_MutualRecursion seeds A -> B -> A (the
// is_even/is_odd shape, docs/testing-spec.md "Fixtures") and proves both
// directions terminate with the correct 2-element transitive set,
// regardless of which node the query starts from.
func TestDependentsCycleSafety_MutualRecursion(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), cycleTestTimeout)
	defer cancel()

	a := insertSymbol(ctx, t, pool, repoID, "parity.py", "is_even")
	b := insertSymbol(ctx, t, pool, repoID, "parity.py", "is_odd")
	insertEdge(ctx, t, pool, repoID, a, b)
	insertEdge(ctx, t, pool, repoID, b, a)

	depsOfB, err := store.Dependents(ctx, repoID, "main", b, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{a, b}, symbolIDs(depsOfB), "everything depending on is_odd, transitively, is {is_even, is_odd}")

	depsOfA, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{a, b}, symbolIDs(depsOfA), "starting from the other node in the cycle must be symmetric")

	fwdOfA, err := store.Deps(ctx, repoID, "main", a, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{a, b}, symbolIDs(fwdOfA), "is_even's transitive deps are {is_even, is_odd}")
}

// TestDependentsCycleSafety_LongerCycle seeds A -> B -> C -> A and proves
// the guard also handles a cycle longer than a pair, which a naive
// "compare only to the immediately-previous node" guard would miss.
func TestDependentsCycleSafety_LongerCycle(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), cycleTestTimeout)
	defer cancel()

	a := insertSymbol(ctx, t, pool, repoID, "a.go", "A")
	b := insertSymbol(ctx, t, pool, repoID, "b.go", "B")
	c := insertSymbol(ctx, t, pool, repoID, "c.go", "C")
	insertEdge(ctx, t, pool, repoID, a, b)
	insertEdge(ctx, t, pool, repoID, b, c)
	insertEdge(ctx, t, pool, repoID, c, a)

	deps, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err, "Dependents must terminate on a 3-node cycle")
	assert.ElementsMatch(t, []uuid.UUID{a, b, c}, symbolIDs(deps))

	fwd, err := store.Deps(ctx, repoID, "main", b, 0)
	require.NoError(t, err, "Deps must terminate on a 3-node cycle")
	assert.ElementsMatch(t, []uuid.UUID{a, b, c}, symbolIDs(fwd))
}

// TestDependentsCycleSafety_StartOutsideCycle seeds D -> A with A -> B -> C
// -> A forming a cycle reachable from D, but D itself outside it. Deps(D)
// must walk into the cycle and still terminate with the correct set
// {A, B, C} (D is excluded: Deps returns what D depends on, not D itself).
func TestDependentsCycleSafety_StartOutsideCycle(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), cycleTestTimeout)
	defer cancel()

	a := insertSymbol(ctx, t, pool, repoID, "a.go", "A")
	b := insertSymbol(ctx, t, pool, repoID, "b.go", "B")
	c := insertSymbol(ctx, t, pool, repoID, "c.go", "C")
	d := insertSymbol(ctx, t, pool, repoID, "d.go", "D")
	insertEdge(ctx, t, pool, repoID, a, b)
	insertEdge(ctx, t, pool, repoID, b, c)
	insertEdge(ctx, t, pool, repoID, c, a)
	insertEdge(ctx, t, pool, repoID, d, a)

	fwd, err := store.Deps(ctx, repoID, "main", d, 0)
	require.NoError(t, err, "Deps starting outside the cycle must still terminate")
	assert.ElementsMatch(t, []uuid.UUID{a, b, c}, symbolIDs(fwd), "D's transitive deps are the whole cycle, not D itself")

	// And the reverse direction, queried from inside the cycle, must also
	// see D as a dependent (it depends on A) alongside the rest of the
	// cycle.
	deps, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{a, b, c, d}, symbolIDs(deps), "everything depending on A, transitively, includes D from outside the cycle")
}

// TestDependentsCTE_GuardRemovedHangs is the mutation test: it runs the
// EXACT graph fixture that TestDependentsCycleSafety_MutualRecursion
// proves terminates correctly, but through a hand-inlined copy of the
// Dependents query with the CYCLE clause deleted and no depth/row cap
// substituted in its place. This is the "break your own code" proof the
// wave-5 brief asks for: it demonstrates the CYCLE clause is the thing
// making the real query terminate, not some other incidental property of
// the fixture or the query shape.
//
// This intentionally fails by TIMEOUT (context deadline), not by
// assertion -- per the brief, that is the weaker of the two signals a
// mutation can produce (a hang/panic proves less than a clean assertion
// failure, since in principle a slow-but-finite query could look
// indistinguishable from a truly infinite one within any fixed bound). It
// is included anyway because for THIS specific property -- does recursion
// terminate at all -- a bounded-context timeout is the only observable
// difference available: there is no well-typed "assert not infinite"
// short of actually waiting. The companion tests above assert the
// *correct finite result* by clean equality, which is the strong signal;
// this test only adds "and removing the guard entirely breaks that",
// isolating the guard as load-bearing rather than redundant.
func TestDependentsCTE_GuardRemovedHangs(t *testing.T) {
	t.Parallel()
	_, pool, repoID := newTestStore(t)
	// A short bound: on a healthy machine the guarded query returns in
	// milliseconds (asserted by the other subtests, unbounded save for
	// cycleTestTimeout), so 3s comfortably separates "terminates" from
	// "does not" without making the suite slow.
	const guardRemovedBound = 3 * time.Second
	bg := context.Background()

	a := insertSymbol(bg, t, pool, repoID, "parity.py", "is_even")
	b := insertSymbol(bg, t, pool, repoID, "parity.py", "is_odd")
	insertEdge(bg, t, pool, repoID, a, b)
	insertEdge(bg, t, pool, repoID, b, a)

	// Identical to the Dependents query in internal/db/queries/code_graph.sql
	// EXCEPT the "CYCLE symbol_id SET is_cycle USING visited_path" clause is
	// gone and nothing replaces it -- no depth cap, no row cap. If this
	// still terminated quickly, it would mean the CYCLE clause was never
	// the thing stopping the real query, i.e. the guard was decorative.
	const unguarded = `
WITH RECURSIVE dependents(symbol_id, depth) AS (
    SELECT ge.from_symbol_id, 1
    FROM graph_edges ge
    WHERE ge.repo_id = $1 AND ge.target_branch = $2 AND ge.to_symbol_id = $3
  UNION ALL
    SELECT ge.from_symbol_id, d.depth + 1
    FROM graph_edges ge
    JOIN dependents d ON ge.to_symbol_id = d.symbol_id
    WHERE ge.repo_id = $1 AND ge.target_branch = $2
)
SELECT DISTINCT symbol_id FROM dependents LIMIT $4`

	ctx, cancel := context.WithTimeout(bg, guardRemovedBound)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rows, err := pool.Query(ctx, unguarded, repoID, "main", b, 100000)
		if err != nil {
			done <- err
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				done <- err
				return
			}
		}
		done <- rows.Err()
	}()

	select {
	case err := <-done:
		t.Fatalf("expected the guardless query to hang past %s, but it returned (err=%v) -- the CYCLE clause may not be the thing terminating the real query, or this mutation no longer reproduces the cycle", guardRemovedBound, err)
	case <-ctx.Done():
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "the guardless query must still be running when the bound expires")
		t.Logf("confirmed: without the CYCLE clause, the identical mutual-recursion fixture does not terminate within %s (timeout-shaped failure, the weaker signal -- see doc comment)", guardRemovedBound)
	}
}

// symbolIDs extracts the symbol ids from a Dependency slice for order-
// independent set comparisons.
func symbolIDs(deps []Dependency) []uuid.UUID {
	ids := make([]uuid.UUID, len(deps))
	for i, d := range deps {
		ids[i] = d.Symbol.ID
	}
	return ids
}

// TestSymbolHistory_AppendAndQuery proves append/query round-trips and
// that SymbolHistory orders most-recent-first via the UUID v7 primary key
// (no timestamp column exists on symbol_history, docs/persistence-spec.md).
func TestSymbolHistory_AppendAndQuery(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	sym := insertSymbol(ctx, t, pool, repoID, "a.go", "A")

	count, err := store.AppendSymbolHistory(ctx, []HistoryEntryInput{
		{SymbolID: sym, Commit: "c1", Ref: "main", Message: "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	// A real clock tick between inserts keeps UUID v7's embedded timestamp
	// strictly increasing even on platforms with coarse monotonic
	// resolution, so the ORDER BY id DESC assertion below is unambiguous.
	time.Sleep(2 * time.Millisecond)
	count, err = store.AppendSymbolHistory(ctx, []HistoryEntryInput{
		{SymbolID: sym, Commit: "c2", Ref: "main", Message: "second"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	entries, err := store.History(ctx, sym, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "c2", entries[0].Commit, "most recent commit must come first")
	assert.Equal(t, "c1", entries[1].Commit)

	limited, err := store.History(ctx, sym, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, "c2", limited[0].Commit)
}

// TestSymbolHistory_CascadesWithSymbolDelete corroborates loam-05j: unlike
// every other code-intel table, symbol_history carries no repo_id/
// target_branch of its own (0002_code_intel.up.sql), so repo-scoped
// deletion only reaches it transitively -- deleting the owning symbol (and
// transitively, the owning repo) must cascade to symbol_history via its
// symbol_id FK.
func TestSymbolHistory_CascadesWithSymbolDelete(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	sym := insertSymbol(ctx, t, pool, repoID, "a.go", "A")
	_, err := store.AppendSymbolHistory(ctx, []HistoryEntryInput{{SymbolID: sym, Commit: "c1", Ref: "main", Message: "first"}})
	require.NoError(t, err)

	var before int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM symbol_history WHERE symbol_id = $1`, sym).Scan(&before))
	require.Equal(t, 1, before)

	_, err = pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, repoID)
	require.NoError(t, err)

	var after int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM symbol_history WHERE symbol_id = $1`, sym).Scan(&after))
	assert.Equal(t, 0, after, "deleting the owning repo must cascade through symbols down to symbol_history")
}
