//go:build integration

package storesuite

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/testembed"
)

// unit returns a testembed.Dimension-wide vector that is all zero except
// index i set to 1, matching internal/chunkstore's own test vocabulary.
func unit(i int) []float32 {
	v := make([]float32, testembed.Dimension)
	v[i] = 1
	return v
}

// mix returns unit(0) + unit(1): a vector at a known, non-orthogonal angle
// to unit(0), giving three strictly-ordered, non-tied cosine distances (see
// TestStoreSuite_HNSWNearestNeighbourOrdering's doc comment).
func mix() []float32 {
	v := make([]float32, testembed.Dimension)
	v[0] = 1
	v[1] = 1
	return v
}

// TestStoreSuite_HNSWNearestNeighbourOrdering is Demo M1's second live
// proof, and the one loam-962 exists because of. Read that bead's NOTES
// before touching this test.
//
// THE HONEST PART: at this table's actual size, Postgres does NOT choose
// the chunks_embedding HNSW index for this query -- verified live with
// EXPLAIN, not assumed. chunks also carries chunks_repo_id_target_branch_idx
// (a btree on (repo_id, target_branch), 0002_code_intel.up.sql:92), and that
// btree satisfies this query's WHERE filter more cheaply than either a
// sequential scan or an HNSW probe at this row count. So an UNFORCED query
// here would silently demo a btree-then-exact-sort, not the index the demo
// claims to show -- exactly the failure mode loam-962's DESIGN warns about
// ("a live 'HNSW ordering' demo must either force the plan ... or use a
// dataset that reaches the index unaided ... otherwise the demo silently
// shows the wrong thing").
//
// WHY THIS FIXTURE, SPECIFICALLY, LOSES TO THE BTREE: NOT because this
// package's shared container mixes rows from several repos into one
// selective-looking filter -- checked, not assumed: this file is the ONLY
// place in this package that ever inserts into chunks (grep confirms it),
// so this repo's 3 rows are the entire table, the same ~100%-of-the-table
// shape as loam-962's own scaling experiment at its smallest sizes. The
// real cause is that this test (like every test in this file) never runs
// ANALYZE on those freshly-inserted rows, so the planner is working from
// default, not-measured selectivity statistics that underestimate how many
// rows match the repo_id + target_branch filter -- verified live during
// loam-962: ANALYZE-ing the identical 3-row, single-repo table flips the
// SAME query to a Seq Scan instead of this btree, because the planner's
// row estimate corrects itself once statistics exist. Do not read "the
// btree wins here" as "the btree wins at small scale in general" -- it is
// this specific never-ANALYZE-d, single-digit-row fixture that wins, and it
// is representative of neither a realistic table nor even this same table
// post-ANALYZE. loam-962's
// actual production question -- whether an UNFORCED query reaches
// chunks_embedding at a realistic table size -- was answered separately, by
// a scaling experiment that seeds a single repo up to tens of thousands of
// chunks and ANALYZEs before every EXPLAIN; see the DECISION comment on
// SearchChunksByEmbeddingScoped (internal/db/queries/chunks.sql) for the
// measured numbers. This test's job is narrower and stays narrower: prove
// the HNSW path is reachable and correct at all, deterministically, at
// whatever size this demo fixture happens to be -- not characterize when
// production reaches for it unaided.
//
// THE FIX, matching loam-962's recorded decision: force the plan explicitly
// and say so out loud. `SET LOCAL enable_seqscan = off` ALONE is verified
// insufficient (the btree still wins over a seq scan, so disabling seq scan
// does not touch this competition at all) -- the competing btree itself
// must be dropped for the duration of a rolled-back transaction. This is
// the identical technique internal/chunkstore's assertHNSWIndexReachable
// already proves and mutation-tests; this test does not re-derive that
// proof, it reuses the technique (necessarily re-implemented here in raw
// SQL, since a _test.go helper is not importable across packages) purely to
// narrate it for the demo, over data seeded through chunkstore.Store's real
// production API.
//
// Also per loam-962's NOTES: hnsw.iterative_scan is off by default in
// pgvector 0.8.x, so even this forced index scan applies the repo/branch
// filter at the executor AFTER the index returns tuples, not during the
// scan itself -- this test does not claim otherwise.
func TestStoreSuite_HNSWNearestNeighbourOrdering(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := mustPool(t)
	store := chunkstore.New(pool, testLogger())
	repoID := insertRepo(ctx, t, pool, "group/demo-hnsw-repo")

	t.Logf("seeding three chunks at known, non-tied cosine distances from the query vector")
	_, err := store.ReplaceFileChunks(ctx, repoID, "main", "near.go", []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "near", Embedding: unit(0)},
	})
	require.NoError(t, err)
	_, err = store.ReplaceFileChunks(ctx, repoID, "main", "mid.go", []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "mid", Embedding: mix()},
	})
	require.NoError(t, err)
	_, err = store.ReplaceFileChunks(ctx, repoID, "main", "far.go", []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "far", Embedding: unit(1)},
	})
	require.NoError(t, err)
	t.Logf("near.go distance 0, mid.go distance ~0.2929, far.go distance 1 -- strictly increasing, no ties")

	results, err := store.Search(ctx, []uuid.UUID{repoID}, "main", unit(0), 3)
	require.NoError(t, err)
	require.Len(t, results, 3)
	order := []string{results[0].File, results[1].File, results[2].File}
	t.Logf("Store.Search (production API, unforced plan) returned: %v", order)
	assert.Equal(t, []string{"near.go", "mid.go", "far.go"}, order, "nearest-first order must hold regardless of which plan Postgres chooses")

	forceHNSWPlanAndVerify(ctx, t, pool, repoID)
}

// forceHNSWPlanAndVerify drops the competing btree and disables sequential
// scan inside a transaction that is always rolled back (never perturbing
// sharedPool's schema for any other test in this package), captures a real
// EXPLAIN of the same query chunkstore.Store.Search issues
// (SearchChunksByEmbeddingScoped, internal/db/queries/chunks.sql), asserts
// the plan genuinely says "Index Scan using chunks_embedding" -- not merely
// that the index exists (internal/db/migrations already checks that via
// pg_indexes) -- and re-runs the query through the forced plan to prove the
// same nearest-first order comes back through the real index, not just an
// exact sort.
func forceHNSWPlanAndVerify(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `DROP INDEX chunks_repo_id_target_branch_idx`)
	require.NoError(t, err, "dropping the competing btree (loam-962) must succeed inside this rolled-back transaction")

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
	t.Logf("forced plan, competing btree dropped for this rolled-back transaction (loam-962's honest-demo requirement):\n%s", plan.String())
	assert.Contains(t, plan.String(), "Index Scan using chunks_embedding",
		"forcing enable_seqscan off and dropping the competing btree must route this query through the HNSW index")

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
	t.Logf("real HNSW index scan returned: %v", order)
	assert.Equal(t, []string{"near.go", "mid.go", "far.go"}, order, "the real HNSW index path must return the same nearest-first order as the unforced query")
}
