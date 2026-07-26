//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon. Run
// explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/rolestore/... -v
//
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround, not a CI
// setting). Uses testdb.PostgresImage (pgvector/pgvector:pg16), the image
// every integration test that runs migrations.Migrate must use, since
// migration 0002_code_intel issues CREATE EXTENSION vector.
package rolestore

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// newTestStore migrates a fresh Postgres container (proving roles_name_key
// and role_operations_operation_check actually exist against the real
// 0001_init schema, not just that the Go compiles) and returns a Store
// wired over the real sqlc-generated Queries.
func newTestStore(t *testing.T) *Store {
	t.Helper()
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
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return NewStore(pool, logger)
}

// TestGetRole_BuiltinAuthor_ResolvesSeededOperations proves GetRole
// resolves the "author" role seeded by 0001_init.up.sql to exactly the
// operations that migration grants it -- against the real schema, not a
// fixture this package invented.
func TestGetRole_BuiltinAuthor_ResolvesSeededOperations(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	role, err := store.GetRole(t.Context(), "author")
	require.NoError(t, err)
	assert.Equal(t, "author", role.Name)
	assert.True(t, role.Builtin, "the seeded author role is builtin")
	assert.ElementsMatch(t, []string{
		"work.start", "work.set", "work.request_review", "work.reply",
		"git.clone", "git.push", "work.read", "graph.query", "search",
	}, role.Operations, "must match 0001_init.up.sql's author seed exactly -- neither more nor fewer operations")
	assert.NotContains(t, role.Operations, "work.verdict", "an author must not be seeded with the reviewer-only work.verdict operation")
}

// TestGetRole_BuiltinReviewer_ResolvesSeededOperations mirrors the above
// for "reviewer", including proving it lacks work.start and git.push --
// the two operations roles.feature's "A reviewer may not start a work
// branch or push" scenario depends on.
func TestGetRole_BuiltinReviewer_ResolvesSeededOperations(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	role, err := store.GetRole(t.Context(), "reviewer")
	require.NoError(t, err)
	assert.Equal(t, "reviewer", role.Name)
	assert.True(t, role.Builtin)
	assert.ElementsMatch(t, []string{
		"work.read", "work.reply", "work.verdict", "git.clone", "graph.query", "search",
	}, role.Operations)
	assert.NotContains(t, role.Operations, "work.start", "a reviewer must not be able to start a work branch")
	assert.NotContains(t, role.Operations, "git.push", "a reviewer must not be able to push")
}

// TestGetRole_UnknownRole_ReturnsErrNotFound proves an unrecognized role
// name -- e.g. a typo'd Loam-Agent-Role header -- is rejected against the
// real roles_name_key unique index, not just a mocked assumption.
func TestGetRole_UnknownRole_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.GetRole(t.Context(), "not-a-real-role")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}
