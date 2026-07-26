//go:build integration

// See integration_test.go's header for the podman/ryuk workaround note; it
// applies equally here. Run explicitly with:
//
//	go test -tags=integration ./internal/db/migrations/... -run TestCodeIntel -v
package migrations

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// codeIntelTables is the exact derived-table set 0002_code_intel.up.sql must
// create, per docs/persistence-spec.md "Code intelligence" and this bead's
// ACCEPTANCE CRITERIA.
var codeIntelTables = []string{
	"symbols",
	"symbol_references",
	"graph_edges",
	"symbol_history",
	"chunks",
}

// pgvectorImage is a Postgres 16 image with the pgvector extension already
// built in. Plain postgres:16-alpine (loam-54o.3's image) has no `vector`
// extension available at all -- `CREATE EXTENSION vector` fails outright
// against it -- so this migration's integration test needs a different
// image than the metadata migration's.
const pgvectorImage = "pgvector/pgvector:pg16"

// TestCodeIntelMigrationAgainstRealPostgres runs the full migration set
// (0001_init then 0002_code_intel) against a real pgvector-enabled Postgres,
// proving: the vector extension and all five derived tables exist with
// their FKs; migrate up twice is a no-op (ErrNoChange); the HNSW index
// exists and an actual nearest-neighbour query over seeded vectors returns
// rows in distance order; the dependents recursive CTE terminates over a
// mutual-recursion cycle; and migrate down cleanly reverses both the tables
// and the extension.
func TestCodeIntelMigrationAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
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

	// First apply: exercises m.Up() actually running both migrations.
	require.NoError(t, Migrate(ctx, dsn, logger))
	assertVectorExtensionExists(ctx, t, dsn)
	assertCodeIntelTablesExist(ctx, t, dsn)
	assertGraphEdgesKindCheck(ctx, t, dsn)
	assertHNSWIndexExists(ctx, t, dsn)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	repoID := seedRepo(ctx, t, pool)
	assertHNSWNearestNeighbourOrdering(ctx, t, pool, repoID)
	assertDependentsCTETerminatesOnCycle(ctx, t, pool, repoID)

	// Second apply against an already-migrated database: exercises the
	// ErrNoChange idempotency branch for real, for both migrations.
	require.NoError(t, Migrate(ctx, dsn, logger))

	migrateDown(ctx, t, dsn)
	assertCodeIntelTablesAbsent(ctx, t, dsn)
	assertVectorExtensionAbsent(ctx, t, dsn)
}

// assertVectorExtensionExists checks pg_extension directly, independent of
// migrate's own bookkeeping.
func assertVectorExtensionExists(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`,
	).Scan(&exists))
	assert.True(t, exists, "expected the vector extension to exist after migrate up")
}

// assertVectorExtensionAbsent is the down-migration mirror.
func assertVectorExtensionAbsent(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`,
	).Scan(&exists))
	assert.False(t, exists, "expected the vector extension to be dropped after migrate down")
}

// assertCodeIntelTablesExist queries information_schema directly so the
// assertion proves the schema Migrate actually left behind.
func assertCodeIntelTablesExist(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	for _, table := range codeIntelTables {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.Truef(t, exists, "expected table %q to exist after migrate up", table)
	}
	// embedding must be vector(768) -- the documented constant, matching
	// both production nomic-embed-text and internal/testembed.Dimension.
	var udtName string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT udt_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'chunks' AND column_name = 'embedding'`,
	).Scan(&udtName))
	assert.Equal(t, "vector", udtName)
	var dim int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT atttypmod FROM pg_attribute WHERE attrelid = 'chunks'::regclass AND attname = 'embedding'`,
	).Scan(&dim))
	assert.Equal(t, 768, dim, "chunks.embedding must be vector(768)")
}

// assertCodeIntelTablesAbsent is the down-migration mirror.
func assertCodeIntelTablesAbsent(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	for _, table := range codeIntelTables {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.Falsef(t, exists, "expected table %q to be dropped after migrate down", table)
	}
}

// assertGraphEdgesKindCheck proves graph_edges.kind's CHECK constraint is
// real and enforced by Postgres, not just documented: a 'dependency' row
// inserts cleanly, an arbitrary other value is rejected.
func assertGraphEdgesKindCheck(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES (gen_random_uuid(), 'group/check-repo', 'https://example.com/repo.git', 'example.com', 'main')`,
	)
	require.NoError(t, err)
	var repoID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM repos WHERE name = 'group/check-repo'`).Scan(&repoID))
	var fromID, toID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO symbols (id, repo_id, target_branch, file, name, kind) VALUES (gen_random_uuid(), $1, 'main', 'a.go', 'A', 'function') RETURNING id`,
		repoID,
	).Scan(&fromID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO symbols (id, repo_id, target_branch, file, name, kind) VALUES (gen_random_uuid(), $1, 'main', 'b.go', 'B', 'function') RETURNING id`,
		repoID,
	).Scan(&toID))
	_, err = pool.Exec(ctx,
		`INSERT INTO graph_edges (id, repo_id, target_branch, from_symbol_id, to_symbol_id, kind) VALUES (gen_random_uuid(), $1, 'main', $2, $3, 'dependency')`,
		repoID, fromID, toID,
	)
	require.NoError(t, err, "kind='dependency' must be accepted")
	_, err = pool.Exec(ctx,
		`INSERT INTO graph_edges (id, repo_id, target_branch, from_symbol_id, to_symbol_id, kind) VALUES (gen_random_uuid(), $1, 'main', $2, $3, 'bogus')`,
		repoID, fromID, toID,
	)
	require.Error(t, err, "an arbitrary kind must violate graph_edges_kind_check")
	assert.Contains(t, err.Error(), "graph_edges_kind_check")
}

// assertHNSWIndexExists checks pg_indexes for the exact chunks_embedding
// HNSW index docs/persistence-spec.md:157 mandates.
func assertHNSWIndexExists(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	var indexdef string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'chunks_embedding'`,
	).Scan(&indexdef))
	assert.Contains(t, indexdef, "USING hnsw")
	assert.Contains(t, indexdef, "vector_cosine_ops")
}

// seedRepo inserts a repos row and returns its id, the FK every derived
// table in this migration needs.
func seedRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var repoID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES (gen_random_uuid(), 'group/vector-repo', 'https://example.com/repo.git', 'example.com', 'main')
		 RETURNING id`,
	).Scan(&repoID))
	return repoID
}

// unitVector returns a 768-dim vector.Vector that is all zero except index i
// set to 1, so distinct i values are maximally distinguishable under cosine
// distance and a nearest-neighbour query has an unambiguous expected order.
func unitVector(i int) pgvector.Vector {
	v := make([]float32, 768)
	v[i] = 1
	return pgvector.NewVector(v)
}

// assertHNSWNearestNeighbourOrdering seeds three chunks with orthogonal unit
// vectors and a query vector close to one of them, then proves the
// `ORDER BY embedding <=> :q` operator docs/persistence-spec.md:153 names
// actually returns rows in ascending cosine-distance order via the real
// HNSW index -- not just that the index exists, but that it (or the planner
// falling back to it correctly) produces correct results.
func assertHNSWNearestNeighbourOrdering(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID string) {
	t.Helper()
	near := unitVector(0)
	mid := pgvector.NewVector(func() []float32 {
		v := make([]float32, 768)
		v[0] = 1
		v[1] = 1
		return v
	}())
	far := unitVector(767)
	seedChunk(ctx, t, pool, repoID, "near.go", near)
	seedChunk(ctx, t, pool, repoID, "mid.go", mid)
	seedChunk(ctx, t, pool, repoID, "far.go", far)

	query := unitVector(0)
	rows, err := pool.Query(ctx,
		`SELECT file FROM chunks WHERE repo_id = $1 ORDER BY embedding <=> $2 LIMIT 3`,
		repoID, query,
	)
	require.NoError(t, err)
	defer rows.Close()
	var order []string
	for rows.Next() {
		var file string
		require.NoError(t, rows.Scan(&file))
		order = append(order, file)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"near.go", "mid.go", "far.go"}, order, "nearest-neighbour query must return rows in ascending cosine-distance order")
}

// seedChunk inserts one chunks row for repoID with the given file and
// embedding.
func seedChunk(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID, file string, embedding pgvector.Vector) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO chunks (id, repo_id, target_branch, file, start_line, end_line, content, embedding)
		 VALUES (gen_random_uuid(), $1, 'main', $2, 1, 1, 'content', $3)`,
		repoID, file, embedding,
	)
	require.NoError(t, err)
}

// dependentsCTE walks graph_edges from a starting symbol, following
// from_symbol_id -> to_symbol_id edges, guarding against revisiting a
// symbol already in the path so a cycle terminates instead of looping
// forever. This is the shape the code graph store (loam-54o.14) will use
// for its `dependents`/`deps` queries (docs/persistence-spec.md:143).
const dependentsCTE = `
WITH RECURSIVE dependents AS (
    SELECT from_symbol_id, to_symbol_id, ARRAY[from_symbol_id] AS path
    FROM graph_edges
    WHERE from_symbol_id = $1
    UNION ALL
    SELECT e.from_symbol_id, e.to_symbol_id, d.path || e.from_symbol_id
    FROM graph_edges e
    JOIN dependents d ON e.from_symbol_id = d.to_symbol_id
    WHERE NOT e.from_symbol_id = ANY(d.path)
)
SELECT DISTINCT to_symbol_id FROM dependents
`

// assertDependentsCTETerminatesOnCycle seeds a two-symbol mutual-recursion
// cycle -- the same shape as internal/testfixture's is_even/is_odd Python
// fixture (docs/testing-spec.md "Fixtures") -- and proves dependentsCTE
// terminates and returns the correct reachable set, rather than hanging or
// erroring on infinite recursion.
func assertDependentsCTETerminatesOnCycle(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID string) {
	t.Helper()
	var isEvenID, isOddID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO symbols (id, repo_id, target_branch, file, name, kind) VALUES (gen_random_uuid(), $1, 'main', 'parity.py', 'is_even', 'function') RETURNING id`,
		repoID,
	).Scan(&isEvenID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO symbols (id, repo_id, target_branch, file, name, kind) VALUES (gen_random_uuid(), $1, 'main', 'parity.py', 'is_odd', 'function') RETURNING id`,
		repoID,
	).Scan(&isOddID))
	_, err := pool.Exec(ctx,
		`INSERT INTO graph_edges (id, repo_id, target_branch, from_symbol_id, to_symbol_id, kind) VALUES (gen_random_uuid(), $1, 'main', $2, $3, 'dependency')`,
		repoID, isEvenID, isOddID,
	)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO graph_edges (id, repo_id, target_branch, from_symbol_id, to_symbol_id, kind) VALUES (gen_random_uuid(), $1, 'main', $2, $3, 'dependency')`,
		repoID, isOddID, isEvenID,
	)
	require.NoError(t, err)

	done := make(chan struct{})
	var deps []string
	var queryErr error
	go func() {
		defer close(done)
		rows, err := pool.Query(ctx, dependentsCTE, isEvenID)
		if err != nil {
			queryErr = err
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				queryErr = err
				return
			}
			deps = append(deps, id)
		}
		queryErr = rows.Err()
	}()
	select {
	case <-done:
		require.NoError(t, queryErr)
		assert.ElementsMatch(t, []string{isEvenID, isOddID}, deps, "is_even's dependents must be exactly {is_even, is_odd} despite the mutual-recursion cycle")
	case <-time.After(10 * time.Second):
		t.Fatal("dependents recursive CTE did not terminate within 10s -- likely looping on the is_even/is_odd cycle")
	}
}
