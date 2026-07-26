//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./internal/store/chunks/... -v
//
// On podman (e.g. a `podman machine` forwarding /var/run/docker.sock), also
// set TESTCONTAINERS_RYUK_DISABLED=true (see internal/db/migrations's
// integration_test.go for why -- podman's Docker-compat API does not resolve
// the reaper sidecar's expected `bridge` network, so the container never
// starts without it). This is a local convenience only, not a CI setting.
package chunks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testembed"
)

// pgvectorImage matches internal/db/migrations's own integration tests:
// plain postgres:16-alpine has no `vector` extension at all.
const pgvectorImage = "pgvector/pgvector:pg16"

// sharedDSN is the one migrated Postgres every test in this package runs
// against, started once in TestMain rather than one container per test.
// Isolation between tests comes from each seeding its own repo row (and, by
// FK, its own chunks) instead of from separate databases -- the same fix a
// sibling store bead already used under this same shared build machine's
// container contention: fewer concurrent testcontainers, not a shortcut on
// coverage, since every test still runs against the real schema and a real
// server.
var sharedDSN string

// TestMain starts one pgvector-enabled Postgres container, applies the
// production migration set, and hands every test in this package the same
// DSN, tearing the container down once after the whole package's tests
// finish.
func TestMain(m *testing.M) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, pgvectorImage,
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

// newRegisteredPool builds a pool through internal/db.NewPool, the same
// production path cmd/server uses -- pgvector types registered via
// AfterConnect (internal/db/pool.go).
func newRegisteredPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: dsn}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// newUnregisteredPool builds a plain pgxpool.Pool with no AfterConnect hook
// at all -- deliberately bypassing internal/db.NewPool -- so tests can prove
// what breaks without pgvector type registration.
func newUnregisteredPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// insertRepo inserts a minimal repos row and returns its id, the FK
// chunks.repo_id requires.
func insertRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES (gen_random_uuid(), $1, 'https://example.com/repo.git', 'example.com', 'main')
		 RETURNING id`,
		name,
	).Scan(&id))
	return id
}

// unit returns a testembed.Dimension-wide vector that is all zero except
// index i set to 1. Sized off testembed.Dimension, never a bare 768
// literal, so this test tracks whatever width internal/testembed and
// production nomic-embed-text actually use (docs/persistence-spec.md
// "chunks" -- vector(768) is a documented constant, not a magic number).
func unit(i int) []float32 {
	v := make([]float32, testembed.Dimension)
	v[i] = 1
	return v
}

// mix returns unit(0) + unit(1): a vector at a known, non-orthogonal angle
// to unit(0) so ordering against it is strictly between "identical" and
// "orthogonal", with no ties (see TestSearch_HNSWNearestNeighbourOrdering's
// doc comment for the exact distances this produces).
func mix() []float32 {
	v := make([]float32, testembed.Dimension)
	v[0] = 1
	v[1] = 1
	return v
}

// --- Registration proof: the vacuous-test hazard this bead's brief warns
// about is a test that "proves" pgvector wiring by round-tripping a scalar
// vector column, which passes with zero type registration (pgvector.Vector
// implements both sql.Scanner and driver.Valuer as a text fallback). These
// two tests instead inspect the pgx type map directly and scan an ARRAY,
// which cannot be satisfied by the Scanner/Valuer fallback -- and prove the
// negative case too: the same array-scan genuinely fails without
// registration, not just "would in theory".

// TestPgvectorRegistration_ActiveOnStorePool proves the pool internal/db.NewPool
// builds -- the one Store uses in production -- has pgvector's vector codec
// genuinely registered, via both discriminating checks named in this bead's
// brief: the type map reports the vector OID bound to *pgxvec.VectorCodec,
// and scanning `SELECT ARRAY[...]` into a *[]pgvector.Vector succeeds (the
// sql.Scanner/driver.Valuer fallback only ever satisfies a single scalar
// value, never an array).
func TestPgvectorRegistration_ActiveOnStorePool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	typ, ok := conn.Conn().TypeMap().TypeForName("vector")
	require.True(t, ok, "the vector type must be registered in the connection's type map")
	assert.IsType(t, &pgxvec.VectorCodec{}, typ.Codec, "the registered vector type must use pgxvec's VectorCodec")

	var arr []pgvector.Vector
	err = pool.QueryRow(ctx, `SELECT ARRAY[$1::vector]`, pgvector.NewVector(unit(0))).Scan(&arr)
	require.NoError(t, err, "scanning a vector ARRAY must succeed once the codec is registered")
	require.Len(t, arr, 1)
	assert.Equal(t, unit(0), arr[0].Slice())
}

// TestPgvectorRegistration_WithoutAfterConnect_ArrayScanFails is the mutation
// this bead's brief explicitly requires: a pool built WITHOUT the
// AfterConnect registration internal/db.NewPool wires in. If the array-scan
// check above still passed against this pool, it would mean the check
// itself is vacuous; it must fail here, and with a scan-type error (an
// assertion-shaped failure), not a hang or panic.
func TestPgvectorRegistration_WithoutAfterConnect_ArrayScanFails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newUnregisteredPool(t, sharedDSN)

	var arr []pgvector.Vector
	err := pool.QueryRow(ctx, `SELECT ARRAY[$1::vector]`, pgvector.NewVector(unit(0))).Scan(&arr)
	require.Error(t, err, "without AfterConnect registration, scanning a vector ARRAY must fail -- proving the happy-path check above is discriminating, not vacuous")
	assert.Contains(t, err.Error(), "cannot scan unknown type")
}

// --- Store-level behavior ---

// TestReplaceFileChunks_PerFileDeleteAndReplace proves the store's core
// write contract against a real database: replacing one file's chunks
// leaves every other file's chunks untouched, and the old rows for the
// replaced file are genuinely gone (not just superseded).
func TestReplaceFileChunks_PerFileDeleteAndReplace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(ctx, t, pool, "group/replace-repo")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(pool, logger)

	_, err := s.ReplaceFileChunks(ctx, repoID, "main", "a.go", []ChunkInput{
		{StartLine: 1, EndLine: 5, Content: "func A() {}", Embedding: unit(0)},
		{StartLine: 6, EndLine: 10, Content: "func A2() {}", Embedding: unit(1)},
	})
	require.NoError(t, err)
	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "b.go", []ChunkInput{
		{StartLine: 1, EndLine: 3, Content: "func B() {}", Embedding: unit(2)},
	})
	require.NoError(t, err)

	replaced, err := s.ReplaceFileChunks(ctx, repoID, "main", "a.go", []ChunkInput{
		{StartLine: 1, EndLine: 8, Content: "func AMerged() {}", Embedding: unit(3)},
	})
	require.NoError(t, err)
	require.Len(t, replaced, 1)
	assert.Equal(t, "func AMerged() {}", replaced[0].Content)
	assert.Equal(t, unit(3), replaced[0].Embedding, "the embedding must round-trip through pgvector-go exactly")

	var aCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE repo_id = $1 AND target_branch = 'main' AND file = 'a.go'`, repoID,
	).Scan(&aCount))
	assert.Equal(t, 1, aCount, "the two original a.go chunks must be gone, replaced by exactly one")

	var bCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE repo_id = $1 AND target_branch = 'main' AND file = 'b.go'`, repoID,
	).Scan(&bCount))
	assert.Equal(t, 1, bCount, "replacing a.go must not touch b.go's chunks")
}

// TestReplaceFileChunks_EmptyInputs_ClearsFile proves passing no inputs
// deletes a file's chunks without inserting any -- the shape an incremental
// re-embed uses when a file was removed from the tree.
func TestReplaceFileChunks_EmptyInputs_ClearsFile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(ctx, t, pool, "group/clear-repo")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(pool, logger)

	_, err := s.ReplaceFileChunks(ctx, repoID, "main", "removed.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "gone soon", Embedding: unit(0)},
	})
	require.NoError(t, err)
	result, err := s.ReplaceFileChunks(ctx, repoID, "main", "removed.go", nil)
	require.NoError(t, err)
	assert.Empty(t, result)

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM chunks WHERE repo_id = $1 AND file = 'removed.go'`, repoID,
	).Scan(&count))
	assert.Equal(t, 0, count)
}

// TestSearch_HNSWNearestNeighbourOrdering proves Store.Search's ORDER BY
// embedding <=> :q actually returns rows nearest-first via the real HNSW
// index, using three chunks at deterministic, non-tied cosine distances
// from the query vector q = unit(0):
//
//   - "near.go" = unit(0): identical to q, cosine distance 0.
//   - "mid.go"  = unit(0)+unit(1): dot product 1, norm sqrt(2), so cosine
//     similarity 1/sqrt(2) ~= 0.7071 and distance ~= 0.2929.
//   - "far.go"  = unit(1): orthogonal to q, cosine similarity 0, distance 1.
//
// 0 < 0.2929 < 1 with no ties, so "nearest-first" has exactly one correct
// order -- the file names read as a result (near/mid/far), not number soup.
//
// Calibration note: pgvector's HNSW is an *approximate* nearest-neighbour
// index in general, so "returns the true nearest-first order" is not a
// blanket guarantee of the index type. What this test actually relies on is
// narrower and stable: with only three vectors in the whole table, per-repo
// filtering during the index scan, and pgvector's default HNSW search
// parameters (ef_search=40 >> 3 candidates), the graph exploration covers
// every candidate, so the result is exact for a dataset this small -- not
// merely likely. This is the same seeding shape (and the same reasoning)
// internal/db/migrations/code_intel_integration_test.go already established
// for the raw-SQL path; this test proves the STORE method produces the same
// order, scoped across repos.
func TestSearch_HNSWNearestNeighbourOrdering(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(pool, logger)
	repoID := insertRepo(ctx, t, pool, "group/ordering-repo")

	_, err := s.ReplaceFileChunks(ctx, repoID, "main", "near.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "near", Embedding: unit(0)},
	})
	require.NoError(t, err)
	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "mid.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "mid", Embedding: mix()},
	})
	require.NoError(t, err)
	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "far.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "far", Embedding: unit(1)},
	})
	require.NoError(t, err)

	results, err := s.Search(ctx, []uuid.UUID{repoID}, "main", unit(0), 3)
	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.Equal(t, []string{"near.go", "mid.go", "far.go"},
		[]string{results[0].File, results[1].File, results[2].File},
		"Search must return rows in ascending cosine-distance order")
}

// TestSearch_ScopedByRepoIDs_ExcludesOutOfScopeRepos is the discriminating
// scoping test: it seeds a chunk in a repo NOT in the search scope with an
// embedding IDENTICAL to the query vector (so it would rank first if the
// repo-id filter were dropped or broadened), and proves it never appears.
func TestSearch_ScopedByRepoIDs_ExcludesOutOfScopeRepos(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(pool, logger)
	inScope := insertRepo(ctx, t, pool, "group/in-scope-repo")
	outOfScope := insertRepo(ctx, t, pool, "group/out-of-scope-repo")

	_, err := s.ReplaceFileChunks(ctx, inScope, "main", "in.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "in scope", Embedding: mix()},
	})
	require.NoError(t, err)
	_, err = s.ReplaceFileChunks(ctx, outOfScope, "main", "out.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "out of scope, identical to query", Embedding: unit(0)},
	})
	require.NoError(t, err)

	results, err := s.Search(ctx, []uuid.UUID{inScope}, "main", unit(0), 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "in.go", results[0].File, "a chunk identical to the query vector must still be excluded when its repo is out of scope")
}

// TestSearch_ScopedByTargetBranch_ExcludesOtherBranches proves the search
// also honors target_branch, not only repo_id: a chunk on a different
// branch of the SAME in-scope repo, again seeded identical to the query
// vector so it would rank first if branch filtering were broken, must not
// appear.
func TestSearch_ScopedByTargetBranch_ExcludesOtherBranches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(pool, logger)
	repoID := insertRepo(ctx, t, pool, "group/branch-repo")

	_, err := s.ReplaceFileChunks(ctx, repoID, "main", "main.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "on main", Embedding: mix()},
	})
	require.NoError(t, err)
	_, err = s.ReplaceFileChunks(ctx, repoID, "feature", "feature.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "on feature, identical to query", Embedding: unit(0)},
	})
	require.NoError(t, err)

	results, err := s.Search(ctx, []uuid.UUID{repoID}, "main", unit(0), 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "main.go", results[0].File)
}
