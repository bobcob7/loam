//go:build integration

// See pool_integration_test.go's header for the podman/ryuk workaround note;
// it applies equally here. Run explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/db/... -run TestCopyFrom -v
package db

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
// dimension header. This test proves the fix -- a pool built by db.NewPool,
// i.e. with AfterConnect registration active -- actually closes that gap,
// by running CopyFrom for real and checking the rows land with the exact
// values intact, both through a scalar Scan AND an array Scan (SELECT
// ARRAY(...) into *[]pgvector.Vector), since the array path independently
// cannot be satisfied by the sql.Scanner fallback either (it fails with
// "cannot scan unknown type (OID ...) in text format" -- see
// TestCopyFromChunksEmbeddingFailsWithoutRegistration for that failure
// reproduced live on an unregistered pool).
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
	// File names are ordered "a-" before "b-" so ORDER BY file ASC (used by
	// assertChunkEmbeddingsArrayScan) returns near before far, matching the
	// order asserted below -- alphabetical "far.go" < "near.go" would
	// otherwise silently swap the expected order out from under this test.
	rows := pgx.CopyFromRows([][]any{
		{pgUUID(nearID), repoID, "main", "a-near.go", int32(1), int32(2), "near content", near},
		{pgUUID(farID), repoID, "main", "b-far.go", int32(3), int32(4), "far content", far},
	})
	count, err := pool.CopyFrom(ctx, pgx.Identifier{"chunks"}, chunkColumns, rows)
	require.NoError(t, err, "CopyFrom into chunks.embedding must succeed through a pool with AfterConnect registration active")
	assert.EqualValues(t, 2, count)

	assertChunkEmbeddingScans(ctx, t, pool, pgUUID(nearID), near)
	assertChunkEmbeddingScans(ctx, t, pool, pgUUID(farID), far)
	assertChunkEmbeddingsArrayScan(ctx, t, pool, repoID, []pgvector.Vector{near, far})
}

// TestCopyFromChunksEmbeddingFailsWithoutRegistration is loam-36t's
// discriminating negative proof, run permanently (not just as a one-off
// manual mutation) so a future regression that drops AfterConnect
// registration is caught by `go test -tags=integration` rather than only by
// an agent remembering to redo this by hand. It deliberately builds the pool
// with pgxpool.New directly, bypassing db.NewPool/AfterConnect entirely, and
// asserts BOTH independent binary-protocol failures the pgvector hazard
// note describes: CopyFrom's corrupt encoding (SQLSTATE 54000, a nonsense
// "too many dimensions" error) and the array-scan's honest "unknown type"
// error. If AfterConnect registration were ever silently restored on this
// pool, both assertions below would fail on the "must fail" require.Error
// calls -- an assertion failure, not a hang or panic.
func TestCopyFromChunksEmbeddingFailsWithoutRegistration(t *testing.T) {
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

	// Deliberately NOT db.NewPool: no AfterConnect, so pgxvec.RegisterTypes
	// never runs on any connection this pool hands out. This reproduces the
	// exact unregistered state loam-36t's DESCRIPTION found live on
	// bead/schema-sqlc (which predates loam-47l's AfterConnect fix).
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	repoID := seedChunkRepo(ctx, t, pool, "group/unregistered-repo")
	assertCopyFromFailsWithCorruptEncoding(ctx, t, pool, repoID)
	assertArrayScanFailsWithUnknownType(ctx, t, pool, repoID)
}

// assertCopyFromFailsWithCorruptEncoding attempts the same CopyFrom shape
// TestCopyFromChunksEmbeddingThroughRegisteredPool proves succeeds, against
// an unregistered pool, and requires it fail with the specific corrupt
// binary encoding loam-36t's DESCRIPTION reproduced: pgx wrote the
// driver.Valuer's TEXT output into the binary COPY stream and Postgres
// misparsed the leading bytes as a vector dimension count, so the server's
// complaint is SQLSTATE 54000 ("vector cannot have more than 16000
// dimensions") -- a nonsense error, not a clean type mismatch. Matched via
// errors.As against *pgconn.PgError so this asserts the exact error
// identity, not merely "an error occurred".
func assertCopyFromFailsWithCorruptEncoding(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID pgtype.UUID) {
	t.Helper()
	rows := pgx.CopyFromRows([][]any{
		{pgUUID(uuid.New()), repoID, "main", "unregistered.go", int32(1), int32(2), "content", pgvector.NewVector(unitEmbedding(0))},
	})
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"chunks"}, chunkColumns, rows)
	require.Error(t, err, "CopyFrom of a pgvector.Vector must fail on a pool with no AfterConnect registration -- a silent pass here would mean the corrupt binary encoding this bead exists to catch went undetected")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a *pgconn.PgError, got %T: %v", err, err)
	assert.Equal(t, "54000", pgErr.Code, "the corrupt encoding surfaces as SQLSTATE 54000, not a clean type-mismatch error")
	assert.Contains(t, pgErr.Message, "vector cannot have more than", "expected the misparsed-dimension-header message this bead's DESCRIPTION reproduced")
}

// assertArrayScanFailsWithUnknownType is the array-scan half of loam-36t's
// negative proof: it first inserts one chunk via plain Exec (the scalar
// Insert path pgvector.Vector's driver.Valuer/sql.Scanner fallback satisfies
// even with no registration at all -- the false-confidence hazard this
// bead's suite otherwise relies on), then attempts SELECT ARRAY(...) into
// *[]pgvector.Vector, which the sql.Scanner fallback cannot satisfy because
// pgx never hands it a text-format cell for an array element -- it fails
// with "cannot scan unknown type (OID ...) in text format" instead.
func assertArrayScanFailsWithUnknownType(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID pgtype.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO chunks (id, repo_id, target_branch, file, start_line, end_line, content, embedding) VALUES ($1, $2, 'main', 'array-scan.go', 1, 2, 'content', $3)`,
		pgUUID(uuid.New()), repoID, pgvector.NewVector(unitEmbedding(0)),
	)
	require.NoError(t, err, "scalar Exec must still succeed on an unregistered pool -- pgvector.Vector's driver.Valuer fallback covers it, which is exactly why this path alone is not a discriminating test")
	var got []pgvector.Vector
	err = pool.QueryRow(ctx,
		`SELECT ARRAY(SELECT embedding FROM chunks WHERE repo_id = $1 AND target_branch = 'main')`,
		repoID,
	).Scan(&got)
	require.Error(t, err, "array-scan of vector columns must fail on an unregistered pool -- the sql.Scanner fallback cannot satisfy this path")
	assert.Contains(t, err.Error(), "cannot scan unknown type", "expected pgx's unknown-OID array-scan failure, not the scalar fallback succeeding")
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
// the array half of this bead's positive proof, and the one the sql.Scanner
// fallback cannot satisfy at all (see assertArrayScanFailsWithUnknownType).
func assertChunkEmbeddingsArrayScan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID pgtype.UUID, want []pgvector.Vector) {
	t.Helper()
	var got []pgvector.Vector
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT ARRAY(SELECT embedding FROM chunks WHERE repo_id = $1 AND target_branch = 'main' ORDER BY file)`,
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
