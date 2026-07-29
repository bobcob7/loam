//go:build integration

// See integration_test.go's header for the podman/ryuk workaround note; it
// applies equally here. Run explicitly with:
//
//	go test -tags=integration ./internal/db/migrations/... -run TestSchemaCheck -v
package migrations

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/testdb"
)

// TestSchemaCheckAgainstRealPostgres exercises SchemaCheck against a real
// database and golang-migrate's REAL bookkeeping table, walking the
// states a running server's readiness probe has to tell apart. A fake
// pgx.Row could not establish any of this: the whole point of the check
// is that it reads the table migrate itself wrote, with the column names
// and the empty-table semantics migrate itself chose, so substituting a
// stub for the database would only assert this test's own assumptions
// about that table back at itself.
//
// The states, in order against one container:
//
//  1. Before any migration has run: the table does not exist at all.
//     (This is the state a pool connected to a virgin database sees, and
//     it must read as not-current, not as an unexpected crash.)
//  2. After Migrate: current.
//  3. dirty=true: not current -- a migration that started and never
//     finished, which needs an operator, not a retry.
//  4. Behind (version rolled back to 1): not current.
//  5. Ahead (version 999): not current -- something migrated this
//     database past what this binary understands.
//  6. Restored to the real version: current again, proving the check is a
//     live read rather than a value latched on first call.
func TestSchemaCheckAgainstRealPostgres(t *testing.T) {
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

	// Step 1 runs BEFORE Migrate, so this pool is deliberately built with
	// plain pgxpool.New rather than db.NewPool: db.NewPool's AfterConnect
	// registers the pgvector type and cannot connect to an unmigrated
	// database at all (internal/db/pool.go).
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	check := NewSchemaCheck(pool)

	err = check.CheckSchema(ctx)
	require.Error(t, err, "a database with no schema_migrations table at all must read as not-current")
	assert.Contains(t, err.Error(), migrationsTable)

	require.NoError(t, Migrate(ctx, dsn, logger))
	require.NoError(t, check.CheckSchema(ctx), "a freshly migrated database must read as current")

	current, err := embeddedVersion()
	require.NoError(t, err)

	setSchemaMigrations(ctx, t, pool, int64(current), true)
	err = check.CheckSchema(ctx)
	require.Error(t, err, "a dirty migration state must read as not-current")
	assert.ErrorIs(t, err, errSchemaDirty)

	setSchemaMigrations(ctx, t, pool, 1, false)
	err = check.CheckSchema(ctx)
	require.Error(t, err, "a database behind the embedded set must read as not-current")
	assert.ErrorIs(t, err, errSchemaNotCurrent)

	setSchemaMigrations(ctx, t, pool, 999, false)
	err = check.CheckSchema(ctx)
	require.Error(t, err, "a database ahead of the embedded set must read as not-current")
	assert.ErrorIs(t, err, errSchemaNotCurrent)

	setSchemaMigrations(ctx, t, pool, int64(current), false)
	require.NoError(t, check.CheckSchema(ctx), "restoring the real version must make the check pass again: it is a live read, not a latched verdict")
}

// setSchemaMigrations rewrites golang-migrate's bookkeeping row directly,
// which is how this test manufactures each state without needing a real
// half-applied migration.
func setSchemaMigrations(ctx context.Context, t *testing.T, pool *pgxpool.Pool, version int64, dirty bool) {
	t.Helper()
	tag, err := pool.Exec(ctx, `UPDATE `+migrationsTable+` SET version = $1, dirty = $2`, version, dirty)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "golang-migrate keeps exactly one bookkeeping row; this test's premise is wrong if it does not")
}

// TestSchemaCheckOnAnEmptyBookkeepingTable covers the one state the
// sequence above cannot reach by UPDATE: the table exists but holds no
// row. golang-migrate produces this transiently, and pgx surfaces it as
// ErrNoRows rather than an error about the table -- a branch that would
// otherwise be a 503 with a misleading reason.
func TestSchemaCheckOnAnEmptyBookkeepingTable(t *testing.T) {
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
	require.NoError(t, Migrate(ctx, dsn, logger))
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, `DELETE FROM `+migrationsTable)
	require.NoError(t, err)
	err = NewSchemaCheck(pool).CheckSchema(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSchemaNotCurrent)
}
