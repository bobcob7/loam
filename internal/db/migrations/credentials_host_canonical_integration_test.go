//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; see this
// package's integration_test.go for how to run it and the podman/ryuk
// caveat (TESTCONTAINERS_RYUK_DISABLED=true).
package migrations

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/testdb"
)

// seededCredential is one row this test inserts directly (bypassing
// Migrate/the application entirely) between applying 0004 and 0005, so
// 0005_credentials_host_canonical.up.sql runs against data shaped exactly
// like the live incident loam-0hjq was filed from -- not data this
// package's own migration path could have produced, since SetUpstreamToken
// has canonicalized every host since that bead landed.
type seededCredential struct {
	host      string
	marker    []byte // stand-in for token_ciphertext; a distinctive value proves WHICH row's data survived a collision.
	validated bool
	updatedAt time.Time
}

// insertCredential writes row directly via db (the *sql.DB newMigrator
// itself returns), explicit id/timestamps and all -- exactly the shape
// migrations_integration_test.go's own raw-SQL assertions already use
// elsewhere in this package, and the only way to control updated_at
// precisely enough to test "last write wins".
func insertCredential(ctx context.Context, t *testing.T, db *sql.DB, row seededCredential) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO credentials (id, host, token_ciphertext, validated, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		uuid.New(), row.host, row.marker, row.validated, row.updatedAt,
	)
	require.NoError(t, err, "seeding credentials row for host %s", row.host)
}

// readCredential reads back token_ciphertext/validated for a real host
// string, failing the test if no such row exists.
func readCredential(ctx context.Context, t *testing.T, db *sql.DB, host string) ([]byte, bool) {
	t.Helper()
	var marker []byte
	var validated bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT token_ciphertext, validated FROM credentials WHERE host = $1`, host,
	).Scan(&marker, &validated), "expected exactly one credentials row for host %s", host)
	return marker, validated
}

// credentialRowCount counts every credentials row whose host matches
// likePattern, for asserting a collision pair collapsed to exactly one
// survivor and that an unrelated row was neither duplicated nor dropped.
func credentialRowCount(ctx context.Context, t *testing.T, db *sql.DB, likePattern string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM credentials WHERE host LIKE $1`, likePattern,
	).Scan(&count))
	return count
}

// TestCredentialsHostCanonicalMigration_RewritesAndResolvesCollisions is
// this bead's migration proof: 0005_credentials_host_canonical.up.sql
// against the REAL schema (credentials_host_key's UNIQUE(host) included),
// seeded with data shaped like the live incident this bead was filed from.
//
// It stops the real migrator after 0004 (m.Steps(4)), seeds six rows by
// hand, then applies exactly 0005 (m.Steps(1)) and asserts:
//   - a lone scheme-qualified row is renamed to the bare host, keeping its
//     own ciphertext and validated flag.
//   - a collision pair where the BARE row is newer survives as the bare
//     row's own data (the scheme-qualified duplicate is deleted).
//   - a collision pair where the SCHEME-QUALIFIED row is newer survives
//     with THAT row's data, still keyed at the bare host (proving "last
//     write wins" is symmetric, not "always prefer the bare row").
//   - an already-bare row is untouched.
//   - a scheme-qualified row carrying a path is left alone entirely (not
//     this migration's job -- see the up migration's own comment).
func TestCredentialsHostCanonicalMigration_RewritesAndResolvesCollisions(t *testing.T) {
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
	t.Cleanup(func() { assert.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	m, db, err := newMigrator(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	t.Cleanup(func() { closeMigrator(context.Background(), m, logger) })

	require.NoError(t, m.Steps(4), "applying migrations 0001-0004")

	now := time.Now().UTC().Truncate(time.Microsecond)
	older := now.Add(-1 * time.Hour)
	newer := now

	// Lone scheme-qualified row, no collision.
	insertCredential(ctx, t, db, seededCredential{
		host: "https://forge-only-scheme.example.com", marker: []byte("lone-scheme-marker"), validated: true, updatedAt: now,
	})
	// Collision pair 1: the BARE row is newer -- it must survive.
	insertCredential(ctx, t, db, seededCredential{
		host: "https://forge-collision-1.example.com", marker: []byte("collision1-scheme-STALE"), validated: false, updatedAt: older,
	})
	insertCredential(ctx, t, db, seededCredential{
		host: "forge-collision-1.example.com", marker: []byte("collision1-bare-FRESH"), validated: true, updatedAt: newer,
	})
	// Collision pair 2: the SCHEME-QUALIFIED row is newer -- it must
	// survive, still ending up keyed at the bare host.
	insertCredential(ctx, t, db, seededCredential{
		host: "https://forge-collision-2.example.com", marker: []byte("collision2-scheme-FRESH"), validated: true, updatedAt: newer,
	})
	insertCredential(ctx, t, db, seededCredential{
		host: "forge-collision-2.example.com", marker: []byte("collision2-bare-STALE"), validated: false, updatedAt: older,
	})
	// Already-bare row: untouched by this migration.
	insertCredential(ctx, t, db, seededCredential{
		host: "already-bare.example.com", marker: []byte("already-bare-marker"), validated: true, updatedAt: now,
	})
	// A scheme-qualified row carrying a path: deliberately NOT this
	// migration's job (see the up migration's own comment) -- must be
	// left exactly as seeded.
	insertCredential(ctx, t, db, seededCredential{
		host: "https://forge-with-path.example.com/owner/repo", marker: []byte("path-marker"), validated: true, updatedAt: now,
	})

	require.NoError(t, m.Steps(1), "applying migration 0005")

	marker, validated := readCredential(ctx, t, db, "forge-only-scheme.example.com")
	assert.Equal(t, []byte("lone-scheme-marker"), marker)
	assert.True(t, validated)
	assert.Equal(t, 1, credentialRowCount(ctx, t, db, "%forge-only-scheme.example.com"),
		"the scheme-qualified original must be gone, not merely joined by a new bare row")

	marker, validated = readCredential(ctx, t, db, "forge-collision-1.example.com")
	assert.Equal(t, []byte("collision1-bare-FRESH"), marker, "the NEWER row (the bare one here) must be the survivor")
	assert.True(t, validated)
	assert.Equal(t, 1, credentialRowCount(ctx, t, db, "%forge-collision-1.example.com%"), "exactly one row must remain after the collision resolves")

	marker, validated = readCredential(ctx, t, db, "forge-collision-2.example.com")
	assert.Equal(t, []byte("collision2-scheme-FRESH"), marker, "the NEWER row (the scheme-qualified one here) must be the survivor, even though it is the one being renamed")
	assert.True(t, validated)
	assert.Equal(t, 1, credentialRowCount(ctx, t, db, "%forge-collision-2.example.com%"), "exactly one row must remain after the collision resolves")

	marker, validated = readCredential(ctx, t, db, "already-bare.example.com")
	assert.Equal(t, []byte("already-bare-marker"), marker)
	assert.True(t, validated)

	marker, validated = readCredential(ctx, t, db, "https://forge-with-path.example.com/owner/repo")
	assert.Equal(t, []byte("path-marker"), marker, "a scheme-qualified host carrying a path must be left exactly as seeded")
	assert.True(t, validated)
}
