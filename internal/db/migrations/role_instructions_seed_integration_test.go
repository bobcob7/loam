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

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/testdb"
)

// roleInstructions reads back roles.instructions for a built-in role by
// name, failing the test if no such row exists -- the same "read the real
// column back" shape credentials_host_canonical_integration_test.go's
// readCredential uses.
func roleInstructions(ctx context.Context, t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var instructions string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT instructions FROM roles WHERE name = $1`, name,
	).Scan(&instructions), "expected exactly one roles row named %s", name)
	return instructions
}

// setRoleInstructions writes roles.instructions directly, bypassing
// UpdateRoleInstructions/the application entirely -- the way this test
// manufactures the "an operator already typed real text here" state the
// live deployment was found in, and that 0006's guard exists to protect.
func setRoleInstructions(ctx context.Context, t *testing.T, db *sql.DB, name, instructions string) {
	t.Helper()
	tag, err := db.ExecContext(ctx, `UPDATE roles SET instructions = $1 WHERE name = $2`, instructions, name)
	require.NoError(t, err)
	rows, err := tag.RowsAffected()
	require.NoError(t, err)
	require.EqualValuesf(t, 1, rows, "expected exactly one roles row named %s", name)
}

// newMigratedTo5 spins up a fresh Postgres container and applies migrations
// 0001-0005 (everything before this bead's 0006), returning the live
// *migrate.Migrate (for stepping 0006 up or back down) and the *sql.DB
// newMigrator itself returns (for reading roles.instructions directly) --
// the shared setup every test below starts from, so 0006 is always the one
// migration under test.
func newMigratedTo5(t *testing.T) (*migrate.Migrate, *sql.DB) {
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
	t.Cleanup(func() { assert.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	m, db, err := newMigrator(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	t.Cleanup(func() { closeMigrator(context.Background(), m, logger) })
	require.NoError(t, m.Steps(5), "applying migrations 0001-0005")
	return m, db
}

// TestRoleInstructionsSeedMigration_FillsEmptyBuiltins is this bead's
// principal proof: against a database left exactly as 0001_init seeds it
// (both built-ins at instructions = ”), applying 0006 fills BOTH author
// and reviewer with non-empty text -- acceptance criterion 5's
// "GetInstructions returns non-empty role_instructions for both built-ins"
// starts from this same column, since roleStoreAdapter.RoleInstructions
// (cmd/server/main.go) is a direct passthrough of it.
func TestRoleInstructionsSeedMigration_FillsEmptyBuiltins(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo5(t)

	require.Empty(t, roleInstructions(ctx, t, db, "author"), "0001_init must still seed author with empty instructions before 0006 runs")
	require.Empty(t, roleInstructions(ctx, t, db, "reviewer"), "0001_init must still seed reviewer with empty instructions before 0006 runs")

	require.NoError(t, m.Steps(1), "applying migration 0006")

	author := roleInstructions(ctx, t, db, "author")
	reviewer := roleInstructions(ctx, t, db, "reviewer")
	assert.NotEmpty(t, author, "0006 must fill author's instructions when it was empty")
	assert.NotEmpty(t, reviewer, "0006 must fill reviewer's instructions when it was empty")
	assert.NotEqual(t, author, reviewer, "author and reviewer must get distinct, role-specific text, not one shared string")
}

// TestRoleInstructionsSeedMigration_GuardPreservesNonEmptyText is
// acceptance criterion 2's proof: a deployment that already has non-empty
// text on a built-in (the live incident this bead's own report documents:
// 92 human-typed characters on 'author') must keep that text verbatim
// after 0006 runs, because the migration's UPDATE is guarded by
// `coalesce(instructions, ”) = ”`. This seeds non-empty text directly
// (bypassing the application, exactly like the live row this reproduces)
// and runs the REAL migration file, not a copy of its SQL in isolation.
func TestRoleInstructionsSeedMigration_GuardPreservesNonEmptyText(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo5(t)

	const operatorText = "Your job is to complete a task using these tools and others available to you from the agent."
	setRoleInstructions(ctx, t, db, "author", operatorText)
	require.Empty(t, roleInstructions(ctx, t, db, "reviewer"), "reviewer must still be empty going into 0006, to prove the guard is evaluated per-row, not skipped globally")

	require.NoError(t, m.Steps(1), "applying migration 0006")

	assert.Equal(t, operatorText, roleInstructions(ctx, t, db, "author"), "0006 must NOT overwrite a built-in role's already-non-empty instructions")
	assert.NotEmpty(t, roleInstructions(ctx, t, db, "reviewer"), "0006 must still fill reviewer, which was empty and therefore not protected by the guard")
}

// TestRoleInstructionsSeedMigration_DownIsANoOp is acceptance criterion 3's
// proof: reverting 0006 must not blank instructions back to ” -- that
// would destroy operator text exactly as an unconditional up-migration
// would have. This applies 0006 for real, reverts it with the package's
// own Down machinery (m.Steps(-1), the same *migrate.Migrate the up path
// used), and asserts BOTH built-ins still carry their seeded text
// afterward.
func TestRoleInstructionsSeedMigration_DownIsANoOp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo5(t)
	require.NoError(t, m.Steps(1), "applying migration 0006")

	author := roleInstructions(ctx, t, db, "author")
	reviewer := roleInstructions(ctx, t, db, "reviewer")
	require.NotEmpty(t, author)
	require.NotEmpty(t, reviewer)

	require.NoError(t, m.Steps(-1), "reverting migration 0006")

	assert.Equal(t, author, roleInstructions(ctx, t, db, "author"), "reverting 0006 must not blank author's instructions")
	assert.Equal(t, reviewer, roleInstructions(ctx, t, db, "reviewer"), "reverting 0006 must not blank reviewer's instructions")
}
