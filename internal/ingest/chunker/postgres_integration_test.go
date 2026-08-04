//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./internal/ingest/chunker/... -run TestChunkFile_InvalidUTF8LateInFile -v
//
// On podman (e.g. a `podman machine` forwarding /var/run/docker.sock), also
// set TESTCONTAINERS_RYUK_DISABLED=true (see internal/db/migrations's
// integration_test.go for why).
//
// loam-c94.20: production ingest of a large Go repo failed with
//
//	ERROR: invalid byte sequence for encoding "UTF8": 0xa5 (SQLSTATE 22021)
//
// against a large .sql file saved with a stray Mac-Roman/Latin-1 byte.
// isBinary only sniffs the first binarySniffLen bytes for a NUL byte, and
// nothing else in this package validated encoding, so the bad byte reached
// a Postgres text column unexamined. This file proves that end to end
// against a real database: chunk a fixture carrying an actual 0xa5 byte
// placed PAST binarySniffLen (proving the fix, once added, must scan the
// whole file, not just isBinary's prefix), and persist every produced unit
// through the real internal/chunkstore.Store -- the same store the
// production orchestrator uses -- rather than asserting only in-memory
// content.
package chunker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testembed"
)

// sharedChunkerDSN is the one migrated Postgres this file's tests share,
// mirroring internal/chunkstore/integration_test.go's own sharedDSN
// pattern: one container for the package's integration tests, isolation
// between tests coming from each seeding its own repo row rather than from
// separate containers.
var sharedChunkerDSN string

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
	sharedChunkerDSN = dsn
	code := m.Run()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared pgvector container:", err)
	}
	os.Exit(code)
}

// buildLateInvalidUTF8SQLFixture builds a large, otherwise-genuine .sql
// file whose one bad byte -- 0xa5, the exact byte the production error
// named -- sits well past binarySniffLen. isBinary's NUL sniff never sees
// it (there is no NUL anywhere in this fixture), so this is purely an
// encoding problem, not a binary one, and its position is deliberately
// chosen past the same prefix isBinary inspects: a fix that only re-checked
// that prefix would have the identical hole and would still miss this
// fixture.
func buildLateInvalidUTF8SQLFixture(t *testing.T) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("-- legacy.sql: reproduces loam-c94.20's production SQLSTATE 22021\n")
	b.WriteString("SELECT 1;\n")
	for b.Len() < binarySniffLen+500 {
		b.WriteString("-- padding comment line so the bad byte lands past binarySniffLen\n")
	}
	require.Greater(t, b.Len(), binarySniffLen, "fixture must already exceed binarySniffLen before the bad byte is appended")
	b.WriteString("-- old Mac-Roman bullet in a comment: \xa5 (valid Mac Roman/Latin-1, invalid UTF-8)\n")
	b.WriteString("SELECT 2;\n")
	content := []byte(b.String())
	require.NotContains(t, string(content[:binarySniffLen]), "\xa5", "the bad byte must not be visible to isBinary's own prefix sniff")
	return content
}

// insertChunkerTestRepo mirrors internal/chunkstore/integration_test.go's
// own insertRepo: a minimal repos row for the FK chunks.repo_id requires.
func insertChunkerTestRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
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

// TestChunkFile_InvalidUTF8LateInFile_PersistsThroughRealPostgres is
// loam-c94.20's reproduction/regression: chunk buildLateInvalidUTF8SQLFixture
// with the real Chunker (a .sql file has no grammar, so this exercises the
// sliding-window fallback, not the parser), then persist every produced
// unit through internal/chunkstore.Store -- the same store the production
// orchestrator writes chunks through -- against a real Postgres. Before
// loam-c94.20's fix this fails with SQLSTATE 22021 on the unit carrying the
// bad byte; after the fix every unit persists and the file is counted in
// Stats as sanitized.
func TestChunkFile_InvalidUTF8LateInFile_PersistsThroughRealPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	content := buildLateInvalidUTF8SQLFixture(t)
	c := NewChunker(nil, testLogger())
	units, result, ok, err := c.ChunkFile(ctx, "queries/legacy.sql", content, fixedBudgeter(1_000_000))
	require.NoError(t, err)
	require.True(t, ok, "a .sql file with no NUL byte must not be treated as binary")
	require.NotEmpty(t, units, "the sliding-window fallback must still produce units for this fixture")
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: sharedChunkerDSN}, testLogger())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	repoID := insertChunkerTestRepo(ctx, t, pool, "group/loam-c94-20-repro")
	store := chunkstore.New(pool, testLogger())
	inputs := make([]chunkstore.ChunkInput, len(units))
	for i, u := range units {
		inputs[i] = chunkstore.ChunkInput{
			StartLine: u.StartLine,
			EndLine:   u.EndLine,
			Content:   u.Content,
			Embedding: make([]float32, testembed.Dimension),
		}
	}
	_, persistErr := store.ReplaceFileChunks(ctx, repoID, "main", "queries/legacy.sql", inputs)
	if persistErr != nil {
		// Before loam-c94.20's fix, this branch is where the bug reproduced:
		// a *pgconn.PgError with Code "22021" ("invalid byte sequence for
		// encoding \"UTF8\": 0xa5"), the exact production error. Logged
		// rather than silently swallowed, in case this test ever regresses.
		var pgErr *pgconn.PgError
		if errors.As(persistErr, &pgErr) {
			t.Logf("Postgres rejected the persist: SQLSTATE %s: %s", pgErr.Code, pgErr.Message)
		}
	}
	require.NoError(t, persistErr, "every unit, including the one that carried the invalid byte, must persist once sanitized")
	assert.True(t, result.SanitizedInvalidUTF8, "the file must be recorded as sanitized, not silently cleaned")
	var storedContent string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT content FROM chunks WHERE repo_id = $1 AND file = 'queries/legacy.sql' AND content LIKE '%bullet%'`,
		repoID,
	).Scan(&storedContent))
	assert.NotContains(t, storedContent, "\xa5", "the stored content must no longer carry the raw invalid byte")
	assert.Contains(t, storedContent, "�", "the stored content must carry the Unicode replacement character in its place")
}
