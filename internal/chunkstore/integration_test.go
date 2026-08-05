//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./internal/chunkstore/... -v
//
// On podman (e.g. a `podman machine` forwarding /var/run/docker.sock), also
// set TESTCONTAINERS_RYUK_DISABLED=true (see internal/db/migrations's
// integration_test.go for why -- podman's Docker-compat API does not resolve
// the reaper sidecar's expected `bridge` network, so the container never
// starts without it). This is a local convenience only, not a CI setting.
package chunkstore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testembed"
)

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

// TestSearch_HNSWNearestNeighbourOrdering proves Store.Search returns rows
// in ascending cosine-distance order, using three chunks at deterministic,
// non-tied distances from the query vector q = unit(0):
//
//   - "near.go" = unit(0): identical to q, cosine distance 0.
//   - "mid.go"  = unit(0)+unit(1): dot product 1, norm sqrt(2), so cosine
//     similarity 1/sqrt(2) ~= 0.7071 and distance ~= 0.2928932188.
//   - "far.go"  = unit(1): orthogonal to q, cosine similarity 0, distance 1.
//
// 0 < 0.2928932188 < 1 with no ties, so "nearest-first" has exactly one
// correct order -- the file names read as a result (near/mid/far), not
// number soup.
//
// Calibration note, corrected again by loam-962 (an earlier version of this
// comment claimed a Seq Scan and "no btree on (repo_id, target_branch)" --
// both wrong the same way loam-962's own NOTES record a sibling comment
// once was): chunks_repo_id_target_branch_idx DOES exist
// (0002_code_intel.up.sql:92), and it is what Postgres actually picks here,
// confirmed live with EXPLAIN, not assumed -- because this test (like every
// test in this file) never runs ANALYZE on its freshly-inserted rows, so
// the planner falls back to default, not-measured selectivity statistics
// that underestimate how many of this table's rows match the repo_id +
// target_branch filter, making the btree look artificially cheap. Verified
// by adding an ANALYZE before the same EXPLAIN, live, during loam-962: the
// identical 3-row table then flips to a Seq Scan instead, because the
// planner's row estimate corrects from 1 to the true 3 (this repo has
// ~100% of the table, single-repo/single-branch, same as loam-962's own
// scaling experiment). So the plan this test happens to see is an artifact
// of skipping ANALYZE on a tiny table, not evidence about production
// (which does ANALYZE): loam-962's realistic-scale experiment (single repo,
// 100 to 50,000 chunks, ANALYZE before every EXPLAIN) found the unforced
// planner reaches for chunks_embedding well before "realistic" scale --
// see the DECISION comment on SearchChunksByEmbeddingScoped
// (internal/db/queries/chunks.sql) for the measured numbers. Either way,
// neither the btree nor a Seq Scan is chunks_embedding, so what THIS
// assertion actually proves is narrower than the name first suggested:
// Postgres computes and sorts the <=> operator correctly over the filtered
// rows. It is an exact sort here, not an approximate HNSW traversal, and
// pgvector's ef_search value is irrelevant to it because the index is never
// consulted for this query shape/size.
// assertHNSWIndexReachable, below, is the assertion that actually exercises
// the HNSW path: it forces the planner onto chunks_embedding with `SET
// LOCAL enable_seqscan = off`, confirms the plan really says "Index Scan
// using chunks_embedding" (not asserted, checked), and confirms the SAME
// near/mid/far order comes back through it. loam-ejr's live demo should use
// that forced-index form (or a dataset shaped so the planner reaches for
// the index unaided) if the point is to visibly demonstrate the HNSW path
// rather than an exact sort that happens to produce the same answer.
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

	assertHNSWIndexReachable(ctx, t, pool, repoID)
}

// assertHNSWIndexReachable proves the chunks_embedding HNSW index is not
// merely present (internal/db/migrations/code_intel_integration_test.go
// already checks that via pg_indexes) but actually reachable and correct.
// `SET LOCAL enable_seqscan = off` alone is not enough here, verified
// live: chunks also carries chunks_repo_id_target_branch_idx (a btree on
// (repo_id, target_branch), from 0002_code_intel.up.sql), and with this
// small a table the planner satisfies the WHERE filter via that btree and
// sorts the tiny (often single-row) result directly -- cheaper than an
// HNSW probe, and never touching chunks_embedding at all, even with
// sequential scan disabled. So this also drops that btree for the
// transaction (transactional DDL; rolled back at the end, never committed)
// to remove the competing plan, leaving Postgres exactly two options for
// the ORDER BY ... LIMIT with sequential scan penalized: the HNSW index, or
// a full sort. Only then does EXPLAIN legitimately confirm
// "Index Scan using chunks_embedding" instead of a different index
// covering the same rows. Both the DROP INDEX and SET LOCAL are scoped to
// this rolled-back transaction, so this never perturbs sharedDSN's schema
// or session defaults for any other test in the package.
func assertHNSWIndexReachable(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `DROP INDEX chunks_repo_id_target_branch_idx`)
	require.NoError(t, err, "dropping the competing btree must succeed inside the rolled-back transaction")

	const query = `SELECT file FROM chunks WHERE repo_id = ANY($1::uuid[]) AND target_branch = $2 ORDER BY embedding <=> $3 LIMIT $4`
	ids := []pgtype.UUID{pgUUID(repoID)}
	q := pgvector.NewVector(unit(0))

	explainRows, err := tx.Query(ctx, "EXPLAIN "+query, ids, "main", q, int32(3))
	require.NoError(t, err)
	var plan strings.Builder
	for explainRows.Next() {
		var line string
		require.NoError(t, explainRows.Scan(&line))
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	require.NoError(t, explainRows.Err())
	explainRows.Close()
	t.Logf("forced-index plan:\n%s", plan.String())
	assert.Contains(t, plan.String(), "Index Scan using chunks_embedding",
		"forcing enable_seqscan off must route this query through the HNSW index; actual plan:\n"+plan.String())

	orderRows, err := tx.Query(ctx, query, ids, "main", q, int32(3))
	require.NoError(t, err)
	defer orderRows.Close()
	var order []string
	for orderRows.Next() {
		var file string
		require.NoError(t, orderRows.Scan(&file))
		order = append(order, file)
	}
	require.NoError(t, orderRows.Err())
	assert.Equal(t, []string{"near.go", "mid.go", "far.go"}, order,
		"the real HNSW index path must return the same nearest-first order as the unforced query")
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

// --- SAVEPOINT isolation (loam-c94.24) ---

// badUTF8 is content Postgres itself rejects: 0xff is not a valid UTF-8
// byte in any position, so the INSERT fails with SQLSTATE 22021 (character
// not in repertoire) at the SERVER, which is the only way to get a genuine
// mid-transaction statement error rather than a Go-side one that never
// reaches the connection. Everything in this section depends on that
// distinction -- the defect under test lives entirely in Postgres's
// transaction semantics, so a failure the driver short-circuits would
// prove nothing.
func badUTF8(prefix string) string {
	return prefix + string([]byte{0xff})
}

// chunkContents reads back the content of every chunks row for repoID,
// keyed by file, so an assertion can name WHICH file survived rather than
// counting how many did. Each file in these tests carries distinct content
// for exactly that reason: with identical content per file, "the right rows
// were written" and "some rows were written" are indistinguishable.
func chunkContents(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) map[string][]string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT file, content FROM chunks WHERE repo_id = $1 ORDER BY file, start_line`, repoID)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var file, content string
		require.NoError(t, rows.Scan(&file, &content))
		out[file] = append(out[file], content)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestNewInTx_OneRejectedFileStillLetsTheOthersCommit is this bead's
// central proof, and it is deliberately an END-TO-COMMIT assertion: the
// rows are read back from a fresh pool query AFTER the caller's own
// tx.Commit returns, not from inside the transaction where every write
// still looks fine.
//
// Before loam-c94.24 this could not pass at any point in the batch.
// Postgres aborts the ENTIRE transaction the instant one statement in it
// errors, so the bad file's INSERT poisoned everything: the two later
// files' statements failed with SQLSTATE 25P02 and the commit itself
// failed with "commit unexpectedly resulted in rollback", taking the file
// that had ALREADY landed down with it.
//
// The rejected file sits in the MIDDLE on purpose. A file that fails last
// leaves nothing after it to be poisoned, and a file that fails first
// leaves nothing before it to be discarded; only a middle failure tests
// both directions at once.
func TestNewInTx_OneRejectedFileStillLetsTheOthersCommit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(ctx, t, pool, "group/savepoint-batch-repo")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	s := NewInTx(tx, logger)

	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "before.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func Before() {}", Embedding: unit(0)},
	})
	require.NoError(t, err)

	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "bad.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: badUTF8("func Bad() {}"), Embedding: unit(1)},
	})
	require.Error(t, err, "Postgres must actually reject the invalid UTF-8, or this test proves nothing about surviving a rejection")
	assert.NotErrorIs(t, err, ErrTransactionUnusable,
		"a rejection the savepoint unwound must NOT be reported as an unusable transaction -- that sentinel is what makes a batch loop give up")

	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "after.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func After() {}", Embedding: unit(2)},
	})
	require.NoError(t, err, "the transaction must still be usable after a rejection -- before loam-c94.24 this failed with SQLSTATE 25P02")

	require.NoError(t, tx.Commit(ctx), "the commit must succeed; before loam-c94.24 it failed with \"commit unexpectedly resulted in rollback\"")

	assert.Equal(t, map[string][]string{
		"before.go": {"func Before() {}"},
		"after.go":  {"func After() {}"},
	}, chunkContents(ctx, t, pool, repoID),
		"exactly the two good files' own contents must be committed -- the one before the rejection and the one after it -- and nothing for the rejected file")
}

// TestNewInTx_RejectedReplace_LeavesThePriorChunksIntactThroughTheCommit is
// acceptance criterion 3, and it is verified WITH savepoints rather than
// carried over from the whole-transaction-abort case, where "prior chunks
// intact" was true only because nothing committed at all.
//
// Now the transaction DOES commit, so the delete and the inserts must
// unwind TOGETHER inside it: ROLLBACK TO SAVEPOINT has to undo the DELETE
// that ReplaceFileChunks issues first, not just the INSERT that failed.
// If it did not, the file would commit with zero chunks -- silently
// unsearchable, and far worse than stale.
//
// The prior chunks are two rows with distinct content, so "the prior
// chunks survived" cannot be confused with "some row survived".
func TestNewInTx_RejectedReplace_LeavesThePriorChunksIntactThroughTheCommit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(ctx, t, pool, "group/savepoint-prior-repo")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	_, err := New(pool, logger).ReplaceFileChunks(ctx, repoID, "main", "stale.go", []ChunkInput{
		{StartLine: 1, EndLine: 5, Content: "func Old1() {}", Embedding: unit(0)},
		{StartLine: 6, EndLine: 9, Content: "func Old2() {}", Embedding: unit(1)},
	})
	require.NoError(t, err)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	s := NewInTx(tx, logger)

	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "stale.go", []ChunkInput{
		{StartLine: 1, EndLine: 5, Content: "func New1() {}", Embedding: unit(2)},
		{StartLine: 6, EndLine: 9, Content: badUTF8("func New2() {}"), Embedding: unit(3)},
	})
	require.Error(t, err, "the second chunk's invalid UTF-8 must be rejected AFTER the delete and the first insert already ran, or the unwind is not being tested")

	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "other.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func Other() {}", Embedding: unit(4)},
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, map[string][]string{
		"stale.go": {"func Old1() {}", "func Old2() {}"},
		"other.go": {"func Other() {}"},
	}, chunkContents(ctx, t, pool, repoID),
		"the rejected file must keep BOTH of its prior chunks -- the DELETE must have unwound with the INSERTs, never leaving it emptied or half-replaced -- while the unrelated file still commits")
}

// TestNewInTx_ManyConsecutiveRejections_LeaveTheTransactionCommittable
// guards the leak the unit test can only pin at the statement level: every
// rejection is unwound with ROLLBACK TO SAVEPOINT, which leaves the
// savepoint ESTABLISHED, so a missing RELEASE would accumulate one live
// savepoint per rejected file. That costs memory on the server and, past
// Postgres's 64-subtransaction-per-backend cache, forces every later
// snapshot onto the slow suboverflowed path -- both invisible to a
// two-file test.
//
// 80 rejections is chosen to cross that 64 threshold rather than to be a
// round number.
func TestNewInTx_ManyConsecutiveRejections_LeaveTheTransactionCommittable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(ctx, t, pool, "group/savepoint-many-repo")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	s := NewInTx(tx, logger)

	for i := range 80 {
		_, err := s.ReplaceFileChunks(ctx, repoID, "main", fmt.Sprintf("bad/%d.go", i), []ChunkInput{
			{StartLine: 1, EndLine: 1, Content: badUTF8(fmt.Sprintf("func Bad%d() {}", i)), Embedding: unit(0)},
		})
		require.Error(t, err, "rejection %d must be reported", i)
		require.NotErrorIs(t, err, ErrTransactionUnusable, "rejection %d must still leave the transaction usable", i)
	}
	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "good.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func Good() {}", Embedding: unit(0)},
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, map[string][]string{"good.go": {"func Good() {}"}}, chunkContents(ctx, t, pool, repoID))
}

// TestNewInTx_DeadConnection_ReportsTheTransactionUnusable is the other
// half of the classification loam-c94.24 folds in. Killing the backend
// from a SECOND connection leaves the store's transaction with a socket
// that is simply gone: the resulting error carries no *pgconn.PgError and
// matches none of pgx's named sentinels, so internal/ingest/vectors would
// have classified it as a per-file REJECTION and retried it once per
// remaining file -- turning one infrastructure failure into N, precisely
// because savepoints made rejections survivable.
//
// ErrTransactionUnusable is not a guess about the error's shape: it is
// what this store OBSERVES when its own savepoint statement cannot reach
// the server.
func TestNewInTx_DeadConnection_ReportsTheTransactionUnusable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(ctx, t, pool, "group/savepoint-dead-repo")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	s := NewInTx(tx, logger)
	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "alive.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func Alive() {}", Embedding: unit(0)},
	})
	require.NoError(t, err)

	var backendPID int
	require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID))
	killer := newRegisteredPool(t, sharedDSN)
	_, err = killer.Exec(ctx, `SELECT pg_terminate_backend($1)`, backendPID)
	require.NoError(t, err)

	_, err = s.ReplaceFileChunks(ctx, repoID, "main", "after-death.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func AfterDeath() {}", Embedding: unit(1)},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTransactionUnusable,
		"a connection that is simply gone must be reported as an unusable transaction, not as a per-file rejection a batch loop would keep retrying")
}

// TestNewInTx_AlreadyAbortedTransaction_ReportsUnusableRatherThanABare25P02
// pins the server behaviour that makes a bare SQLSTATE 25P02 unreachable
// through this constructor, which is a claim
// internal/ingest/vectors.Persist's error classification now rests on and
// should not have to take on trust.
//
// SAVEPOINT is itself a statement, so on a transaction some other
// participant already aborted it fails exactly like any other statement
// would -- with 25P02, in_failed_sql_transaction. Because
// savepointTransactor establishes the savepoint BEFORE running fn, that
// failure is reported as ErrTransactionUnusable and fn's queries never
// run. So a Store built with NewInTx cannot hand its caller a bare 25P02:
// the sentinel is always there too, and vectors' classifier matches it
// first.
//
// The transaction is poisoned here by a statement this package did not
// issue (a division by zero on the caller's own tx), which is the only
// honest way to model the case: a participant other than this store
// breaking the transaction is the entire premise.
func TestNewInTx_AlreadyAbortedTransaction_ReportsUnusableRatherThanABare25P02(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newRegisteredPool(t, sharedDSN)
	repoID := insertRepo(ctx, t, pool, "group/savepoint-poisoned-repo")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT 1/0`)
	require.Error(t, err, "the transaction must actually be poisoned by a statement this store did not issue")

	_, err = NewInTx(tx, logger).ReplaceFileChunks(ctx, repoID, "main", "after-poison.go", []ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func AfterPoison() {}", Embedding: unit(0)},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTransactionUnusable,
		"a transaction another participant already aborted is not a per-file rejection, and the savepoint statement is what discovers that")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the server's own error must stay in the chain")
	assert.Equal(t, pgerrcode.InFailedSQLTransaction, pgErr.Code,
		"the underlying code is 25P02 -- but it arrives WRAPPED in the sentinel, which is the whole point: internal/ingest/vectors matches the sentinel first and never has to reason about a bare 25P02 from this store")
	assert.Contains(t, err.Error(), "establishing savepoint",
		"the failure must be the SAVEPOINT itself, before any of the file's own statements ran")
}

// BenchmarkReplaceFileChunks_SavepointOverhead is acceptance criterion 4:
// the savepoint's cost measured on a realistic batch instead of assumed
// free. The "savepoint" and "none" arms run the SAME ReplaceFileChunks code
// against the same server on the same connection, differing only in the
// transactor, so the delta between them is the two extra round trips
// (SAVEPOINT, RELEASE SAVEPOINT) and nothing else.
//
// The batch shape (500 files x 4 chunks) is drawn from what an incremental
// ingest of a medium repository actually looks like, not from what makes
// the number flattering: the savepoint cost is per FILE while the write
// cost is per CHUNK, so the ratio is worst at a low chunks-per-file count.
//
// The third arm, "savepointstatementsonly", exists because the first two
// turned out to be the wrong instrument on their own: run repeatedly, the
// container's own I/O variance is LARGER than the effect, and the arms
// swap places between runs. That is itself a finding worth keeping -- the
// overhead is below this setup's noise floor -- but it is not a number.
// The third arm measures the two statements in isolation over the same
// 500 iterations on the same connection, which is low-variance and gives
// the absolute per-file cost the other two can then be read against.
//
// Numbers from -benchtime 10x -count=3 on an M4 against the
// testcontainers Postgres this package already starts (loam-c94.24):
//
//	savepoint                1797 / 1766 / 1841 ms per 500-file batch
//	none                     1633 / 1639 / 1531 ms
//	savepointstatementsonly   228 /  229 /  223 ms  (~453us per file)
//	bareroundtrip             234 /  231 /  229 ms  (1000x SELECT 1)
//
// so ~+12% on the batch, and the isolated arm (227ms) accounts for the
// paired delta (200ms) within noise. The decisive comparison is the
// bareroundtrip arm: the same 1000 statements, but "SELECT 1", ran
// 234 / 231 / 229 ms -- indistinguishable from the same count of
// SAVEPOINT/RELEASE. The savepoint statements do no measurable server-side
// work; the entire cost is round-trip latency, which is a property of
// where Postgres sits relative to the process, not of this change. That
// arm is committed rather than described precisely because it is the claim
// everything else here rests on: re-run it before believing the +12%,
// which is machine- and load-specific in a way the RATIO between these two
// arms is not.
//
// This is also why the narrower alternative the bead offers -- a savepoint
// around only the statements that can be rejected -- is not actually
// narrower here. ReplaceFileChunks issues a DELETE and then the INSERTs,
// every one of them can be rejected, and the DELETE must unwind WITH the
// INSERTs or a rejected file commits emptied instead of stale (see
// TestNewInTx_RejectedReplace_LeavesThePriorChunksIntactThroughTheCommit).
// The narrowest savepoint that preserves that guarantee starts before the
// DELETE and ends after the last INSERT, which is exactly the one that is
// there.
//
// Run it with:
//
//	go test -tags=integration ./internal/chunkstore/ -run '^$' -bench SavepointOverhead -benchtime 10x
func BenchmarkReplaceFileChunks_SavepointOverhead(b *testing.B) {
	const (
		files          = 500
		chunksPerFile  = 4
		benchmarkFiles = files
	)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx := context.Background()
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: sharedDSN}, logger)
	if err != nil {
		b.Fatal(err)
	}
	defer pool.Close()
	var repoID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES (gen_random_uuid(), $1, 'https://example.com/repo.git', 'example.com', 'main')
		 RETURNING id`, "group/bench-savepoint-repo").Scan(&repoID); err != nil {
		b.Fatal(err)
	}
	inputs := make([]ChunkInput, chunksPerFile)
	for i := range inputs {
		inputs[i] = ChunkInput{StartLine: i*10 + 1, EndLine: i*10 + 9, Content: fmt.Sprintf("func Bench%d() { /* %s */ }", i, strings.Repeat("x", 200)), Embedding: unit(i)}
	}
	for _, tc := range []struct {
		name string
		with func(tx pgx.Tx, q queries) transactor
	}{
		{name: "savepoint", with: func(tx pgx.Tx, q queries) transactor { return &savepointTransactor{tx: tx, q: q} }},
		{name: "none", with: func(_ pgx.Tx, q queries) transactor { return benchNoSavepoint{q: q} }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				tx, err := pool.Begin(ctx)
				if err != nil {
					b.Fatal(err)
				}
				q := gen.New(tx)
				s := newStore(q, tc.with(tx, q), logger)
				b.StartTimer()
				for f := range benchmarkFiles {
					if _, err := s.ReplaceFileChunks(ctx, repoID, "main", fmt.Sprintf("pkg/bench/f%d.go", f), inputs); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if err := tx.Rollback(ctx); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
	// bareroundtrip is the control the "savepointstatementsonly" arm is only
	// meaningful against: the same COUNT of the cheapest statement Postgres
	// can answer, on the same connection, in the same transaction. If the
	// savepoint arm matches it, SAVEPOINT and RELEASE are doing no
	// server-side work worth measuring and the overhead is round-trip
	// latency. That is the doc comment's decisive claim, and it lives here
	// so a reader who doubts the +12% can re-run it rather than take it from
	// a comment (it was measured, deleted as scratch, and then had to be
	// rewritten by review to be checked -- which is the argument for
	// keeping it).
	b.Run("bareroundtrip", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			tx, err := pool.Begin(ctx)
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			for range benchmarkFiles {
				if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
					b.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := tx.Rollback(ctx); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	})
	b.Run("savepointstatementsonly", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			tx, err := pool.Begin(ctx)
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			for range benchmarkFiles {
				if _, err := tx.Exec(ctx, "SAVEPOINT "+fileSavepoint); err != nil {
					b.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT "+fileSavepoint); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if err := tx.Rollback(ctx); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	})
}

// benchNoSavepoint is the benchmark's baseline: exactly what NewInTx did
// before loam-c94.24 -- run fn against the caller's transaction with no
// savepoint around it. It lives in the test file rather than in the
// package so the production code carries no unused, unsafe alternative
// path that a future edit could reach for by accident.
type benchNoSavepoint struct {
	q queries
}

func (t benchNoSavepoint) withinTx(_ context.Context, fn func(q queries) error) error {
	return fn(t.q)
}
