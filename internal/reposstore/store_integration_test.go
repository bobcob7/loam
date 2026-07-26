//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./internal/reposstore/... -run TestStoreAgainstRealPostgres -v
//
// On podman also set TESTCONTAINERS_RYUK_DISABLED=true (see
// internal/db/migrations/integration_test.go for why). Uses the
// pgvector/pgvector:pg16 image, not plain postgres:16-alpine: migrations.Migrate
// applies BOTH 0001_init and 0002_code_intel, and 0002 runs `CREATE EXTENSION
// IF NOT EXISTS vector` -- a plain image has no such extension to create at
// all, so Migrate itself would fail before this package's tables ever exist.
package reposstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
)

// newTestStore migrates a fresh pgvector-enabled Postgres container
// (proving repos_name_key and the repo_target_branches FK actually exist,
// not just that the Go compiles) and returns a Store wired over the real
// sqlc-generated Queries.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, "pgvector/pgvector:pg16",
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
	return NewStore(gen.New(pool), logger)
}

func TestCreateRepoAndGetRepoByNameAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created, err := store.CreateRepo(t.Context(), CreateRepoParams{
		Name:          "group/real-pg",
		UpstreamURL:   "https://example.com/group/real-pg.git",
		ForgeHost:     "example.com",
		IndexedBranch: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, "idle", created.SyncState)
	byName, err := store.GetRepoByName(t.Context(), "group/real-pg")
	require.NoError(t, err, "GetRepoByName must resolve the name just created via repos_name_key")
	assert.Equal(t, created.ID, byName.ID)
	byID, err := store.GetRepoByID(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.Name, byID.Name)
}

func TestCreateRepoDuplicateNameViolatesRepoNameKey(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	params := CreateRepoParams{
		Name:          "group/duplicate",
		UpstreamURL:   "https://example.com/group/duplicate.git",
		ForgeHost:     "example.com",
		IndexedBranch: "main",
	}
	_, err := store.CreateRepo(t.Context(), params)
	require.NoError(t, err)
	_, err = store.CreateRepo(t.Context(), params)
	require.Error(t, err, "a second repo with the same name must be rejected by repos_name_key UNIQUE (name)")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "the underlying error must be a real Postgres constraint violation, not something this store synthesizes")
	assert.Equal(t, "23505", pgErr.Code, "unique_violation")
	assert.Equal(t, "repos_name_key", pgErr.ConstraintName)
}

func TestGetRepoNotFoundAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	missing, err := uuid.NewV7()
	require.NoError(t, err)
	_, err = store.GetRepoByID(t.Context(), missing)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
	_, err = store.GetRepoByName(t.Context(), "group/does-not-exist")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

func TestListReposPaginatesWithRealCount(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	for i := 0; i < 3; i++ {
		name := "group/list-" + string(rune('a'+i))
		_, err := store.CreateRepo(t.Context(), CreateRepoParams{
			Name:          name,
			UpstreamURL:   "https://example.com/" + name + ".git",
			ForgeHost:     "example.com",
			IndexedBranch: "main",
		})
		require.NoError(t, err)
	}
	firstPage, err := store.ListRepos(t.Context(), Page{Limit: 2, Offset: 0})
	require.NoError(t, err)
	assert.Equal(t, 3, firstPage.Total, "COUNT(*) must reflect every matching row, not just the page returned")
	require.Len(t, firstPage.Repos, 2)
	secondPage, err := store.ListRepos(t.Context(), Page{Limit: 2, Offset: 2})
	require.NoError(t, err)
	assert.Equal(t, 3, secondPage.Total)
	require.Len(t, secondPage.Repos, 1, "the last page must return the remaining single row")
}

func TestUpdateRepoAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	created, err := store.CreateRepo(t.Context(), CreateRepoParams{
		Name:          "group/update-me",
		UpstreamURL:   "https://example.com/group/update-me.git",
		ForgeHost:     "example.com",
		IndexedBranch: "main",
	})
	require.NoError(t, err)
	updated, err := store.UpdateRepo(t.Context(), created.ID, UpdateRepoParams{
		UpstreamURL:   "https://example.com/group/update-me.git",
		ForgeHost:     "example.com",
		IndexedBranch: "release",
	})
	require.NoError(t, err)
	assert.Equal(t, "release", updated.IndexedBranch)
	assert.Equal(t, "group/update-me", updated.Name, "name must survive an update untouched -- there is no rename path")
	reread, err := store.GetRepoByID(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "release", reread.IndexedBranch)
}

func TestUpdateRepoNotFoundAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	missing, err := uuid.NewV7()
	require.NoError(t, err)
	_, err = store.UpdateRepo(t.Context(), missing, UpdateRepoParams{IndexedBranch: "main"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

func TestTargetBranchLifecycleAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	repo, err := store.CreateRepo(t.Context(), CreateRepoParams{
		Name:          "group/branches",
		UpstreamURL:   "https://example.com/group/branches.git",
		ForgeHost:     "example.com",
		IndexedBranch: "main",
	})
	require.NoError(t, err)
	added, err := store.AddTargetBranch(t.Context(), repo.ID, "main")
	require.NoError(t, err)
	assert.False(t, added.IngestedRef.Ok, "a brand-new target branch must report no ingested ref -- the full-rebuild signal")
	again, err := store.AddTargetBranch(t.Context(), repo.ID, "main")
	require.NoError(t, err, "adding an already-enrolled branch must be idempotent, not a conflict error")
	assert.False(t, again.IngestedRef.Ok)
	_, err = store.AddTargetBranch(t.Context(), repo.ID, "develop")
	require.NoError(t, err)
	branches, err := store.ListTargetBranches(t.Context(), repo.ID)
	require.NoError(t, err)
	require.Len(t, branches, 2)
	ref, err := store.IngestedRef(t.Context(), repo.ID, "main")
	require.NoError(t, err)
	assert.False(t, ref.Ok, "reading a never-ingested branch must report Ok=false, never a zero-value ref mistaken for real")
	advanced, err := store.AdvanceIngestedRef(t.Context(), repo.ID, "main", "abc123", time.Now().UTC(), []byte(`{"grammar":1}`))
	require.NoError(t, err)
	require.True(t, advanced.IngestedRef.Ok)
	assert.Equal(t, "abc123", advanced.IngestedRef.Ref)
	rereadRef, err := store.IngestedRef(t.Context(), repo.ID, "main")
	require.NoError(t, err)
	require.True(t, rereadRef.Ok)
	assert.Equal(t, "abc123", rereadRef.Ref)
	require.NoError(t, store.RemoveTargetBranch(t.Context(), repo.ID, "develop"))
	remaining, err := store.ListTargetBranches(t.Context(), repo.ID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "main", remaining[0].Branch)
	err = store.RemoveTargetBranch(t.Context(), repo.ID, "develop")
	require.Error(t, err, "removing an already-removed branch must not silently succeed")
	assert.ErrorIs(t, err, errNotFound)
}

func TestAddTargetBranchViolatesForeignKeyForUnknownRepo(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	unknownRepo, err := uuid.NewV7()
	require.NoError(t, err)
	_, err = store.AddTargetBranch(t.Context(), unknownRepo, "main")
	require.Error(t, err, "repo_target_branches.repo_id REFERENCES repos(id) must reject an unknown repo")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	assert.Equal(t, "23503", pgErr.Code, "foreign_key_violation")
}
