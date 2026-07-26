//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon. Run with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/storetx/... -v
//
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround, not a CI
// setting). One shared pgvector container for the whole binary, started in
// TestMain per this wave's container-discipline rule -- not one per test.
package storetx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testembed"
)

// sharedDSN is the one migrated pgvector-enabled Postgres this whole test
// binary uses, started once in TestMain. Two independent *pgxpool.Pool
// values are opened against it in the test below -- one to hold the writer
// transaction, one purely as the concurrent reader -- so isolation between
// them comes from being genuinely separate connections, not from any
// container-per-test split.
var sharedDSN string

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

// unitVector returns a testembed.Dimension-wide vector, all zero except
// index i set to 1 -- a valid vector(768) value, sized off testembed's own
// constant rather than a bare literal, matching internal/chunkstore's own
// integration test convention.
func unitVector(i int) []float32 {
	v := make([]float32, testembed.Dimension)
	v[i] = 1
	return v
}

// insertRepo seeds a minimal repos row directly, autocommitted before the
// transaction under test ever opens.
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

// readSymbolName reads symbols.name for the single seeded (repo, branch,
// file) row via pool -- a connection wholly separate from the writer
// transaction under test.
func readSymbolName(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) string {
	t.Helper()
	var name string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT name FROM symbols WHERE repo_id = $1 AND target_branch = 'main' AND file = 'a.go'`, repoID,
	).Scan(&name))
	return name
}

// readChunkContent reads chunks.content for the single seeded (repo,
// branch, file) row via pool.
func readChunkContent(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) string {
	t.Helper()
	var content string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT content FROM chunks WHERE repo_id = $1 AND target_branch = 'main' AND file = 'a.go'`, repoID,
	).Scan(&content))
	return content
}

// TestMultiStoreWriteInOneTransaction_InvisibleUntilCommit is loam-2ph's
// acceptance test: internal/codegraph, internal/chunkstore, and
// internal/reposstore are all bound to the SAME pgx.Tx via NewInTx/
// NewStoreInTx, write inside it, and a concurrent reader on an entirely
// separate pool/connection must see the PRIOR state while that transaction
// is open and the NEW state only once it commits. This is the exact
// property loam-c94.12's one-transaction atomic swap needs -- "readers keep
// serving the prior index until commit" -- proved across real stores over a
// real Postgres, not asserted about the constructors in the abstract.
func TestMultiStoreWriteInOneTransaction_InvisibleUntilCommit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	writerPool, err := pgxpool.New(ctx, sharedDSN)
	require.NoError(t, err)
	t.Cleanup(writerPool.Close)
	readerPool, err := pgxpool.New(ctx, sharedDSN)
	require.NoError(t, err)
	t.Cleanup(readerPool.Close)

	repoID := insertRepo(ctx, t, writerPool, "group/storetx-"+uuid.Must(uuid.NewV7()).String())

	// Seed the "prior index" state, autocommitted on writerPool before the
	// swap transaction opens -- this is what the reader must keep seeing
	// until commit.
	seedCodegraph := codegraph.New(gen.New(writerPool), logger)
	_, err = seedCodegraph.ReplaceFileSymbols(ctx, repoID, "main", "a.go", []codegraph.SymbolInput{
		{Name: "Old", Kind: "function"},
	})
	require.NoError(t, err)
	seedChunks := chunkstore.New(writerPool, logger)
	_, err = seedChunks.ReplaceFileChunks(ctx, repoID, "main", "a.go", []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "old content", Embedding: unitVector(0)},
	})
	require.NoError(t, err)
	seedRepos := reposstore.NewStore(gen.New(writerPool), logger)
	_, err = seedRepos.AddTargetBranch(ctx, repoID, "main")
	require.NoError(t, err)
	priorRef, err := seedRepos.IngestedRef(ctx, repoID, "main")
	require.NoError(t, err)
	require.False(t, priorRef.Ok, "a freshly enrolled target branch must report no ingested ref yet")

	// The swap: one transaction, three stores bound to it via NewInTx/
	// NewStoreInTx, none of which may open a nested transaction of its own.
	tx, err := writerPool.Begin(ctx)
	require.NoError(t, err)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	})

	txCodegraph := codegraph.NewInTx(tx, logger)
	txChunks := chunkstore.NewInTx(tx, logger)
	txRepos := reposstore.NewStoreInTx(tx, logger)

	_, err = txCodegraph.ReplaceFileSymbols(ctx, repoID, "main", "a.go", []codegraph.SymbolInput{
		{Name: "New", Kind: "function"},
	})
	require.NoError(t, err)
	_, err = txChunks.ReplaceFileChunks(ctx, repoID, "main", "a.go", []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "new content", Embedding: unitVector(1)},
	})
	require.NoError(t, err)
	_, err = txRepos.AdvanceIngestedRef(ctx, repoID, "main", "new-ref-sha", time.Now().UTC(), []byte(`{}`))
	require.NoError(t, err)

	// Mid-transaction: the reader, on a wholly separate pool/connection,
	// must still see the PRIOR state for all three stores' writes -- the
	// half-built-index defect this bead exists to close.
	assert.Equal(t, "Old", readSymbolName(ctx, t, readerPool, repoID),
		"a concurrent reader must not see the uncommitted symbol replace")
	assert.Equal(t, "old content", readChunkContent(ctx, t, readerPool, repoID),
		"a concurrent reader must not see the uncommitted chunk replace")
	readerRepos := reposstore.NewStore(gen.New(readerPool), logger)
	midRef, err := readerRepos.IngestedRef(ctx, repoID, "main")
	require.NoError(t, err)
	assert.False(t, midRef.Ok, "a concurrent reader must not see the uncommitted ingested_ref advance")

	require.NoError(t, tx.Commit(ctx))
	committed = true

	// Post-commit: the SAME reader connection must now see every store's
	// new state -- proving the three writes landed as one atomic unit, not
	// independently.
	assert.Equal(t, "New", readSymbolName(ctx, t, readerPool, repoID),
		"after commit, the reader must see the new symbol")
	assert.Equal(t, "new content", readChunkContent(ctx, t, readerPool, repoID),
		"after commit, the reader must see the new chunk")
	postRef, err := readerRepos.IngestedRef(ctx, repoID, "main")
	require.NoError(t, err)
	require.True(t, postRef.Ok, "after commit, the reader must see the advanced ingested_ref")
	assert.Equal(t, "new-ref-sha", postRef.Ref)
}
