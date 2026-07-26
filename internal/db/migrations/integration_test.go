//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./internal/db/migrations/... -run TestMigrateAgainstRealPostgres -v
//
// On podman (e.g. a `podman machine` forwarding /var/run/docker.sock), also
// set TESTCONTAINERS_RYUK_DISABLED=true:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/db/migrations/... -run TestMigrateAgainstRealPostgres -v
//
// Without it the reaper sidecar testcontainers-go starts alongside every
// container fails outright ("unable to find network with name or ID
// bridge: network not found") because podman's Docker-compat API does not
// resolve the reaper's expected `bridge` network the way a real Docker
// daemon does -- so the test never reaches Migrate() at all. This is a
// local convenience only: with ryuk disabled, cleanup relies entirely on
// t.Cleanup(container.Terminate), which does not run on SIGKILL, a CI step
// timeout, or a -timeout panic. Do not disable ryuk in CI without a
// reaper-equivalent sweep (e.g. a scheduled `docker rm` pass) --
// tracked separately.
package migrations

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// expectedTables is the exact metadata table set loam-54o.3's migration must
// create, per docs/persistence-spec.md "Metadata" and this bead's NOTES.
var expectedTables = []string{
	"repos",
	"repo_target_branches",
	"credentials",
	"roles",
	"role_operations",
	"work_branches",
	"review_rounds",
	"verdicts",
	"threads",
	"comments",
	"ingest_jobs",
}

// TestMigrateAgainstRealPostgres is the first real `migrate up` this project
// has ever executed (see loam-54o.3's NOTES): every other test in this
// package exercises the embed layer or a pre-connection error path against
// no live database at all. This spins up a real Postgres via
// testcontainers-go, runs Migrate (the actual m.Up() / ErrNoChange /
// m.Version() / WithInstance path), asserts the ten metadata tables and the
// built-in role seed exist, then migrates down and asserts a clean revert.
//
// Uses pgvectorImage (defined in code_intel_integration_test.go), not plain
// postgres:16-alpine: Migrate applies every pending migration in order and
// has no "stop after 0001" mode, so once 0002_code_intel.up.sql exists
// (loam-54o.4) this test's single Migrate call runs it too, and
// CREATE EXTENSION vector fails outright against an image that doesn't ship
// the extension. This is a test-fixture change only -- the assertions below
// (tables, role seed, forbidden columns, unique constraints) are untouched,
// and 0001_init.up.sql itself is not modified.
func TestMigrateAgainstRealPostgres(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, pgvectorImage,
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

	// First apply: exercises m.Up() actually running SQL, not ErrNoChange.
	require.NoError(t, Migrate(ctx, dsn, logger))
	assertTablesExist(ctx, t, dsn)
	assertBuiltinRolesSeeded(ctx, t, dsn)
	assertForbiddenColumnsAbsent(ctx, t, dsn)
	assertUniqueConstraintsEnforced(ctx, t, dsn)

	// Second apply against an already-migrated database: exercises the
	// ErrNoChange idempotency branch for real.
	require.NoError(t, Migrate(ctx, dsn, logger))

	migrateDown(ctx, t, dsn)
	assertTablesAbsent(ctx, t, dsn)
}

// assertForbiddenColumnsAbsent checks the columns this bead's ACCEPTANCE
// CRITERIA and NOTES explicitly say must NOT exist -- the stale
// pre-correction column names (repos.default_target and version columns,
// ingest_jobs.old_ref/new_ref) and the dropped SSH credential columns --
// plus that verdicts carries no "stale" column (staleness is derived, per
// docs/persistence-spec.md) and no verdicts_live_reviewer partial index.
func assertForbiddenColumnsAbsent(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	forbidden := []struct{ table, column string }{
		{"repos", "default_target"},
		{"repos", "last_indexed_ref"},
		{"repos", "pipeline_version"},
		{"repos", "grammar_version"},
		{"repos", "embed_model"},
		{"repos", "description_schema"},
		{"credentials", "ssh_private_key_ciphertext"},
		{"verdicts", "stale"},
		{"ingest_jobs", "old_ref"},
		{"ingest_jobs", "new_ref"},
	}
	for _, f := range forbidden {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
			f.table, f.column,
		).Scan(&exists))
		assert.Falsef(t, exists, "column %s.%s must not exist per docs/persistence-spec.md / bead NOTES", f.table, f.column)
	}
	var hasCiphertext bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'credentials' AND column_name = 'token_ciphertext')`,
	).Scan(&hasCiphertext))
	assert.True(t, hasCiphertext, "credentials.token_ciphertext must exist (token-only credentials)")
	var partialIndexExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'verdicts_live_reviewer')`,
	).Scan(&partialIndexExists))
	assert.False(t, partialIndexExists, "verdicts_live_reviewer partial index must not exist -- staleness is derived, not indexed")
}

// assertUniqueConstraintsEnforced proves review_rounds UNIQUE(work_branch_id,
// number) and verdicts UNIQUE(round_id, reviewer) are real, enforced
// constraints -- not just documentation -- by inserting a full row chain
// (repo -> work_branch -> review_round -> verdict) and then attempting a
// duplicate insert that must fail with a unique-violation error from
// Postgres itself.
func assertUniqueConstraintsEnforced(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	var repoID, branchID, roundID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES (gen_random_uuid(), 'group/repo', 'https://example.com/repo.git', 'example.com', 'main')
		 RETURNING id`,
	).Scan(&repoID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO work_branches (id, repo_id, name, target, author)
		 VALUES (gen_random_uuid(), $1, 'wb-1', 'main', 'agent-1')
		 RETURNING id`,
		repoID,
	).Scan(&branchID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO review_rounds (id, work_branch_id, number, requested_by)
		 VALUES (gen_random_uuid(), $1, 1, 'agent-1')
		 RETURNING id`,
		branchID,
	).Scan(&roundID))
	_, err = pool.Exec(ctx,
		`INSERT INTO review_rounds (id, work_branch_id, number, requested_by) VALUES (gen_random_uuid(), $1, 1, 'agent-1')`,
		branchID,
	)
	require.Error(t, err, "duplicate (work_branch_id, number) must violate review_rounds' unique constraint")
	assert.Contains(t, err.Error(), "duplicate key value violates unique constraint")

	_, err = pool.Exec(ctx,
		`INSERT INTO verdicts (id, round_id, reviewer, outcome) VALUES (gen_random_uuid(), $1, 'reviewer-1', 'approve')`,
		roundID,
	)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO verdicts (id, round_id, reviewer, outcome) VALUES (gen_random_uuid(), $1, 'reviewer-1', 'disapprove')`,
		roundID,
	)
	require.Error(t, err, "duplicate (round_id, reviewer) must violate verdicts' unique constraint")
	assert.Contains(t, err.Error(), "duplicate key value violates unique constraint")
}

// assertTablesExist queries information_schema directly (independent of the
// migrations package's own bookkeeping) so the assertion proves the schema
// Migrate actually left behind, not just that migrate reported success.
func assertTablesExist(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	for _, table := range expectedTables {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.Truef(t, exists, "expected table %q to exist after migrate up", table)
	}
}

// assertTablesAbsent is the down-migration mirror of assertTablesExist.
func assertTablesAbsent(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	for _, table := range expectedTables {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
			table,
		).Scan(&exists)
		require.NoError(t, err)
		assert.Falsef(t, exists, "expected table %q to be dropped after migrate down", table)
	}
}

// assertBuiltinRolesSeeded checks the author/reviewer built-in role seed and
// its role_operations rows, matching docs/web-spec.md's fixed vocabulary.
func assertBuiltinRolesSeeded(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT name, builtin FROM roles ORDER BY name`)
	require.NoError(t, err)
	type role struct {
		name    string
		builtin bool
	}
	var got []role
	for rows.Next() {
		var r role
		require.NoError(t, rows.Scan(&r.name, &r.builtin))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2)
	assert.Equal(t, "author", got[0].name)
	assert.True(t, got[0].builtin)
	assert.Equal(t, "reviewer", got[1].name)
	assert.True(t, got[1].builtin)

	// Compare the exact operation SETS, not just their cardinality: a seed
	// that swapped one capability for another (e.g. reviewer seeded with
	// git.push instead of git.clone) would still pass a count(*)==6 check --
	// that is precisely the "permission bug shipped as data" this assertion
	// exists to catch. Literals are the sorted vocabulary from
	// docs/web-spec.md's built-in role list (:132-137).
	wantAuthorOps := []string{
		"git.clone", "git.push", "graph.query", "search",
		"work.read", "work.reply", "work.request_review", "work.set", "work.start",
	}
	wantReviewerOps := []string{
		"git.clone", "graph.query", "search", "work.read", "work.reply", "work.verdict",
	}
	assert.Equal(t, wantAuthorOps, roleOperations(ctx, t, pool, "author"))
	assert.Equal(t, wantReviewerOps, roleOperations(ctx, t, pool, "reviewer"))
}

// roleOperations returns the sorted set of operations role_operations grants
// roleName, joined through roles by name.
func roleOperations(ctx context.Context, t *testing.T, pool *pgxpool.Pool, roleName string) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT ro.operation FROM role_operations ro JOIN roles r ON r.id = ro.role_id WHERE r.name = $1 ORDER BY ro.operation`,
		roleName,
	)
	require.NoError(t, err)
	var ops []string
	for rows.Next() {
		var op string
		require.NoError(t, rows.Scan(&op))
		ops = append(ops, op)
	}
	require.NoError(t, rows.Err())
	return ops
}

// migrateDown drives golang-migrate's Down() directly (Migrate only exposes
// Up), through the same iofs source + pgx/v5 WithInstance wiring
// migrations.go's Migrate uses, so the down.sql files run through the real
// harness rather than a hand-rolled SQL exec.
func migrateDown(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	sourceDriver, err := iofs.New(migrationFiles, migrationsDir)
	require.NoError(t, err)
	defer sourceDriver.Close()
	db, err := sql.Open("pgx/v5", dsn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.PingContext(ctx))
	databaseDriver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", sourceDriver, "pgx5", databaseDriver)
	require.NoError(t, err)
	defer func() {
		_, _ = m.Close()
	}()
	err = m.Down()
	if !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err)
	}
}
