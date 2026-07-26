//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./internal/mirrorsync/... -run TestStoreRepoLister -v
//
// On podman also set TESTCONTAINERS_RYUK_DISABLED=true (see
// internal/db/migrations/integration_test.go for why).
package mirrorsync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/testdb"
)

// sharedDSN is the one migrated Postgres every test in this package runs
// against, started once in TestMain rather than one container per test
// (docs/bead-workflow.md's container-discipline convention, also used by
// internal/chunkstore and internal/codegraph).
var sharedDSN string

// TestMain starts one pgvector-enabled Postgres container, applies the
// production migration set, and hands every test in this package the
// same DSN, tearing the container down once after the whole package's
// tests finish.
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

// newTestReposStore opens its own pool over the shared, already-migrated
// container and wraps it in a real *reposstore.Store. Each test seeds its
// own uniquely-named repos, so tests can run in parallel against the one
// shared container without interfering with each other.
func newTestReposStore(t *testing.T) *reposstore.Store {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	pool, err := pgxpool.New(ctx, sharedDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return reposstore.NewStore(gen.New(pool), logger)
}

// TestStoreRepoListerListsEveryEnrolledRepoAgainstRealPostgres drives a
// real *reposstore.Store through the mirrorsync.RepoLister interface via
// StoreRepoLister (loam-13z's acceptance criterion: "a test drives the
// real store through the interface"), proving both that Store satisfies
// repoNameLister structurally and that the adapter's conversion holds
// against a real database, not just a mock. lister is declared as the
// RepoLister interface itself, not *StoreRepoLister, so a compile break
// here would mean the production seam -- not just this concrete type --
// stopped being satisfiable.
func TestStoreRepoListerListsEveryEnrolledRepoAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	store := newTestReposStore(t)
	// Created out of alphabetical order: ListAllRepoNames orders by name
	// (internal/db/queries/repos.sql), so the returned RepoIDs must come
	// back sorted regardless of enrollment order.
	names := []string{"group/producer-c", "group/producer-a", "group/producer-b"}
	for _, name := range names {
		_, err := store.CreateRepo(t.Context(), reposstore.CreateRepoParams{
			Name:          name,
			UpstreamURL:   "https://example.com/" + name + ".git",
			ForgeHost:     "example.com",
			IndexedBranch: "main",
		})
		require.NoError(t, err)
	}
	var lister RepoLister = NewStoreRepoLister(store)
	repos, err := lister.ListRepos(t.Context())
	require.NoError(t, err)
	var got []RepoID
	for _, repo := range repos {
		if repo == "group/producer-a" || repo == "group/producer-b" || repo == "group/producer-c" {
			got = append(got, repo)
		}
	}
	require.Equal(t, []RepoID{"group/producer-a", "group/producer-b", "group/producer-c"}, got,
		"every repo just enrolled must come back as a RepoID equal to its name (never its id), ordered by name")
}
