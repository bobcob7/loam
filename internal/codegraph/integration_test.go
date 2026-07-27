//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/codegraph/... -v
package codegraph

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

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// sharedPool is one migrated pgvector-backed Postgres for the whole test
// binary, started once in TestMain rather than per test. Every test below
// scopes its rows to its own freshly generated repoID (cascading FKs mean a
// DELETE FROM repos in one test can never touch another test's rows), so
// sharing one container/pool across tests is safe and is not a shortcut on
// isolation. It is a direct fix for a real failure observed on first run:
// starting an independent container per test (this package has 11 test
// functions, all t.Parallel()) drove 11 concurrent container starts at the
// local podman/docker daemon and the whole run blew the -timeout 300s
// budget without a single test failing on its own merits -- see this
// bead's final report for the raw goroutine dump that showed exactly that
// (every stuck goroutine was inside testcontainers'/moby's container-start
// HTTP call, not inside any query this package runs).
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
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	pool := sharedPool

	repoID := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx,
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

// TestRecomputeGraphEdges_DuplicateReferencesProduceOneEdge is FIX 3's
// proof: a symbol referencing the same name twice in one file (a common,
// ordinary case -- e.g. calling the same function twice) must resolve to
// exactly one graph_edges row for that (from, to) pair, not one per
// reference. Before FIX 3 (ResolveGraphEdgeCandidates without DISTINCT),
// this test fails with count == 2: harmless-looking in isolation, but the
// Dependents/Deps recursive CTEs join through graph_edges with UNION ALL,
// so k parallel edges between the same pair multiply the branch count k
// times per hop of recursion -- the bug compounds even though DISTINCT ON
// hides it from the final output.
func TestRecomputeGraphEdges_DuplicateReferencesProduceOneEdge(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()

	_, err := store.ReplaceFileSymbols(ctx, repoID, "main", "b.go", []SymbolInput{
		{Line: int32Ptr(1), Name: "Callee", Kind: "function"},
	})
	require.NoError(t, err)
	_, err = store.ReplaceFileSymbols(ctx, repoID, "main", "a.go", []SymbolInput{
		{Line: int32Ptr(5), Name: "Caller", Kind: "function"},
	})
	require.NoError(t, err)
	// Caller references Callee twice -- e.g. two call sites in one
	// function body.
	_, err = store.ReplaceFileReferences(ctx, repoID, "main", "a.go", []ReferenceInput{
		{Name: "Callee", Kind: "function", Line: 10},
		{Name: "Callee", Kind: "function", Line: 12},
	})
	require.NoError(t, err)

	inserted, err := store.RecomputeGraphEdges(ctx, repoID, "main")
	require.NoError(t, err)
	assert.Equal(t, int64(1), inserted, "two references to the same name from the same enclosing symbol must resolve to exactly one edge candidate")

	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM graph_edges WHERE repo_id = $1`, repoID).Scan(&count))
	assert.Equal(t, 1, count, "graph_edges must hold exactly one row for the (Caller, Callee) pair, not one per duplicate reference")
}

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

	deps, truncated, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err, "Dependents must terminate on a self-edge, not hang or error")
	assert.False(t, truncated)
	assert.ElementsMatch(t, []uuid.UUID{a}, symbolIDs(deps))

	fwd, truncated, err := store.Deps(ctx, repoID, "main", a, 0)
	require.NoError(t, err, "Deps must terminate on a self-edge, not hang or error")
	assert.False(t, truncated)
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

	depsOfB, _, err := store.Dependents(ctx, repoID, "main", b, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{a, b}, symbolIDs(depsOfB), "everything depending on is_odd, transitively, is {is_even, is_odd}")

	depsOfA, _, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{a, b}, symbolIDs(depsOfA), "starting from the other node in the cycle must be symmetric")

	fwdOfA, _, err := store.Deps(ctx, repoID, "main", a, 0)
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

	deps, _, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err, "Dependents must terminate on a 3-node cycle")
	assert.ElementsMatch(t, []uuid.UUID{a, b, c}, symbolIDs(deps))

	fwd, _, err := store.Deps(ctx, repoID, "main", b, 0)
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

	fwd, _, err := store.Deps(ctx, repoID, "main", d, 0)
	require.NoError(t, err, "Deps starting outside the cycle must still terminate")
	assert.ElementsMatch(t, []uuid.UUID{a, b, c}, symbolIDs(fwd), "D's transitive deps are the whole cycle, not D itself")

	// And the reverse direction, queried from inside the cycle, must also
	// see D as a dependent (it depends on A) alongside the rest of the
	// cycle.
	deps, _, err := store.Dependents(ctx, repoID, "main", a, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{a, b, c, d}, symbolIDs(deps), "everything depending on A, transitively, includes D from outside the cycle")
}

// TestDependents_NearestDepthFirst_NotUUIDOrder is FIX 2's proof: a capped
// Dependents call must keep the NEAREST symbols (smallest depth), not an
// arbitrary subset picked by symbol UUID order. It builds a graph where
// UUID creation order and depth order actively disagree -- Root <-
// Mid <- Deep is a 2-hop chain (Deep is a depth-2 dependent of Root), and
// five more symbols (Shallow1..Shallow5) each depend on Root directly
// (depth 1), but are created AFTER Deep, so their UUIDv7s sort later than
// Deep's. Root has 6 depth-1 dependents total (Mid + 5 Shallows), so a
// Dependents(Root, limit=3) call has more than enough depth-1 candidates
// to fill its quota -- Deep (depth 2) must never appear in the result.
// Before FIX 2 (ORDER BY s.id, d.depth applied directly after DISTINCT ON
// (s.id), i.e. truncating in UUID order), Deep's early-created UUID put it
// ahead of some Shallow nodes despite being strictly farther away, so it
// wrongly appeared in a limit-3 result while a genuine depth-1 dependent
// was dropped -- an assertion failure, not a timeout, and a strictly
// stronger check than "the query returned".
func TestDependents_NearestDepthFirst_NotUUIDOrder(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), cycleTestTimeout)
	defer cancel()

	root := insertSymbol(ctx, t, pool, repoID, "root.go", "Root")
	mid := insertSymbol(ctx, t, pool, repoID, "mid.go", "Mid")
	deep := insertSymbol(ctx, t, pool, repoID, "deep.go", "Deep")
	insertEdge(ctx, t, pool, repoID, mid, root) // Mid depends on Root (depth 1)
	insertEdge(ctx, t, pool, repoID, deep, mid) // Deep depends on Mid, so transitively on Root (depth 2)

	var shallow []uuid.UUID
	for i := range 5 {
		s := insertSymbol(ctx, t, pool, repoID, fmt.Sprintf("shallow%d.go", i), fmt.Sprintf("Shallow%d", i))
		insertEdge(ctx, t, pool, repoID, s, root) // depth 1, created after deep
		shallow = append(shallow, s)
	}

	deps, truncated, err := store.Dependents(ctx, repoID, "main", root, 3)
	require.NoError(t, err)
	assert.True(t, truncated, "6 depth-1 dependents exist for a limit of 3")
	require.Len(t, deps, 3)
	for _, d := range deps {
		assert.Equal(t, int32(1), d.Depth, "every returned symbol must be depth 1: Root has 6 depth-1 dependents, more than enough to fill limit=3 without ever reaching Deep")
		assert.NotEqual(t, deep, d.Symbol.ID, "Deep (depth 2) must not appear ahead of a depth-1 dependent just because its UUID sorts earlier")
	}
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

	a := insertSymbol(t.Context(), t, pool, repoID, "parity.py", "is_even")
	b := insertSymbol(t.Context(), t, pool, repoID, "parity.py", "is_odd")
	insertEdge(t.Context(), t, pool, repoID, a, b)
	insertEdge(t.Context(), t, pool, repoID, b, a)

	// Identical to the Dependents query in internal/db/queries/code_graph.sql
	// EXCEPT the "CYCLE symbol_id SET is_cycle USING visited_path" clause is
	// gone and nothing replaces it -- no depth cap, no row cap. If this
	// still terminated quickly, it would mean the CYCLE clause was never
	// the thing stopping the real query, i.e. the guard was decorative.
	// The LIMIT $4 here is deliberately generous (100000, passed below) and
	// deliberately NOT a stand-in cycle guard: it sits downstream of the
	// DISTINCT ON dedup subquery (a blocking sort node, same as the real
	// query), so it cannot short-circuit the recursive term even in
	// principle -- see FIX 2's reasoning in code_graph.sql's Dependents
	// comment, which applies identically to this inlined copy.
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
SELECT s.id
FROM (
    SELECT DISTINCT ON (symbol_id) symbol_id, depth
    FROM dependents
    ORDER BY symbol_id, depth
) deduped
JOIN symbols s ON s.id = deduped.symbol_id
ORDER BY deduped.depth, s.id
LIMIT $4`

	ctx, cancel := context.WithTimeout(t.Context(), guardRemovedBound)
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

	entries, truncated, err := store.History(ctx, sym, 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, entries, 2)
	assert.Equal(t, "c2", entries[0].Commit, "most recent commit must come first")
	assert.Equal(t, "c1", entries[1].Commit)

	limited, truncated, err := store.History(ctx, sym, 1)
	require.NoError(t, err)
	assert.True(t, truncated, "asking for 1 of 2 entries must report truncated=true")
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

// --- LookupSymbolsByName (loam-awr): name -> []Symbol, the not-found
// signal a handler needs to produce docs/cli-spec.md's exit 3, and the
// resolution step Dependents/Deps/History need before they can accept a
// name instead of a uuid.UUID. ---

// insertRepo seeds an additional repos row -- distinct from newTestStore's
// own repo -- so scoping tests can build a second, out-of-scope repo
// sharing the same sharedPool/Store.
func insertRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		id, name,
	)
	require.NoError(t, err)
	return id
}

// insertSymbolOnBranch mirrors insertSymbol but lets the caller pick
// target_branch, for tests that must prove branch scoping specifically
// (insertSymbol hardcodes 'main').
func insertSymbolOnBranch(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID, targetBranch, file, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx,
		`INSERT INTO symbols (id, repo_id, target_branch, file, line, name, kind) VALUES ($1, $2, $3, $4, 1, $5, 'function')`,
		id, repoID, targetBranch, file, name,
	)
	require.NoError(t, err)
	return id
}

// insertReference seeds one symbol_references row directly (bypassing
// ReplaceFileReferences), mirroring insertSymbol, so
// LookupReferencesByName's scoping tests can build exact fixtures without
// depending on the delete-and-replace path exercised separately.
func insertReference(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID, file, name string, line int32) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx,
		`INSERT INTO symbol_references (id, repo_id, target_branch, file, name, kind, line) VALUES ($1, $2, 'main', $3, $4, 'call', $5)`,
		id, repoID, file, name, line,
	)
	require.NoError(t, err)
	return id
}

// insertReferenceOnBranch mirrors insertSymbolOnBranch for
// symbol_references, letting the caller pick target_branch.
func insertReferenceOnBranch(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID, targetBranch, file, name string, line int32) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx,
		`INSERT INTO symbol_references (id, repo_id, target_branch, file, name, kind, line) VALUES ($1, $2, $3, $4, $5, 'call', $6)`,
		id, repoID, targetBranch, file, name, line,
	)
	require.NoError(t, err)
	return id
}

// referenceIDsFromReferences extracts reference ids from a Reference slice
// for order-independent set comparisons.
func referenceIDsFromReferences(refs []Reference) []uuid.UUID {
	ids := make([]uuid.UUID, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids
}

// --- LookupReferencesByName (loam-4na): name -> []Reference, backing
// `graph refs` -- mirrors the LookupSymbolsByName integration suite below,
// scoping-by-scoping, per this bead's DESIGN CONSTRAINT. ---

// TestLookupReferencesByName_MultipleUseSites proves several references to
// the same name all come back as one call's result -- refs is naturally
// many-rows-per-name (every call site), unlike def's ambiguity case.
func TestLookupReferencesByName_MultipleUseSites(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	a := insertReference(ctx, t, pool, repoID, "a.go", "Login", 5)
	b := insertReference(ctx, t, pool, repoID, "b.go", "Login", 12)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, refs, 2, "every reference to the name must return")
	assert.ElementsMatch(t, []uuid.UUID{a, b}, referenceIDsFromReferences(refs))
}

// TestLookupReferencesByName_NoMatchIsEmptyNotError proves a name with zero
// referencing rows comes back empty, not an error -- but see this bead's
// report: unlike LookupSymbolsByName, this is not by itself an
// authoritative not-found signal for the *symbol* (see
// TestLookupReferencesByName_DistinguishesFromUnreferencedSymbol below).
func TestLookupReferencesByName_NoMatchIsEmptyNotError(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertReference(ctx, t, pool, repoID, "a.go", "Login", 5) // unrelated reference exists in scope

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "NoSuchName", "", 0)
	require.NoError(t, err, "a genuine not-found must not be an error")
	assert.False(t, truncated)
	assert.Empty(t, refs, "zero matching references must come back as an empty slice, never a phantom match from an unrelated name")
}

// TestLookupReferencesByName_EmptyNameMatchesNothing pins that the SQL's
// empty-string-means-no-filter sentinel applies ONLY to $4 (--file), not
// to $3 (name): symbol_references.name is NOT NULL text exactly like
// symbol_references.file is, so widening $4's own OR-empty-string clause
// to also cover $3 would compile and behave identically -- a
// plausible-looking "make these consistent" edit that would silently turn
// an empty name into "match every reference in scope" instead of matching
// nothing. The review round for this bead flagged that no test pinned this
// asymmetry.
func TestLookupReferencesByName_EmptyNameMatchesNothing(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertReference(ctx, t, pool, repoID, "a.go", "Login", 5)
	insertReference(ctx, t, pool, repoID, "b.go", "Logout", 9)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Empty(t, refs, "an empty name must match nothing, never every reference in scope -- name has no wildcard sentinel, unlike --file")
}

// TestLookupReferencesByName_DistinguishesFromUnreferencedSymbol is this
// bead's central design point, exercised end-to-end: "no such symbol" and
// "symbol exists but has never been referenced" both make
// LookupReferencesByName return empty, so a caller needing the distinction
// (docs/cli-spec.md exit 3 for `graph refs`) must compose this with
// LookupSymbolsByName -- the same composition Dependents/Deps/History
// already require. This proves that composition actually works: a name
// with no symbol at all resolves to zero from LookupSymbolsByName (exit 3);
// a name that IS a real, defined symbol but has zero call sites resolves to
// one row from LookupSymbolsByName and zero, non-error rows from
// LookupReferencesByName (exit 0, empty results) -- genuinely
// distinguishable by a caller holding both results, even though
// LookupReferencesByName alone cannot tell the two apart.
func TestLookupReferencesByName_DistinguishesFromUnreferencedSymbol(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	unreferencedID := insertSymbol(ctx, t, pool, repoID, "unused.go", "Orphan")

	notFoundSymbols, _, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "DoesNotExist", "", 0)
	require.NoError(t, err)
	assert.Empty(t, notFoundSymbols, "no symbol named DoesNotExist exists -- this is the not-found case")
	notFoundRefs, _, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "DoesNotExist", "", 0)
	require.NoError(t, err)
	assert.Empty(t, notFoundRefs)

	foundSymbols, _, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Orphan", "", 0)
	require.NoError(t, err)
	require.Len(t, foundSymbols, 1, "Orphan exists as a symbol -- this is NOT the not-found case")
	assert.Equal(t, unreferencedID, foundSymbols[0].ID)

	foundRefs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "Orphan", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Empty(t, foundRefs, "Orphan is never referenced -- an empty reference set, not a not-found condition")
}

// TestLookupReferencesByName_NarrowedByFile proves --file narrows to
// references in exactly one file (docs/cli-spec.md: "--file <path> narrows
// the target to the definition in one file" -- the same narrowing applies
// to refs' use sites).
func TestLookupReferencesByName_NarrowedByFile(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertReference(ctx, t, pool, repoID, "web/login.go", "Login", 5)
	cliRef := insertReference(ctx, t, pool, repoID, "cli/login.go", "Login", 9)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "Login", "cli/login.go", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, refs, 1, "--file must narrow to references in exactly one file")
	assert.Equal(t, cliRef, refs[0].ID)
}

// TestLookupReferencesByName_NarrowedByFile_NoMatchIsNotFound proves --file
// narrowing an existing name down to a file with no matching reference is
// itself a not-found result (empty, not an error).
func TestLookupReferencesByName_NarrowedByFile_NoMatchIsNotFound(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertReference(ctx, t, pool, repoID, "web/login.go", "Login", 5)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "Login", "cli/login.go", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Empty(t, refs, "Login is referenced in web/login.go, not cli/login.go -- narrowing to the wrong file is not-found")
}

// TestLookupReferencesByName_ExcludesOutOfScopeRepo mirrors
// TestLookupSymbolsByName_ExcludesOutOfScopeRepo: it seeds a reference with
// the SAME name in a repo NOT in the lookup's scope -- one that would sort
// first (file "aaa.go") if the repo-id filter were dropped or broadened --
// and proves it never appears.
func TestLookupReferencesByName_ExcludesOutOfScopeRepo(t *testing.T) {
	t.Parallel()
	store, pool, inScopeRepo := newTestStore(t)
	ctx := t.Context()
	outOfScopeRepo := insertRepo(ctx, t, pool, "group/refs-out-of-scope")

	inScopeID := insertReference(ctx, t, pool, inScopeRepo, "zzz.go", "Login", 1)
	insertReference(ctx, t, pool, outOfScopeRepo, "aaa.go", "Login", 1)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{inScopeRepo}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, refs, 1, "the out-of-scope repo's same-named reference must never appear")
	assert.Equal(t, inScopeID, refs[0].ID)
}

// TestLookupReferencesByName_ExcludesOtherTargetBranch proves target_branch
// scoping specifically: a same-named reference on a different branch of the
// SAME repo must not appear, seeded in a file that would sort first if
// branch filtering were dropped.
func TestLookupReferencesByName_ExcludesOtherTargetBranch(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	mainID := insertReferenceOnBranch(ctx, t, pool, repoID, "main", "zzz.go", "Login", 1)
	insertReferenceOnBranch(ctx, t, pool, repoID, "feature", "aaa.go", "Login", 1)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, refs, 1, "a same-named reference on a different target_branch must never appear")
	assert.Equal(t, mainID, refs[0].ID)
}

// TestLookupReferencesByName_IncludesMultipleInScopeRepos proves the plural
// repoIDs scope is a genuine multi-repo OR: two different in-scope repos
// each referencing "Login" must both be returned from one call.
func TestLookupReferencesByName_IncludesMultipleInScopeRepos(t *testing.T) {
	t.Parallel()
	store, pool, repoA := newTestStore(t)
	ctx := t.Context()
	repoB := insertRepo(ctx, t, pool, "group/refs-second-in-scope")

	idA := insertReference(ctx, t, pool, repoA, "a.go", "Login", 1)
	idB := insertReference(ctx, t, pool, repoB, "b.go", "Login", 1)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoA, repoB}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, refs, 2, "both in-scope repos' matches must be returned from a single call")
	assert.ElementsMatch(t, []uuid.UUID{idA, idB}, referenceIDsFromReferences(refs))
}

// TestLookupReferencesByName_TruncatesAndReportsTruncated proves the
// limit/truncated contract against a real database: docs/cli-spec.md:535-537
// requires truncated: true on a capped `graph` response for every
// subquery, refs included -- this seeds 4 use sites of the same name and
// asks for at most 2. It also pins WHICH 2 of the 4 survive (f0.go, f1.go,
// the ORDER BY sr.file, sr.line, sr.id head), not merely that 2 survive:
// the review round for this bead found that swapping the query's
// `ORDER BY sr.file, sr.line, sr.id` for `ORDER BY sr.id DESC` still passed
// the whole suite when only Len==2 was asserted here -- LIMIT's contract
// depends entirely on which order it caps, exactly the failure mode
// TestDependents_NearestDepthFirst_NotUUIDOrder (FIX 2, above) already
// guards for Dependents.
func TestLookupReferencesByName_TruncatesAndReportsTruncated(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	for i := range 4 {
		insertReference(ctx, t, pool, repoID, fmt.Sprintf("f%d.go", i), "Login", int32(i+1))
	}

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 2)
	require.NoError(t, err)
	assert.True(t, truncated, "4 matches exist for a limit of 2, so truncated must be true")
	require.Len(t, refs, 2)
	assert.Equal(t, []string{"f0.go", "f1.go"}, []string{refs[0].File, refs[1].File}, "a capped result must keep the ORDER BY-first rows (f0.go, f1.go), not an arbitrary 2 of the 4 matches")
}

// TestLookupReferencesByName_ExactlyLimitMatches_NotTruncated is the
// negative case against a real database: exactly limit matches must not
// report truncated=true.
func TestLookupReferencesByName_ExactlyLimitMatches_NotTruncated(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertReference(ctx, t, pool, repoID, "a.go", "Login", 1)
	insertReference(ctx, t, pool, repoID, "b.go", "Login", 2)

	refs, truncated, err := store.LookupReferencesByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 2)
	require.NoError(t, err)
	assert.False(t, truncated, "exactly 2 matches for a limit of 2 must not be reported as truncated")
	assert.Len(t, refs, 2)
}

// TestLookupSymbolsByName_ExactlyOneMatch is the unambiguous case: one
// symbol named "Login" resolves to exactly one row.
func TestLookupSymbolsByName_ExactlyOneMatch(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	loginID := insertSymbol(ctx, t, pool, repoID, "auth.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 1, "an unambiguous name must resolve to exactly one row")
	assert.Equal(t, loginID, symbols[0].ID)
	assert.Equal(t, "auth.go", symbols[0].File)
}

// TestLookupSymbolsByName_AmbiguousReturnsAllMatches proves
// docs/cli-spec.md:528-533's "ambiguous target is data, not an error":
// three distinct "Login" symbols in three different files must all come
// back, not an error and not just one of them.
func TestLookupSymbolsByName_AmbiguousReturnsAllMatches(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	a := insertSymbol(ctx, t, pool, repoID, "web/login.go", "Login")
	b := insertSymbol(ctx, t, pool, repoID, "cli/login.go", "Login")
	c := insertSymbol(ctx, t, pool, repoID, "api/login.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 3, "an ambiguous name must return every matching symbol, not fail or pick one")
	assert.ElementsMatch(t, []uuid.UUID{a, b, c}, symbolIDsFromSymbols(symbols))
}

// TestLookupSymbolsByName_NoMatchIsEmptyNotError is this bead's central
// contract, proved against a real database: a name with zero matching
// symbols must come back as an empty, non-error result -- the
// authoritative not-found signal docs/cli-spec.md maps to exit 3, never a
// sentinel error a handler would otherwise have to special-case.
func TestLookupSymbolsByName_NoMatchIsEmptyNotError(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertSymbol(ctx, t, pool, repoID, "auth.go", "Login") // some unrelated symbol exists in scope

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "NoSuchSymbol", "", 0)
	require.NoError(t, err, "a genuine not-found must not be an error")
	assert.False(t, truncated)
	assert.Empty(t, symbols, "zero matches must come back as an empty slice, the authoritative not-found signal")
}

// TestLookupSymbolsByName_DistinguishesNotFoundFromZeroEdges is the
// acceptance criterion this bead exists to satisfy, exercised end-to-end
// against the real store: "a handler can distinguish 'no such symbol' from
// 'symbol with zero edges'" (loam-awr ACCEPTANCE CRITERIA). Before this
// bead, Dependents/Deps took a uuid.UUID a handler had no way to obtain
// from a name, and both cases collapsed to (nil, nil) at that layer. Now:
// looking up a name that resolves to zero symbols is the not-found case
// (exit 3); looking up a name that resolves to exactly one symbol, then
// calling Dependents/Deps on its id and getting an empty, untruncated set,
// is the "exists but isolated" case (exit 0, empty results) -- genuinely
// distinguishable by a caller.
func TestLookupSymbolsByName_DistinguishesNotFoundFromZeroEdges(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	isolatedID := insertSymbol(ctx, t, pool, repoID, "isolated.go", "Orphan")

	notFound, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "DoesNotExist", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Empty(t, notFound, "no symbol named DoesNotExist exists -- this is the not-found case")

	found, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Orphan", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, found, 1, "Orphan exists -- this is NOT the not-found case, even though it has no edges")
	assert.Equal(t, isolatedID, found[0].ID)

	deps, truncated, err := store.Dependents(ctx, repoID, "main", isolatedID, 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Empty(t, deps, "Orphan has no dependents -- an empty edge set, not a not-found condition")
}

// TestLookupSymbolsByName_NarrowedByFile proves --file narrows an
// ambiguous name down to the definition in exactly one file
// (docs/cli-spec.md: "--file <path> narrows the target to the definition
// in one file").
func TestLookupSymbolsByName_NarrowedByFile(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertSymbol(ctx, t, pool, repoID, "web/login.go", "Login")
	cliLogin := insertSymbol(ctx, t, pool, repoID, "cli/login.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Login", "cli/login.go", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 1, "--file must narrow an otherwise-ambiguous name to one match")
	assert.Equal(t, cliLogin, symbols[0].ID)
}

// TestLookupSymbolsByName_NarrowedByFile_NoMatchIsNotFound proves --file
// narrowing an existing name down to a file with no matching definition is
// itself a not-found result (empty, not an error) -- e.g. a symbol that
// really is defined elsewhere, not in the file the caller asked about.
func TestLookupSymbolsByName_NarrowedByFile_NoMatchIsNotFound(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertSymbol(ctx, t, pool, repoID, "web/login.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Login", "cli/login.go", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Empty(t, symbols, "Login is defined in web/login.go, not cli/login.go -- narrowing to the wrong file is not-found")
}

// TestLookupSymbolsByName_ExcludesOutOfScopeRepo is the discriminating
// scoping test the brief asks for, mirroring
// internal/chunkstore's TestSearch_ScopedByRepoIDs_ExcludesOutOfScopeRepos:
// it seeds a symbol with the SAME name in a repo NOT in the lookup's scope
// -- one that would sort first (file "aaa.go", alphabetically ahead of the
// in-scope symbol's file) if the repo-id filter were dropped or broadened
// -- and proves it never appears.
func TestLookupSymbolsByName_ExcludesOutOfScopeRepo(t *testing.T) {
	t.Parallel()
	store, pool, inScopeRepo := newTestStore(t)
	ctx := t.Context()
	outOfScopeRepo := insertRepo(ctx, t, pool, "group/lookup-out-of-scope")

	inScopeID := insertSymbol(ctx, t, pool, inScopeRepo, "zzz.go", "Login")
	insertSymbol(ctx, t, pool, outOfScopeRepo, "aaa.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{inScopeRepo}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 1, "the out-of-scope repo's same-named symbol must never appear")
	assert.Equal(t, inScopeID, symbols[0].ID)
}

// TestLookupSymbolsByName_ExcludesOtherTargetBranch proves target_branch
// scoping specifically: a same-named symbol on a different branch of the
// SAME repo must not appear, seeded in a file that would sort first if
// branch filtering were dropped.
func TestLookupSymbolsByName_ExcludesOtherTargetBranch(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	mainID := insertSymbolOnBranch(ctx, t, pool, repoID, "main", "zzz.go", "Login")
	insertSymbolOnBranch(ctx, t, pool, repoID, "feature", "aaa.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 1, "a same-named symbol on a different target_branch must never appear")
	assert.Equal(t, mainID, symbols[0].ID)
}

// TestLookupSymbolsByName_IncludesMultipleInScopeRepos proves the plural
// repoIDs scope (matching internal/chunkstore.Search's convention) is a
// genuine multi-repo OR, not silently narrowed to the first id: two
// different in-scope repos each declaring "Login" must both be returned
// from one call.
func TestLookupSymbolsByName_IncludesMultipleInScopeRepos(t *testing.T) {
	t.Parallel()
	store, pool, repoA := newTestStore(t)
	ctx := t.Context()
	repoB := insertRepo(ctx, t, pool, "group/lookup-second-in-scope")

	idA := insertSymbol(ctx, t, pool, repoA, "a.go", "Login")
	idB := insertSymbol(ctx, t, pool, repoB, "b.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoA, repoB}, "main", "Login", "", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, symbols, 2, "both in-scope repos' matches must be returned from a single call")
	assert.ElementsMatch(t, []uuid.UUID{idA, idB}, symbolIDsFromSymbols(symbols))
}

// TestLookupSymbolsByName_TruncatesAndReportsTruncated proves the
// limit/truncated contract against a real database, not just a mock:
// docs/cli-spec.md:535-537 requires truncated: true on a capped `graph`
// response for every subquery, not only the blast-radius ones -- this
// seeds 4 same-named symbols and asks for at most 2.
func TestLookupSymbolsByName_TruncatesAndReportsTruncated(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	for i := range 4 {
		insertSymbol(ctx, t, pool, repoID, fmt.Sprintf("f%d.go", i), "Login")
	}

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 2)
	require.NoError(t, err)
	assert.True(t, truncated, "4 matches exist for a limit of 2, so truncated must be true")
	assert.Len(t, symbols, 2)
}

// TestLookupSymbolsByName_ExactlyLimitMatches_NotTruncated is the negative
// case against a real database: exactly limit matches must not report
// truncated=true.
func TestLookupSymbolsByName_ExactlyLimitMatches_NotTruncated(t *testing.T) {
	t.Parallel()
	store, pool, repoID := newTestStore(t)
	ctx := t.Context()
	insertSymbol(ctx, t, pool, repoID, "a.go", "Login")
	insertSymbol(ctx, t, pool, repoID, "b.go", "Login")

	symbols, truncated, err := store.LookupSymbolsByName(ctx, []uuid.UUID{repoID}, "main", "Login", "", 2)
	require.NoError(t, err)
	assert.False(t, truncated, "exactly 2 matches for a limit of 2 must not be reported as truncated")
	assert.Len(t, symbols, 2)
}

// symbolIDsFromSymbols extracts symbol ids from a Symbol slice for
// order-independent set comparisons.
func symbolIDsFromSymbols(symbols []Symbol) []uuid.UUID {
	ids := make([]uuid.UUID, len(symbols))
	for i, s := range symbols {
		ids[i] = s.ID
	}
	return ids
}
