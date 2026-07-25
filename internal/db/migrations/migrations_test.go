package migrations

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// migrationFileName matches the NNNN_name.up.sql / NNNN_name.down.sql
// convention: a numeric version, an underscore, a name, then .up.sql or
// .down.sql.
var migrationFileName = regexp.MustCompile(`^([0-9]+)_(.+)\.(up|down)\.sql$`)

// TestEmbeddedMigrationsAreWellFormed walks the actual embed.FS the
// production code embeds (not a fixture copy) and asserts every entry
// follows the NNNN_name.up.sql / NNNN_name.down.sql convention and that
// every .up.sql has a matching .down.sql with the same version and name,
// and vice versa. This fails if a migration is added without its pair --
// exactly the property the acceptance criterion names.
func TestEmbeddedMigrationsAreWellFormed(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(migrationFiles, migrationsDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "expected at least the bootstrap migration to be embedded")
	ups := make(map[string]bool)
	downs := make(map[string]bool)
	for _, e := range entries {
		require.Falsef(t, e.IsDir(), "unexpected subdirectory %q under %s", e.Name(), migrationsDir)
		match := migrationFileName.FindStringSubmatch(e.Name())
		require.Truef(t, match != nil, "file %q does not follow the NNNN_name.up.sql/.down.sql convention", e.Name())
		key := match[1] + "_" + match[2]
		if match[3] == "up" {
			ups[key] = true
			continue
		}
		downs[key] = true
	}
	for key := range ups {
		assert.Truef(t, downs[key], "migration %s.up.sql has no matching %s.down.sql", key, key)
	}
	for key := range downs {
		assert.Truef(t, ups[key], "migration %s.down.sql has no matching %s.up.sql", key, key)
	}
}

// TestEmbeddedMigrationsAreNotEmpty guards against an .up.sql/.down.sql pair
// that exists but contains no statement at all -- distinct from the
// deliberately trivial (but non-empty) bootstrap SQL this bead ships.
func TestEmbeddedMigrationsAreNotEmpty(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(migrationFiles, migrationsDir)
	require.NoError(t, err)
	for _, e := range entries {
		contents, err := fs.ReadFile(migrationFiles, migrationsDir+"/"+e.Name())
		require.NoError(t, err)
		assert.NotEmptyf(t, strings.TrimSpace(string(contents)), "migration file %q is empty", e.Name())
	}
}

// TestIofsSourceOpensEmbeddedMigrations exercises the actual iofs wiring
// (not just fs.ReadDir) against the production embed.FS, and asserts the
// bootstrap migration is readable as version 1 through the source.Driver
// interface golang-migrate itself uses.
func TestIofsSourceOpensEmbeddedMigrations(t *testing.T) {
	t.Parallel()
	source, err := iofs.New(migrationFiles, migrationsDir)
	require.NoError(t, err)
	defer source.Close()
	first, err := source.First()
	require.NoError(t, err)
	assert.Equal(t, uint(1), first)
	up, identifier, err := source.ReadUp(first)
	require.NoError(t, err)
	defer up.Close()
	assert.Equal(t, "init", identifier)
	down, identifier, err := source.ReadDown(first)
	require.NoError(t, err)
	defer down.Close()
	assert.Equal(t, "init", identifier)
}

func TestMigrateEmptyDSN(t *testing.T) {
	t.Parallel()
	err := Migrate(t.Context(), "", testLogger())
	require.ErrorIs(t, err, errEmptyDSN)
}

func TestMigrateUnparseableDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "not a dsn", dsn: "not a dsn at all"},
		{name: "malformed scheme", dsn: "://bad"},
		{name: "trailing garbage", dsn: "postgres://localhost/db?sslmode=disable extra garbage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Migrate(t.Context(), tt.dsn, testLogger())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "connecting to migration database")
		})
	}
}

func TestMigrateUnreachableDSN(t *testing.T) {
	t.Parallel()
	err := Migrate(t.Context(), "postgres://localhost:1/nonexistent?sslmode=disable&connect_timeout=1", testLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connecting to migration database")
}

// TestMigrateContextCancellation asserts Migrate honors a context that is
// already canceled before the connection attempt starts, rather than
// silently ignoring ctx after accepting it as a parameter.
func TestMigrateContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := Migrate(ctx, "postgres://localhost:1/nonexistent?sslmode=disable", testLogger())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "expected context.Canceled in error chain, got: %v", err)
}
