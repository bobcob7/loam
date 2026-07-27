//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; excluded from
// the default `go test ./...` run. Run explicitly with:
//
//	go test -tags=integration ./cmd/server/... -run TestPolicyStoreAdapter -v
//
// On podman also set TESTCONTAINERS_RYUK_DISABLED=true.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// newTestPolicyStoreAdapter migrates a fresh pgvector-enabled Postgres
// container and returns a policyStoreAdapter wired over the real
// sqlc-generated Queries -- this is production's exact composition
// (see run's own wiring in main.go), proving policyStoreAdapter's repo
// -> work-branch resolution chain against a real database, not just that
// it compiles.
func newTestPolicyStoreAdapter(t *testing.T) (policyStoreAdapter, *reposstore.Store, *workbranchstore.Store) {
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
	repos := reposstore.NewStore(gen.New(pool), logger)
	workBranches := workbranchstore.New(gen.New(pool), logger)
	return policyStoreAdapter{repos: repos, workBranches: workBranches}, repos, workBranches
}

// TestPolicyStoreAdapter_ResolvesRealWorkBranchAgainstRealPostgres proves
// the production composition end to end against a live database: create a
// repo and a work branch for real, then confirm GetWorkBranch resolves
// exactly that row by (repo name, branch name) -- the same lookup
// internal/refpolicy.EvaluatePush performs per ref update in a real push.
func TestPolicyStoreAdapter_ResolvesRealWorkBranchAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	adapter, repos, workBranches := newTestPolicyStoreAdapter(t)
	repo, err := repos.CreateRepo(t.Context(), reposstore.CreateRepoParams{
		Name: "acme/widgets", UpstreamURL: "https://example.com/acme/widgets.git", ForgeHost: "example.com", IndexedBranch: "main",
	})
	require.NoError(t, err)
	created, err := workBranches.Create(t.Context(), repo.ID, "wb-real", "main", "alice")
	require.NoError(t, err)

	got, err := adapter.GetWorkBranch(t.Context(), "acme/widgets", "wb-real")
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "alice", got.Author)
	assert.Equal(t, workbranchstore.StateDraft, got.State)
}

// TestPolicyStoreAdapter_UnknownBranchInAnEnrolledRepoMapsToErrNotFound
// proves the ordinary "not a registered work branch" case (a real,
// enrolled repo, but no such branch name) reports workbranchstore.ErrNotFound
// -- the sentinel refpolicy.EvaluatePush's rule-1 classification depends
// on -- not some other unrecognized error.
func TestPolicyStoreAdapter_UnknownBranchInAnEnrolledRepoMapsToErrNotFound(t *testing.T) {
	t.Parallel()
	adapter, repos, _ := newTestPolicyStoreAdapter(t)
	_, err := repos.CreateRepo(t.Context(), reposstore.CreateRepoParams{
		Name: "acme/widgets", UpstreamURL: "https://example.com/acme/widgets.git", ForgeHost: "example.com", IndexedBranch: "main",
	})
	require.NoError(t, err)

	_, err = adapter.GetWorkBranch(t.Context(), "acme/widgets", "no-such-branch")
	require.Error(t, err)
	assert.True(t, errors.Is(err, workbranchstore.ErrNotFound))
}

// TestPolicyStoreAdapter_UnenrolledRepoAlsoMapsToErrNotFound proves the
// defensive repo-not-found path (unreachable in production, since
// internal/handler/git already required enrollment before this hook ever
// ran) still reports the SAME sentinel rather than a distinguishable
// second error shape the one caller does not expect.
func TestPolicyStoreAdapter_UnenrolledRepoAlsoMapsToErrNotFound(t *testing.T) {
	t.Parallel()
	adapter, _, _ := newTestPolicyStoreAdapter(t)
	_, err := adapter.GetWorkBranch(t.Context(), "acme/never-enrolled", "wb-anything")
	require.Error(t, err)
	assert.True(t, errors.Is(err, workbranchstore.ErrNotFound))
}
