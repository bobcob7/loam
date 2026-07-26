//go:build integration

// See pool_integration_test.go's header for the podman/ryuk workaround note;
// it applies equally here. Run explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/db/... -run TestCopyFrom -v
package db

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testembed"
)

// chunkColumns is the column list InsertChunk (internal/db/gen) and this
// file's CopyFrom calls both write, in table order.
var chunkColumns = []string{"id", "repo_id", "target_branch", "file", "start_line", "end_line", "content", "embedding"}

// TestCopyFromChunksEmbeddingThroughRegisteredPool is loam-36t's positive
// proof: pgx.CopyFrom writes chunks.embedding through the BINARY copy
// protocol, a code path assertVectorRoundTrips (pool_integration_test.go)
// never exercises because it uses Exec/QueryRow -- the extended query
// protocol, which pgvector.Vector's database/sql.Scanner/driver.Valuer
// fallback satisfies with zero type registration. loam-47l's NOTES record
// that CopyFrom of a pgvector.Vector into a real registered-pool-free
// connection failed with "ERROR: vector cannot have more than 16000
// dimensions (SQLSTATE 54000)": pgx wrote the Valuer's TEXT output into the
// binary COPY stream and the server misparsed the leading bytes as a
// dimension header -- a corrupt encoding surfacing as a nonsense error,
// which no existing test caught because scalar Exec/Query round-trip fine
// either way via that same Scanner/Valuer fallback. This test proves the
// fix -- a pool built by db.NewPool, i.e. with AfterConnect registration
// active -- actually closes that gap, by running CopyFrom for real and
// checking the rows land with the exact values intact, both through a
// scalar Scan AND an array Scan (SELECT ARRAY(...) into *[]pgvector.Vector),
// since the array path independently cannot be satisfied by the
// sql.Scanner fallback either -- it fails with "cannot scan unknown type
// (OID ...) in text format" instead.
//
// This is deliberately the ONLY test in this file: the regression this bead
// exists to catch -- AfterConnect registration silently dropped from
// db.NewPool -- is already covered three times over by tests that actually
// exercise db.NewPool: TestNewPoolConfigSetsAfterConnect (pool_test.go, a
// containerless unit test asserting AfterConnect != nil),
// TestNewPoolAgainstRealPostgres (pool_integration_test.go), and this test.
// An earlier version of this file also carried
// TestCopyFromChunksEmbeddingFailsWithoutRegistration, built on a raw
// pgxpool.New pool that never touches db.NewPool at all -- so nothing
// db.NewPool does could ever change its outcome, and under the
// AfterConnect-removed mutation below it stayed green while claiming (in
// its own doc comment) to guard exactly that regression. That is the "test
// advertises coverage it does not have" failure mode this bead was filed to
// eliminate, reproduced inside the fix meant to prevent it, so it was
// deleted rather than fixed in place: a same-shaped test pinning
// *pgconn.PgError's SQLSTATE 54000, pgvector's C-source error string, and
// pgx's "cannot scan unknown type" wording is real coverage of an upstream
// pgx/pgvector implementation detail, not of loam's own wiring, and its
// only realistic failure trigger going forward is a dependency version
// bump -- for the cost of an extra Postgres container every CI run.
//
// MUTATION PROOF (recorded here, not just in the bead's NOTES): commenting
// out `poolCfg.AfterConnect = pgxvec.RegisterTypes` in pool.go and rerunning
// this test reproduces the exact corrupt encoding above as a require.NoError
// assertion failure at the CopyFrom call below -- not a hang, not a panic:
//
//	Error Trace:  copyfrom_integration_test.go:89
//	Error:        Received unexpected error:
//	              ERROR: vector cannot have more than 16000 dimensions (SQLSTATE 54000)
//	Test:         TestCopyFromChunksEmbeddingThroughRegisteredPool
//	Messages:     CopyFrom into chunks.embedding must succeed through a pool with AfterConnect registration active
//
// Restoring pool.go (a no-op diff) makes it pass again.
func TestCopyFromChunksEmbeddingThroughRegisteredPool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
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

	pool, err := NewPool(ctx, Config{DatabaseURL: dsn, EncryptionKey: "key"}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	repoID := seedChunkRepo(ctx, t, pool, "group/copyfrom-repo")
	nearID, farID := uuid.New(), uuid.New()
	near := pgvector.NewVector(unitEmbedding(0))
	far := pgvector.NewVector(unitEmbedding(testembed.Dimension - 1))
	// start_line (1, then 3) is what assertChunkEmbeddingsArrayScan orders
	// by -- not file name, which stays purely descriptive here.
	rows := pgx.CopyFromRows([][]any{
		{pgUUID(nearID), repoID, "main", "near.go", int32(1), int32(2), "near content", near},
		{pgUUID(farID), repoID, "main", "far.go", int32(3), int32(4), "far content", far},
	})
	count, err := pool.CopyFrom(ctx, pgx.Identifier{"chunks"}, chunkColumns, rows)
	require.NoError(t, err, "CopyFrom into chunks.embedding must succeed through a pool with AfterConnect registration active")
	assert.EqualValues(t, 2, count)

	assertChunkEmbeddingScans(ctx, t, pool, pgUUID(nearID), near)
	assertChunkEmbeddingScans(ctx, t, pool, pgUUID(farID), far)
	assertChunkEmbeddingsArrayScan(ctx, t, pool, repoID, []pgvector.Vector{near, far})
}

// seedChunkRepo inserts a repos row (the FK every chunks row needs) and
// returns its id as a pgtype.UUID, ready to bind directly into further
// query args or CopyFrom rows without a separate string<->UUID conversion.
func seedChunkRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) pgtype.UUID {
	t.Helper()
	id := pgUUID(uuid.New())
	_, err := pool.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		id, name,
	)
	require.NoError(t, err)
	return id
}

// assertChunkEmbeddingScans proves one CopyFrom'd row scans back through
// pgvector.Vector with its exact values intact -- the scalar half of this
// bead's positive proof.
func assertChunkEmbeddingScans(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, want pgvector.Vector) {
	t.Helper()
	var got pgvector.Vector
	require.NoError(t, pool.QueryRow(ctx, `SELECT embedding FROM chunks WHERE id = $1`, id).Scan(&got))
	assert.Equal(t, want.Slice(), got.Slice())
}

// assertChunkEmbeddingsArrayScan proves the CopyFrom'd rows for repoID also
// scan back correctly through the ARRAY(...) -> *[]pgvector.Vector path --
// the array half of this bead's positive proof. Orders by start_line (1
// then 3), the actual insertion order want depends on, rather than file --
// decoupling this assertion from the fixture's filenames. A reorder here is
// still caught loudly: want holds unitEmbedding(0) and
// unitEmbedding(testembed.Dimension-1), maximally distinct vectors, so a
// swap produces an unmistakable diff, not a near-miss.
func assertChunkEmbeddingsArrayScan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID pgtype.UUID, want []pgvector.Vector) {
	t.Helper()
	var got []pgvector.Vector
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT ARRAY(SELECT embedding FROM chunks WHERE repo_id = $1 AND target_branch = 'main' ORDER BY start_line)`,
		repoID,
	).Scan(&got))
	require.Len(t, got, len(want))
	for i := range want {
		assert.Equal(t, want[i].Slice(), got[i].Slice())
	}
}

// unitEmbedding returns a testembed.Dimension-wide vector that is all zero
// except index i set to 1, matching code_intel_integration_test.go's
// unitVector helper -- sized off testembed.Dimension, not a bare 768
// literal, so it tracks 0002_code_intel.up.sql's chunks.embedding width.
func unitEmbedding(i int) []float32 {
	v := make([]float32, testembed.Dimension)
	v[i] = 1
	return v
}

// pgUUID converts a uuid.UUID into the pgtype.UUID CopyFrom and query args
// need to bind a uuid column, matching internal/db/gen/query_integration_test.go's
// helper of the same name and shape.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
