//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./internal/db/... -run TestNewPoolAgainstRealPostgres -v
//
// On podman (e.g. a `podman machine` forwarding /var/run/docker.sock), also
// set TESTCONTAINERS_RYUK_DISABLED=true:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/db/... -run TestNewPoolAgainstRealPostgres -v
//
// See internal/db/migrations/integration_test.go for why: without it the
// reaper sidecar testcontainers-go starts alongside every container fails
// outright under podman's Docker-compat API. This is a local convenience
// only -- do not disable ryuk in CI without a reaper-equivalent sweep.
package db

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/migrations"
)

// TestNewPoolAgainstRealPostgres proves the ordering contract this bead
// restores: migrations.Migrate MUST run against the DSN before NewPool is
// called, and once it has, pgvector.RegisterTypes in AfterConnect succeeds
// and a `vector` column round-trips through pgvector-go's Vector type. It
// uses a pgvector-enabled image (pgvector/pgvector:pg16) because a plain
// postgres:16-alpine image has no vector extension to CREATE at all -- per
// this bead's DESIGN note, an image that merely has the extension available
// does not stand in for one where it has been created.
func TestNewPoolAgainstRealPostgres(t *testing.T) {
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

	// This bead's contract: migrations run BEFORE the pool is built, so
	// CREATE EXTENSION vector has already executed by the time NewPool
	// opens any connection. Migrate against a virgin database exercises
	// the 0001 metadata migration on the same first-boot path this bead's
	// ordering fix targets, proving it does not itself need pgvector.
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))

	// The extension-creating migration (0002) is a sibling wave-4 unit's
	// territory (internal/db/migrations, off-limits here) and is not on
	// this branch yet -- per the shared brief, this worktree must not
	// assume its content. CREATE EXTENSION IF NOT EXISTS here stands in
	// for it, isolating what THIS bead owns (NewPool's AfterConnect
	// registration is safe once the extension exists) from whether 0002
	// has landed. It is idempotent, so once 0002 does exist this becomes a
	// harmless no-op rather than a false pass.
	createVectorExtension(ctx, t, dsn)

	pool, err := NewPool(ctx, Config{DatabaseURL: dsn, EncryptionKey: "key"}, logger)
	require.NoError(t, err, "NewPool must succeed once migrations have created the vector extension")
	t.Cleanup(pool.Close)

	assertVectorRoundTrips(ctx, t, pool)
}

// createVectorExtension opens a throwaway connection (deliberately not
// db.NewPool, since the extension must exist BEFORE that is safe to call)
// and issues CREATE EXTENSION IF NOT EXISTS vector.
func createVectorExtension(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	require.NoError(t, err)
}

// assertVectorRoundTrips proves pgvector type registration actually
// happened on the pooled connection -- not just that NewPool returned no
// error -- by writing and reading back a `vector` column through
// pgvector-go's Vector type, which only scans correctly if AfterConnect
// registered the vector codec on that connection's type map.
func assertVectorRoundTrips(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `CREATE TABLE vector_roundtrip (id int PRIMARY KEY, embedding vector(3))`)
	require.NoError(t, err)
	want := pgvector.NewVector([]float32{1.5, -2.25, 3})
	_, err = pool.Exec(ctx, `INSERT INTO vector_roundtrip (id, embedding) VALUES (1, $1)`, want)
	require.NoError(t, err, "insert must succeed through the registered vector codec")
	var got pgvector.Vector
	require.NoError(t, pool.QueryRow(ctx, `SELECT embedding FROM vector_roundtrip WHERE id = 1`).Scan(&got))
	assert.Equal(t, want.Slice(), got.Slice())
}

// TestNewPoolFailsLoudlyWithoutExtension is the negative half of this
// bead's proof: with the extension ABSENT (migrations never run, so
// CREATE EXTENSION vector never executed), NewPool must fail loudly rather
// than silently registering nothing. That loud failure is deliberately
// preserved behavior, per this bead's NOTES: it converts a deployment
// ordering bug into an immediate, legible startup error instead of letting
// it surface later as corrupt vector data. Uses a plain postgres:16-alpine
// image (no pgvector extension available at all) to make the point sharply:
// even if migrations had run, there would be no vector type to create.
func TestNewPoolFailsLoudlyWithoutExtension(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, "postgres:16-alpine",
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

	// Deliberately skip migrations.Migrate: this reproduces the exact
	// first-boot state (extension not created) that deadlocked the server
	// under the old connect-then-migrate order, and proves NewPool still
	// refuses to hand out a usable connection rather than papering over it.
	pool, err := NewPool(ctx, Config{DatabaseURL: dsn, EncryptionKey: "key"}, logger)
	if pool != nil {
		t.Cleanup(pool.Close)
	}
	require.Error(t, err, "NewPool must fail loudly when the vector extension has not been created")
	assert.Contains(t, err.Error(), "pinging database")
}
