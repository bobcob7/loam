//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag. Run explicitly with:
//
//	go test -tags=integration ./internal/reporemove/... -v
//
// On podman also set TESTCONTAINERS_RYUK_DISABLED=true (see
// internal/db/migrations/integration_test.go for why). Uses the
// pgvector/pgvector:pg16 image because migrations.Migrate applies
// 0002_code_intel, which runs `CREATE EXTENSION IF NOT EXISTS vector`.
//
// A unit test cannot prove any of this. The whole database half of
// unenrollment is a single DELETE that relies on the schema's ON DELETE
// CASCADE chain, so a mock querier would only ever prove that Go called
// DeleteRepo -- exactly the vacuous "no error was returned" shape this
// operation cannot afford. These tests assert row counts in all thirteen
// repo-scoped tables against the real migrated schema.
package reporemove

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testembed"
)

// repoScopedTables is every table in the schema that holds data belonging
// to one repo, whether it reaches repos.id directly or through a chain of
// ON DELETE CASCADE foreign keys. seedRepo writes exactly one identified
// row into each and records its id; the removal tests below then assert on
// that row BY ID.
//
// Counting by id rather than by "rows belonging to repo X" is the whole
// point and was arrived at the hard way: an earlier version of this file
// counted the transitively-cascaded tables with a join back to repos
// (verdicts -> review_rounds -> work_branches -> repo_id), which made four
// of these thirteen assertions VACUOUS -- deleting the parent rows made
// the join return zero whether or not the child rows themselves survived,
// so a schema with verdicts' cascade removed passed the test with every
// verdict row still sitting in the table. A direct existence check on a
// known id cannot short-circuit that way: an orphan is exactly what it
// finds.
var repoScopedTables = []struct {
	table string
	// existsSQL counts occurrences of the single row seedRepo wrote into
	// table, given that row's recorded id as $1. It never joins.
	existsSQL string
}{
	{"repos", `SELECT count(*) FROM repos WHERE id = $1`},
	{"repo_target_branches", `SELECT count(*) FROM repo_target_branches WHERE repo_id = $1`},
	{"work_branches", `SELECT count(*) FROM work_branches WHERE id = $1`},
	{"review_rounds", `SELECT count(*) FROM review_rounds WHERE id = $1`},
	{"verdicts", `SELECT count(*) FROM verdicts WHERE id = $1`},
	{"threads", `SELECT count(*) FROM threads WHERE id = $1`},
	{"comments", `SELECT count(*) FROM comments WHERE id = $1`},
	{"ingest_jobs", `SELECT count(*) FROM ingest_jobs WHERE id = $1`},
	{"symbols", `SELECT count(*) FROM symbols WHERE id = $1`},
	{"symbol_references", `SELECT count(*) FROM symbol_references WHERE id = $1`},
	{"graph_edges", `SELECT count(*) FROM graph_edges WHERE id = $1`},
	{"symbol_history", `SELECT count(*) FROM symbol_history WHERE id = $1`},
	{"chunks", `SELECT count(*) FROM chunks WHERE id = $1`},
}

// seeded is one seeded repo: its repos.id, plus the id of the single row
// seedRepo wrote into each table in repoScopedTables, keyed by table name.
type seeded struct {
	repoID uuid.UUID
	rowIDs map[string]uuid.UUID
}

// newHarness migrates a fresh pgvector-enabled Postgres container and
// returns the live pool alongside a Remover wired over the real
// reposstore.Store and a temp LOAM_DATA_DIR.
func newHarness(t *testing.T) (*pgxpool.Pool, *Remover, string) {
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
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.Background()))
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	dataDir := t.TempDir()
	return pool, New(dataDir, reposstore.NewStore(gen.New(pool), logger), logger), dataDir
}

// seedRepo enrols name and writes exactly one identified row into every
// table in repoScopedTables, returning the ids. It asserts every seeded
// row is readable back by the same query the removal assertions use, so a
// schema change that breaks a seed fails here loudly rather than making a
// later "the row is gone" assertion pass vacuously because it was never
// written.
func seedRepo(t *testing.T, pool *pgxpool.Pool, name string) seeded {
	t.Helper()
	ctx := t.Context()
	id := func() uuid.UUID { return uuid.New() }
	repoID, wbID, roundID, verdictID := id(), id(), id(), id()
	threadID, commentID, jobID := id(), id(), id()
	symbolA, symbolB, refID, edgeID, historyID, chunkID := id(), id(), id(), id(), id(), id()
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err, sql)
	}
	exec(`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, $3, $4, 'main')`,
		repoID, name, "https://example.com/"+name+".git", "example.com")
	exec(`INSERT INTO repo_target_branches (repo_id, branch, ingested_ref) VALUES ($1, 'main', 'deadbeef')`, repoID)
	exec(`INSERT INTO work_branches (id, repo_id, name, target, author) VALUES ($1, $2, 'wb-1', 'main', 'agent')`, wbID, repoID)
	exec(`INSERT INTO review_rounds (id, work_branch_id, number, requested_by) VALUES ($1, $2, 1, 'agent')`, roundID, wbID)
	exec(`INSERT INTO verdicts (id, round_id, reviewer, outcome) VALUES ($1, $2, 'reviewer', 'approve')`, verdictID, roundID)
	exec(`INSERT INTO threads (id, work_branch_id, round_id, author) VALUES ($1, $2, $3, 'reviewer')`, threadID, wbID, roundID)
	exec(`INSERT INTO comments (id, thread_id, round_id, author, body) VALUES ($1, $2, $3, 'reviewer', 'looks good')`, commentID, threadID, roundID)
	exec(`INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status) VALUES ($1, $2, 'main', 'full', 'queued')`, jobID, repoID)
	exec(`INSERT INTO symbols (id, repo_id, target_branch, file, line, name, kind) VALUES ($1, $2, 'main', 'a.go', 1, 'A', 'function')`, symbolA, repoID)
	exec(`INSERT INTO symbols (id, repo_id, target_branch, file, line, name, kind) VALUES ($1, $2, 'main', 'b.go', 2, 'B', 'function')`, symbolB, repoID)
	exec(`INSERT INTO symbol_references (id, repo_id, target_branch, file, name, kind, line) VALUES ($1, $2, 'main', 'b.go', 'A', 'function', 5)`, refID, repoID)
	exec(`INSERT INTO graph_edges (id, repo_id, target_branch, from_symbol_id, to_symbol_id, kind) VALUES ($1, $2, 'main', $3, $4, 'dependency')`, edgeID, repoID, symbolB, symbolA)
	exec(`INSERT INTO symbol_history (id, symbol_id, commit, ref, message) VALUES ($1, $2, 'abc123', 'refs/heads/main', 'add A')`, historyID, symbolA)
	exec(`INSERT INTO chunks (id, repo_id, target_branch, file, start_line, end_line, content, embedding) VALUES ($1, $2, 'main', 'a.go', 1, 10, 'func A() {}', $3)`,
		chunkID, repoID, pgvector.NewVector(make([]float32, testembed.Dimension)))
	rec := seeded{repoID: repoID, rowIDs: map[string]uuid.UUID{
		"repos":                repoID,
		"repo_target_branches": repoID,
		"work_branches":        wbID,
		"review_rounds":        roundID,
		"verdicts":             verdictID,
		"threads":              threadID,
		"comments":             commentID,
		"ingest_jobs":          jobID,
		"symbols":              symbolA,
		"symbol_references":    refID,
		"graph_edges":          edgeID,
		"symbol_history":       historyID,
		"chunks":               chunkID,
	}}
	for _, tbl := range repoScopedTables {
		rowID, ok := rec.rowIDs[tbl.table]
		require.True(t, ok, "no seeded row recorded for %s -- its removal assertion would panic or pass vacuously", tbl.table)
		require.Equal(t, 1, countRows(t, pool, tbl.existsSQL, rowID),
			"seed wrote no readable %s row for %s -- the removal assertion on it would be vacuous", tbl.table, name)
	}
	return rec
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, id uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), sql, id).Scan(&n))
	return n
}

// TestDeleteRepo_DropsEveryRepoScopedRowAndOnlyThatReposRows is the
// central destructive-operation test: after removing one of two seeded
// repos, the seeded row in EVERY repo-scoped table must be gone for the
// removed repo and still present for the other. The survivor half is not
// decoration -- it is what separates a correctly scoped cascade from a
// `DELETE FROM symbols` with no WHERE clause, which would satisfy the
// first half perfectly.
func TestDeleteRepo_DropsEveryRepoScopedRowAndOnlyThatReposRows(t *testing.T) {
	t.Parallel()
	pool, remover, _ := newHarness(t)
	removed := seedRepo(t, pool, "acme/widgets")
	kept := seedRepo(t, pool, "acme/keepme")
	require.NoError(t, remover.DeleteRepo(t.Context(), removed.repoID))
	for _, tbl := range repoScopedTables {
		assert.Zero(t, countRows(t, pool, tbl.existsSQL, removed.rowIDs[tbl.table]),
			"%s still holds the removed repo's row -- unenrollment orphaned it", tbl.table)
		assert.Equal(t, 1, countRows(t, pool, tbl.existsSQL, kept.rowIDs[tbl.table]),
			"%s lost a row belonging to a repo that was not removed", tbl.table)
	}
}

// TestDeleteRepo_LeavesCredentialsAlone pins the one repo-adjacent table
// that deliberately does NOT go with a repo: credentials are keyed by
// forge host (credentials_host_key UNIQUE (host)) and shared by every repo
// on that host, so unenrolling one must never revoke the token its
// siblings still authenticate with. There is no FK to cascade through, and
// this test is what would fail if someone added one.
func TestDeleteRepo_LeavesCredentialsAlone(t *testing.T) {
	t.Parallel()
	pool, remover, _ := newHarness(t)
	removed := seedRepo(t, pool, "acme/widgets")
	seedRepo(t, pool, "acme/keepme")
	_, err := pool.Exec(t.Context(),
		`INSERT INTO credentials (id, host, token_ciphertext, validated) VALUES ($1, 'example.com', '\x00'::bytea, true)`, uuid.New())
	require.NoError(t, err)
	require.NoError(t, remover.DeleteRepo(t.Context(), removed.repoID))
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM credentials WHERE host = 'example.com'`).Scan(&n))
	assert.Equal(t, 1, n, "the forge host credential is shared across repos and must survive one repo's removal")
}

// TestDeleteRepo_AlsoRemovesTheMirrorFromDisk covers the half no SQL
// assertion can reach, against the same real store: the bare mirror
// directory the deleted row's name pointed at is gone, and a mirror
// belonging to another enrolled repo is not.
func TestDeleteRepo_AlsoRemovesTheMirrorFromDisk(t *testing.T) {
	t.Parallel()
	pool, remover, dataDir := newHarness(t)
	removed := seedRepo(t, pool, "acme/widgets")
	seedRepo(t, pool, "acme/keepme")
	removedMirror := seedMirrorFor(t, dataDir, "acme/widgets")
	keptMirror := seedMirrorFor(t, dataDir, "acme/keepme")
	require.NoError(t, remover.DeleteRepo(t.Context(), removed.repoID))
	assert.NoDirExists(t, removedMirror)
	assert.DirExists(t, keptMirror)
}

// TestDeleteRepo_UnknownID_ChangesNothing proves an id that is not
// enrolled is an error rather than a silent success, and -- the part that
// matters for a destructive operation -- that it removes nothing at all,
// neither rows nor directories, on its way to that error.
func TestDeleteRepo_UnknownID_ChangesNothing(t *testing.T) {
	t.Parallel()
	pool, remover, dataDir := newHarness(t)
	kept := seedRepo(t, pool, "acme/keepme")
	keptMirror := seedMirrorFor(t, dataDir, "acme/keepme")
	require.ErrorIs(t, remover.DeleteRepo(t.Context(), uuid.New()), reposstore.ErrNotFound)
	for _, tbl := range repoScopedTables {
		assert.Equal(t, 1, countRows(t, pool, tbl.existsSQL, kept.rowIDs[tbl.table]),
			"%s lost a row to a removal of a repo that does not exist", tbl.table)
	}
	assert.DirExists(t, keptMirror)
}

// seedMirrorFor creates a non-empty bare-mirror-shaped directory for name
// under dataDir. It is the integration-side twin of seedMirror (the unit
// test file's helper), spelled separately because it takes the repo name:
// these tests seed two repos at once and need to name each mirror.
func seedMirrorFor(t *testing.T, dataDir, name string) string {
	t.Helper()
	dir := mirrorpath.Dir(dataDir, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte(fmt.Sprintf("ref: refs/heads/main # %s\n", name)), 0o644))
	return dir
}
