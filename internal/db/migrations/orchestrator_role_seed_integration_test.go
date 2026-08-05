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
	"regexp"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/testdb"
)

// orchestratorOps reads back the sorted operations 0009 granted a role by
// name -- the same join role_instructions_seed_integration_test.go's
// helpers use for the instructions column, applied to role_operations.
func orchestratorOps(ctx context.Context, t *testing.T, db *sql.DB, name string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT ro.operation FROM role_operations ro JOIN roles r ON r.id = ro.role_id WHERE r.name = $1 ORDER BY ro.operation`, name)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var got []string
	for rows.Next() {
		var op string
		require.NoError(t, rows.Scan(&op))
		got = append(got, op)
	}
	require.NoError(t, rows.Err())
	return got
}

// orchestratorRow reads back the whole role row 0009 is responsible for,
// failing the test if it does not exist.
func orchestratorRow(ctx context.Context, t *testing.T, db *sql.DB, name string) (instructions string, builtin bool) {
	t.Helper()
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT instructions, builtin FROM roles WHERE name = $1`, name,
	).Scan(&instructions, &builtin), "expected exactly one roles row named %s", name)
	return instructions, builtin
}

// newMigratedTo8 spins up a fresh Postgres container and applies migrations
// 0001-0008 (everything before this bead's 0009), returning the live
// *migrate.Migrate (for stepping 0009 up or back down) and the *sql.DB
// newMigrator itself returns -- the shared setup every test below starts
// from, so 0009 is always the one migration under test. Deliberately a
// sibling of role_instructions_seed_integration_test.go's newMigratedTo5
// rather than a parameterisation of it: each seed test pins the exact
// version its own migration builds on, and a shared helper taking a step
// count would let one test's version drift silently become another's.
func newMigratedTo8(t *testing.T) (*migrate.Migrate, *sql.DB) {
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
	require.NoError(t, m.Steps(8), "applying migrations 0001-0008")
	return m, db
}

// TestOrchestratorRoleSeedMigration_CreatesBuiltinRoleWithText proves 0009
// creates the role at all: no such row exists at 0008, and after 0009 there
// is exactly one, flagged builtin (which is what makes DeleteRole refuse it
// -- queries/roles.sql's `AND NOT builtin`) and carrying non-empty
// instructions.
func TestOrchestratorRoleSeedMigration_CreatesBuiltinRoleWithText(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo8(t)

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM roles WHERE name = 'orchestrator'`).Scan(&count))
	require.Zero(t, count, "no orchestrator role may exist before 0009 runs")

	require.NoError(t, m.Steps(1), "applying migration 0009")

	instructions, builtin := orchestratorRow(ctx, t, db, "orchestrator")
	assert.True(t, builtin, "the orchestrator role must be builtin, so it cannot be deleted")
	assert.NotEmpty(t, instructions, "0009 must seed default instructions, not leave the empty string 0006's whole existence was about")
}

// TestOrchestratorRoleSeedMigration_GrantsExactlyGraphQueryAndSearch is this
// bead's permission proof, and it is asserted from BOTH directions on
// purpose. The exact-set equality catches a seed that granted too little;
// the explicit per-capability loop catches a seed that granted too much and
// names WHICH capability leaked in the failure message. An orchestrator
// supervises and does not act: it must hold no work-branch capability, and
// not git.clone or git.push either.
//
// The forbidden list below is the fixed vocabulary
// (internal/handler/capability.go) minus the two granted members, written
// out rather than derived, because deriving it from that package would make
// this test agree with a future edit that ADDED a capability to the
// vocabulary and to this role in the same change -- which is exactly the
// silent widening it exists to refuse.
func TestOrchestratorRoleSeedMigration_GrantsExactlyGraphQueryAndSearch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo8(t)
	require.NoError(t, m.Steps(1), "applying migration 0009")

	got := orchestratorOps(ctx, t, db, "orchestrator")
	assert.Equal(t, []string{"graph.query", "search"}, got, "the orchestrator role must grant exactly graph.query and search")

	forbidden := []string{
		"work.start", "work.set", "work.request_review", "work.reply",
		"work.verdict", "work.read", "git.clone", "git.push",
	}
	for _, op := range forbidden {
		assert.NotContainsf(t, got, op, "the orchestrator role must not hold %s: it supervises, the agents act", op)
	}
}

// trackerNames are the issue-tracker names the seeded text must not
// contain. Requirement F: that text ships to every deployment on first
// migration, and most of them will not use this repository's tracker or
// any other.
//
// WORD-BOUNDARY patterns rather than bare substrings, and the boundary is
// load-bearing for exactly the name that matters most here -- "bd", the
// tracker this repository mandates and the one a copy-paste from CLAUDE.md
// would introduce. Two characters is short enough to go wrong in both
// directions. This test first used the needle " bd ", spaces on both
// sides; loam-hi5o.31's round-1 review mutated the seed to "Track the task
// in bd, and file follow-ups with bd." and it PASSED at exit 0, because
// "bd," and "bd." never match " bd ". A bare substring goes wrong the
// other way -- "subdirectory" contains "bd" -- so prose naming no tracker
// at all could fail it. \bbd\b is the form that catches the realistic
// violation without inventing one.
//
// THE LIST CANNOT BE COMPLETE and does not try to be: notion, clickup,
// youtrack, redmine and others are absent. Read it as a guard against the
// likely mistake, not as proof of absence. Adding a name is free; removing
// one is not, and round 2 of this bead's own review is why that warning is
// here rather than in a commit message nobody will read. Round 2 dropped
// "linear.app" on a rationale that did not describe it -- the claim was
// that the needle risked matching ordinary English, which "linear.app"
// cannot, and which was true only of a bare "linear" that had never been
// in the list -- and the review demonstrated the cost by mutating the seed
// to "Record every task at linear.app" and watching this check pass.
//
// It deliberately does NOT contain a generic phrase like "issue tracker".
// The bead requires the seeded text to name no PARTICULAR tracker while
// being "phrased for a task arriving from a tracker, a one-off
// instruction, or a paragraph the operator wrote" -- so the category noun
// is sanctioned prose, and forbidding it would fail the seed for doing the
// thing it was asked to do.
var trackerNames = []*regexp.Regexp{
	regexp.MustCompile(`\bbd\b`),
	regexp.MustCompile(`\bbeads\b`),
	regexp.MustCompile(`\bjira\b`),
	regexp.MustCompile(`\btrello\b`),
	regexp.MustCompile(`\basana\b`),
	regexp.MustCompile(`\blinear\.app\b`),
	regexp.MustCompile(`\b(github|gitlab) issues?\b`),
	regexp.MustCompile(`\bclaude\.md\b`),
}

// TestOrchestratorRoleSeedMigration_SeededTextNamesNoIssueTracker pins
// loam-hi5o.31's most easily-inverted requirement. docs/orchestration.md
// lives in this repository and may name the tracker this repository
// mandates; THIS string ships to every deployment on first migration, and
// most of them will not use that tracker or any other. Getting the two
// backwards would ship one project's tooling choice as every operator's
// default policy.
//
// It also checks the seeded text is not the document: the doc is markdown
// with headings and a table, and is several times longer. A future edit
// that pasted docs/orchestration.md in here would satisfy every other
// assertion in this file.
func TestOrchestratorRoleSeedMigration_SeededTextNamesNoIssueTracker(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo8(t)
	require.NoError(t, m.Steps(1), "applying migration 0009")

	instructions, _ := orchestratorRow(ctx, t, db, "orchestrator")
	lowered := strings.ToLower(instructions)
	for _, tracker := range trackerNames {
		assert.Falsef(t, tracker.MatchString(lowered), "the seeded orchestrator instructions must name no issue tracker, matched %s", tracker)
	}
	assert.NotContains(t, instructions, "\n## ", "the seeded text must be prose for an agent, not docs/orchestration.md pasted in")
	assert.NotContains(t, instructions, "| Hazard |", "the seeded text must be prose for an agent, not docs/orchestration.md pasted in")
	assert.Contains(t, instructions, "docs/orchestration.md", "the seeded text must point at the long form")
	assert.Contains(t, instructions, "work set", "the seeded text must name the work branch's title/description as the specification")
}

// TestOrchestratorRoleSeedMigration_GuardsPreserveAnOperatorsOwnRole is the
// redeploy trap 0006 taught this repository, in the form it takes here.
// 0009 has to be safe against a deployment that ALREADY has something named
// 'orchestrator' -- roles_name_key is UNIQUE, so an unguarded INSERT would
// abort the whole migration -- and it must not adopt an operator's custom
// role by grafting builtin capabilities onto it. This manufactures exactly
// that state (a non-builtin role with its own text and its own single
// operation, created the way CreateRole would) and runs the REAL migration
// file, asserting every field survives untouched.
func TestOrchestratorRoleSeedMigration_GuardsPreserveAnOperatorsOwnRole(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo8(t)

	const operatorText = "Whatever this operator decided an orchestrator should do."
	_, err := db.ExecContext(ctx,
		`INSERT INTO roles (id, name, instructions) VALUES ('019f9c4b-0474-7955-9d2e-4e3c9f7b81a2', 'orchestrator', $1)`, operatorText)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO role_operations (role_id, operation) VALUES ('019f9c4b-0474-7955-9d2e-4e3c9f7b81a2', 'work.read')`)
	require.NoError(t, err)

	require.NoError(t, m.Steps(1), "applying migration 0009 over a pre-existing operator-created role")

	instructions, builtin := orchestratorRow(ctx, t, db, "orchestrator")
	assert.Equal(t, operatorText, instructions, "0009 must not overwrite an operator's own instructions")
	assert.False(t, builtin, "0009 must not promote an operator's custom role to builtin")
	assert.Equal(t, []string{"work.read"}, orchestratorOps(ctx, t, db, "orchestrator"),
		"0009 must not graft its own capabilities onto a role it did not create")
}

// TestOrchestratorRoleSeedMigration_GuardPreservesSeededTextOnRerun is the
// same guard from the other side, and the one that matters on a redeploy of
// a database this migration has ALREADY run against: an operator edits the
// text through the console, the migration is applied again (a restore, a
// re-run, a fresh migrator against a live database), and the edit must
// survive. Stepping 0009 down and back up is how the package's own Down
// machinery exercises that, and it is only meaningful BECAUSE the down file
// is a no-op -- which is exactly why a destructive down would make this
// silently lossy. See 0009_orchestrator_role.down.sql.
func TestOrchestratorRoleSeedMigration_GuardPreservesSeededTextOnRerun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo8(t)
	require.NoError(t, m.Steps(1), "applying migration 0009")

	const operatorText = "This deployment's own orchestration policy, typed in the console."
	tag, err := db.ExecContext(ctx, `UPDATE roles SET instructions = $1 WHERE name = 'orchestrator'`, operatorText)
	require.NoError(t, err)
	rows, err := tag.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	require.NoError(t, m.Steps(-1), "reverting migration 0009")
	require.NoError(t, m.Steps(1), "re-applying migration 0009")

	instructions, _ := orchestratorRow(ctx, t, db, "orchestrator")
	assert.Equal(t, operatorText, instructions, "re-applying 0009 must not replace text an operator wrote after it first ran")
}

// TestOrchestratorRoleSeedMigration_DownIsANoOp proves the down file runs
// and deliberately changes nothing. The bookkeeping-version check is what
// separates "the down file ran and left the data alone" from "the down file
// was never executed at all" -- the data assertions alone cannot tell those
// apart.
func TestOrchestratorRoleSeedMigration_DownIsANoOp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m, db := newMigratedTo8(t)
	require.NoError(t, m.Steps(1), "applying migration 0009")
	version, dirty, err := m.Version()
	require.NoError(t, err)
	require.EqualValues(t, 9, version, "expected version 9 after applying migration 0009")
	require.False(t, dirty)
	instructions, _ := orchestratorRow(ctx, t, db, "orchestrator")
	require.NotEmpty(t, instructions)

	require.NoError(t, m.Steps(-1), "reverting migration 0009")
	version, dirty, err = m.Version()
	require.NoError(t, err)
	assert.EqualValues(t, 8, version, "reverting migration 0009 must actually run its down file, dropping the bookkeeping version to 8")
	assert.False(t, dirty)

	got, builtin := orchestratorRow(ctx, t, db, "orchestrator")
	assert.Equal(t, instructions, got, "reverting 0009 must not delete the role or blank its instructions")
	assert.True(t, builtin)
	assert.Equal(t, []string{"graph.query", "search"}, orchestratorOps(ctx, t, db, "orchestrator"), "reverting 0009 must not revoke the role's operations")
}
