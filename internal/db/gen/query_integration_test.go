//go:build integration

// Not sqlc-generated -- a hand-written test alongside the generated files in
// this package, proving the sqlc setup (loam-54o.5) actually works against a
// real database, not just that `go tool sqlc generate` exits 0. See
// internal/db/migrations/integration_test.go and code_intel_integration_test.go
// for the podman/ryuk workaround note; it applies here too:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/db/gen/... -v
package gen

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testembed"
)

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// TestGeneratedQueriesAgainstRealPostgres migrates a real pgvector-enabled
// Postgres (0001_init + 0002_code_intel), then drives the actual
// sqlc-generated Queries type end to end: CreateRepo and
// UpdateRepoSyncState (a text+CHECK column, must round-trip as plain Go
// string per sqlc.yaml's overrides comment) against the metadata schema,
// and InsertChunk + SearchChunksByEmbedding (the pgvector.Vector override)
// against the derived schema -- proving the generated code compiles AND
// executes against the schema it was generated from.
func TestGeneratedQueriesAgainstRealPostgres(t *testing.T) {
	t.Parallel()
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
	defer pool.Close()
	q := New(pool)

	repoID := pgUUID(uuid.New())
	repo, err := q.CreateRepo(ctx, CreateRepoParams{
		ID:            repoID,
		Name:          "group/gen-repo",
		UpstreamUrl:   "https://example.com/repo.git",
		ForgeHost:     "example.com",
		IndexedBranch: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, "idle", repo.SyncState, "sync_state must default to 'idle' and surface as a plain Go string")

	updated, err := q.UpdateRepoSyncState(ctx, UpdateRepoSyncStateParams{
		ID:        repoID,
		SyncState: "syncing",
	})
	require.NoError(t, err)
	assert.Equal(t, "syncing", updated.SyncState, "the text+CHECK sync_state column must round-trip as a plain Go string, not a generated enum type")

	near := pgvector.NewVector(unit(0))
	far := pgvector.NewVector(unit(testembed.Dimension - 1))
	_, err = q.InsertChunk(ctx, InsertChunkParams{
		ID:           pgUUID(uuid.New()),
		RepoID:       repoID,
		TargetBranch: "main",
		File:         "near.go",
		StartLine:    1,
		EndLine:      1,
		Content:      "content",
		Embedding:    near,
	})
	require.NoError(t, err, "InsertChunk must accept a pgvector.Vector for the overridden embedding column")
	_, err = q.InsertChunk(ctx, InsertChunkParams{
		ID:           pgUUID(uuid.New()),
		RepoID:       repoID,
		TargetBranch: "main",
		File:         "far.go",
		StartLine:    1,
		EndLine:      1,
		Content:      "content",
		Embedding:    far,
	})
	require.NoError(t, err)

	results, err := q.SearchChunksByEmbedding(ctx, SearchChunksByEmbeddingParams{
		RepoID:       repoID,
		TargetBranch: "main",
		Embedding:    near,
		Limit:        2,
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "near.go", results[0].File, "SearchChunksByEmbedding must return rows nearest-first")
	assert.Equal(t, "far.go", results[1].File)
}

// unit returns a testembed.Dimension-wide vector that is all zero except
// index i set to 1. Sized off testembed.Dimension (not a bare 768 literal)
// so this test tracks the same width internal/testembed and production
// nomic-embed-text use, per 0002_code_intel.up.sql's chunks.embedding
// comment.
func unit(i int) []float32 {
	v := make([]float32, testembed.Dimension)
	v[i] = 1
	return v
}
