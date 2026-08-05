//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/ingest/orchestrator/... -v
//
// These tests are the ones that actually prove this bead's claim. The unit
// tests pin call ORDER against mocks; only these can show what a concurrent
// reader on a separate connection observes while the swap is mid-flight,
// because that property is Postgres MVCC's, not this package's, and a mock
// cannot exhibit it.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/diffplan"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/graph"
	"github.com/bobcob7/loam/internal/ingest/vectors"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testembed"
)

// writerPool is the pool the ingest transaction is opened on; readerPool is
// a SEPARATE pool against the same database, used only to observe. Two
// genuinely distinct pools (not two checkouts of one) is what makes the
// mid-flight reader a real second session rather than something that could
// accidentally share the writer's connection -- and therefore its
// uncommitted snapshot -- and quietly turn the headline test into a
// tautology.
var (
	writerPool *pgxpool.Pool
	readerPool *pgxpool.Pool
)

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
	for _, target := range []**pgxpool.Pool{&writerPool, &readerPool} {
		pool, err := db.NewPool(ctx, db.Config{DatabaseURL: dsn}, logger)
		if err != nil {
			fmt.Fprintln(os.Stderr, "opening registered pool:", err)
			os.Exit(1)
		}
		*target = pool
	}
	code := m.Run()
	writerPool.Close()
	readerPool.Close()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared pgvector container:", err)
	}
	os.Exit(code)
}

// fixture is one test's own repo: a repos row, an enrolled target branch, a
// real bare mirror on disk under a LOAM_DATA_DIR-shaped tree, and the
// working clone commits are made in. Every test seeds its own, so isolation
// comes from distinct repo ids rather than from separate databases.
type fixture struct {
	repoID   uuid.UUID
	repoName string
	dataDir  string
	work     string
	mirror   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repoID := uuid.Must(uuid.NewV7())
	name := "group/orch-" + repoID.String()
	dataDir := t.TempDir()
	f := &fixture{
		repoID:   repoID,
		repoName: name,
		dataDir:  dataDir,
		work:     t.TempDir(),
		mirror:   mirrorpath.Dir(dataDir, name),
	}
	_, err := writerPool.Exec(t.Context(),
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.invalid/r.git', 'example.invalid', 'main')`,
		repoID, name)
	require.NoError(t, err)
	_, err = writerPool.Exec(t.Context(),
		`INSERT INTO repo_target_branches (repo_id, branch) VALUES ($1, 'main')`, repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(f.mirror), 0o755))
	f.git(t, f.work, "init", "--initial-branch=main")
	f.gitAt(t, filepath.Dir(f.mirror), "init", "--bare", "--initial-branch=main", filepath.Base(f.mirror))
	return f
}

// git runs a git command in dir with a hermetic environment.
func (f *fixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return f.gitAt(t, dir, args...)
}

func (f *fixture) gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=loam", "GIT_AUTHOR_EMAIL=loam@example.invalid",
		"GIT_COMMITTER_NAME=loam", "GIT_COMMITTER_EMAIL=loam@example.invalid",
		// GIT_CONFIG_NOSYSTEM plus a redirected HOME *and*
		// XDG_CONFIG_HOME is the full set: without XDG_CONFIG_HOME git
		// still reads ~/.config/git/config from the real home, and
		// without an explicit identity git guesses user@hostname, which
		// works on a laptop and fails on CI.
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+f.work, "XDG_CONFIG_HOME="+f.work,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

// commit writes files (content keyed by path), removes removals, commits,
// and force-pushes main into the bare mirror -- the same shape
// internal/mirrorsync's forced fetch produces, so an unrelated-history
// rewrite is expressible here exactly as a force-push is in production.
func (f *fixture) commit(t *testing.T, msg string, files map[string]string, removals ...string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(f.work, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	for _, name := range removals {
		require.NoError(t, os.Remove(filepath.Join(f.work, name)))
	}
	f.git(t, f.work, "add", "-A")
	f.git(t, f.work, "commit", "--allow-empty", "-m", msg)
	f.git(t, f.work, "push", "--force", f.mirror, "main:main")
}

// newOrchestratorFor builds the FULL production collaborator graph -- the
// real planner, the real git reader, the real Tree-sitter extractor and
// chunker, the real store adapters -- over tx. Only two things are
// substituted: the embedder (internal/testembed, deterministic and
// offline, standing in for a live Ollama) and, in the tests that need to
// observe or break the swap, the transactor.
func newOrchestratorFor(t *testing.T, f *fixture, tx transactor) *Orchestrator {
	t.Helper()
	return newOrchestratorWithEmbedder(t, f, tx, testembed.New())
}

// chunkEmbedder is everything newOrchestratorWithEmbedder needs from an
// embedder: the two methods internal/ingest/vectors consumes, plus the two
// the orchestrator itself does (the chunker's token budget and the model id
// in the recorded version triple). *testembed.Embedder satisfies it, and so
// does anything wrapping one.
type chunkEmbedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimension() int
	ModelID() string
	ContextWindow() int
}

// newOrchestratorWithEmbedder is newOrchestratorFor with the embedder
// substitutable, so a test can make ONE named file's vector unwritable and
// watch what the rest of the swap does about it (loam-c94.24).
func newOrchestratorWithEmbedder(t *testing.T, f *fixture, tx transactor, embedder chunkEmbedder) *Orchestrator {
	t.Helper()
	return newOrchestratorWithLogger(t, f, tx, embedder, testLogger())
}

// newOrchestratorWithLogger is newOrchestratorWithEmbedder with the logger
// substitutable too, so a test can assert on what the pipeline actually
// TOLD an operator rather than only on what it returned (loam-2d44: the
// log line is one of the surfaces a rejection count has to reach, and a
// count that reaches the return value but not the line an operator reads
// is the same gap one layer over).
func newOrchestratorWithLogger(t *testing.T, f *fixture, tx transactor, embedder chunkEmbedder, logger *slog.Logger) *Orchestrator {
	t.Helper()
	parsers := parser.NewParserPool(logger)
	extractor, err := graph.New(parsers, logger)
	require.NoError(t, err)
	t.Cleanup(extractor.Close)
	return newOrchestrator(
		logger,
		f.dataDir,
		diffplan.New(logger),
		reposstore.NewStore(gen.New(writerPool), logger),
		newGitReader(logger),
		graphAdapter{extractor: extractor, logger: logger},
		chunker.NewChunker(parsers, logger),
		vectorAdapter{indexer: vectors.New(embedder, logger), logger: logger},
		storeDropper{logger: logger},
		refAdapter{logger: logger},
		tx,
		embedder,
		diffplan.Versions{Grammar: GrammarVersion, Pipeline: PipelineVersion, EmbeddingModel: embedder.ModelID()},
	)
}

func (f *fixture) job(kind ingest.Kind) ingest.Job {
	return ingest.Job{ID: uuid.Must(uuid.NewV7()), RepoID: f.repoID, TargetBranch: "main", Kind: kind}
}

// --- observations, all made on readerPool, never on the writer ---

func symbolNames(t *testing.T, f *fixture) []string {
	t.Helper()
	rows, err := readerPool.Query(t.Context(),
		`SELECT name FROM symbols WHERE repo_id = $1 AND target_branch = 'main' ORDER BY name`, f.repoID)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

func symbolFiles(t *testing.T, f *fixture) []string {
	t.Helper()
	rows, err := readerPool.Query(t.Context(),
		`SELECT DISTINCT file FROM symbols WHERE repo_id = $1 AND target_branch = 'main' ORDER BY file`, f.repoID)
	require.NoError(t, err)
	defer rows.Close()
	var files []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		files = append(files, s)
	}
	require.NoError(t, rows.Err())
	return files
}

func chunkCountFor(t *testing.T, f *fixture, file string) int {
	t.Helper()
	var n int
	require.NoError(t, readerPool.QueryRow(t.Context(),
		`SELECT count(*) FROM chunks WHERE repo_id = $1 AND target_branch = 'main' AND file = $2`, f.repoID, file).Scan(&n))
	return n
}

func totalChunks(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	require.NoError(t, readerPool.QueryRow(t.Context(),
		`SELECT count(*) FROM chunks WHERE repo_id = $1 AND target_branch = 'main'`, f.repoID).Scan(&n))
	return n
}

func chunkTextMentioning(t *testing.T, f *fixture, needle string) int {
	t.Helper()
	var n int
	require.NoError(t, readerPool.QueryRow(t.Context(),
		`SELECT count(*) FROM chunks WHERE repo_id = $1 AND target_branch = 'main' AND content LIKE '%' || $2 || '%'`,
		f.repoID, needle).Scan(&n))
	return n
}

func edgeCount(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	require.NoError(t, readerPool.QueryRow(t.Context(),
		`SELECT count(*) FROM graph_edges WHERE repo_id = $1 AND target_branch = 'main'`, f.repoID).Scan(&n))
	return n
}

func ingestedRef(t *testing.T, f *fixture) string {
	t.Helper()
	var ref *string
	require.NoError(t, readerPool.QueryRow(t.Context(),
		`SELECT ingested_ref FROM repo_target_branches WHERE repo_id = $1 AND branch = 'main'`, f.repoID).Scan(&ref))
	if ref == nil {
		return ""
	}
	return *ref
}

// --- transactors under test control ---

// pausingTransactor is a real transaction that stops between "every write
// is staged" and COMMIT, so a test can look at the database from another
// session at precisely the instant a half-built index would be visible if
// this bead's claim were false.
type pausingTransactor struct {
	pool    *pgxpool.Pool
	staged  chan struct{}
	release chan struct{}
}

func (t *pausingTransactor) withinTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	close(t.staged)
	<-t.release
	return tx.Commit(ctx)
}

// realTransactor is the production shape, used by the tests that just need
// an ingest to land.
func realTransactor() transactor { return &pgxTransactor{pool: writerPool} }

// --- the tests ---

// TestIngest_MidFlightReaderSeesThePreviousIndexUntilTheSingleCommit is
// this bead's headline claim, demonstrated rather than asserted.
//
// A first ingest builds an index containing Login. A second ingest renames
// Login to Authenticate and is PAUSED after every drop, insert and edge
// recompute has been staged in its transaction but before COMMIT. At that
// instant a genuinely separate session is asked what it sees. It must see
// the OLD index -- Login present, Authenticate absent, ingested_ref still
// the first commit -- because MVCC hides uncommitted writes. Only after
// the commit is released does the same session see the new one.
//
// Every assertion here is on a MONOTONIC transition (old -> new, once),
// not on a value that cycles: nothing in this test runs a background
// worker, the ingest is driven synchronously by this test, and the pause
// is a rendezvous rather than a timeout, so there is no window to lose a
// race in and no sampling to get unlucky with.
func TestIngest_MidFlightReaderSeesThePreviousIndexUntilTheSingleCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "first", map[string]string{
		"auth.go":    "package auth\n\nfunc Login() {}\n",
		"handler.go": "package auth\n\nfunc Serve() { Login() }\n",
	})
	first, err := newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Equal(t, 2, first.FilesParsed)
	require.Contains(t, symbolNames(t, f), "Login")
	firstRef := ingestedRef(t, f)
	require.NotEmpty(t, firstRef)
	firstChunks := totalChunks(t, f)
	require.Positive(t, firstChunks)

	f.commit(t, "rename Login to Authenticate", map[string]string{
		"auth.go": "package auth\n\nfunc Authenticate() {}\n",
	})
	pauser := &pausingTransactor{pool: writerPool, staged: make(chan struct{}), release: make(chan struct{})}
	orch := newOrchestratorFor(t, f, pauser)
	done := make(chan error, 1)
	go func() {
		_, err := orch.Run(t.Context(), f.job(ingest.KindIncremental))
		done <- err
	}()
	select {
	case <-pauser.staged:
	case err := <-done:
		t.Fatalf("the second ingest finished without ever reaching the staged-but-uncommitted point: %v", err)
	}

	assert.Contains(t, symbolNames(t, f), "Login",
		"mid-swap, a reader on another session must still see the PREVIOUS index: the old symbol is still there")
	assert.NotContains(t, symbolNames(t, f), "Authenticate",
		"mid-swap, none of the new index may be visible: the swap is the commit, not the writes")
	assert.Equal(t, firstRef, ingestedRef(t, f),
		"mid-swap, the recorded diff base must still name the commit the VISIBLE index was built from")
	assert.Equal(t, firstChunks, totalChunks(t, f),
		"mid-swap, the previous chunk set must be intact -- neither dropped nor mixed with the new one")
	assert.Positive(t, chunkTextMentioning(t, f, "Login"),
		"mid-swap, RAG search must still match the old content, because that is still the live index")

	close(pauser.release)
	require.NoError(t, <-done)

	assert.Contains(t, symbolNames(t, f), "Authenticate", "after the commit the new index is live")
	assert.NotContains(t, symbolNames(t, f), "Login", "after the commit the old symbol is gone in the same instant the new one appeared")
	assert.NotEqual(t, firstRef, ingestedRef(t, f), "the recorded diff base advances with the index, in the same commit")
	assert.Zero(t, chunkTextMentioning(t, f, "func Login"), "the old content must no longer be searchable once the swap has landed")
	assert.Positive(t, chunkTextMentioning(t, f, "Authenticate"))
}

// TestIngest_FailurePartwayThroughLeavesThePreviousIndexCompletelyIntact
// is the other half of the same property, and the acceptance criterion
// features/ingestion.feature states as "A failed ingest keeps the previous
// index".
//
// The failing ingest is a FULL rebuild, deliberately: that path stages a
// repo-scoped drop of every symbol, reference, edge and chunk for the
// branch before writing anything back, so if the rollback were not doing
// the work, the observable damage would be total rather than subtle. The
// failure is injected at the very last write, with the entire new index
// already staged -- the most demanding position for the rollback, not the
// easiest.
func TestIngest_FailurePartwayThroughLeavesThePreviousIndexCompletelyIntact(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "first", map[string]string{
		"auth.go":    "package auth\n\nfunc Login() {}\n",
		"handler.go": "package auth\n\nfunc Serve() { Login() }\n",
		"README.md":  "# Docs\n\nSome prose about Login.\n",
	})
	_, err := newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	beforeSymbols := symbolNames(t, f)
	beforeFiles := symbolFiles(t, f)
	beforeChunks := totalChunks(t, f)
	beforeEdges := edgeCount(t, f)
	beforeRef := ingestedRef(t, f)
	require.Positive(t, beforeChunks)
	require.Positive(t, beforeEdges, "the fixture must actually produce edges, or this test cannot prove they survive")

	f.commit(t, "second", map[string]string{"auth.go": "package auth\n\nfunc Authenticate() {}\n"})
	boom := errors.New("simulated failure at the last write of the swap")
	orch := newOrchestratorFor(t, f, realTransactor())
	orch.refs = failingRefWriter{err: boom}
	_, err = orch.Run(t.Context(), f.job(ingest.KindFull))
	require.Error(t, err)
	require.ErrorIs(t, err, boom)

	assert.Equal(t, beforeSymbols, symbolNames(t, f), "a failed ingest must leave the previous symbols exactly as they were")
	assert.Equal(t, beforeFiles, symbolFiles(t, f), "no file may lose its rows to a drop that was staged and then rolled back")
	assert.Equal(t, beforeChunks, totalChunks(t, f), "no chunk may be lost: a partial drop is exactly the failure mode this test exists to exclude")
	assert.Equal(t, beforeEdges, edgeCount(t, f), "graph_edges must survive the rolled-back recompute")
	assert.Equal(t, beforeRef, ingestedRef(t, f), "the recorded diff base must not advance for an ingest that never committed")
	assert.Positive(t, chunkTextMentioning(t, f, "Login"), "the previous index must still be searchable after a failed ingest")
}

// failingRefWriter stands in for refAdapter to fail the LAST write in the
// swap, after every index row is already staged.
type failingRefWriter struct{ err error }

func (w failingRefWriter) AdvanceIngestedRef(context.Context, pgx.Tx, uuid.UUID, string, string, time.Time, []byte) error {
	return w.err
}

// TestIngest_FileThatBecomesBinaryLosesItsStaleChunks is loam-8uo's
// end-to-end regression test.
//
// notes.txt is text at the first ingest and binary at the second. Because
// it still EXISTS at the new ref it is in the plan's reparse set, not its
// DropFiles -- so nothing but the chunker emitting a zero-unit entry for
// it can cause its previous chunks to be deleted. Before the fix those
// chunks survived every future ingest and kept matching RAG search for
// content the file no longer had.
func TestIngest_FileThatBecomesBinaryLosesItsStaleChunks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "text", map[string]string{
		"notes.txt": "Meeting notes about the quarterly plan and the rollout schedule.\n",
		"a.go":      "package a\n\nfunc A() {}\n",
	})
	orch := newOrchestratorFor(t, f, realTransactor())
	_, err := orch.Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Positive(t, chunkCountFor(t, f, "notes.txt"), "the text file must be chunked at all, or the regression cannot be observed")
	require.Positive(t, chunkTextMentioning(t, f, "quarterly"))

	f.commit(t, "now binary", map[string]string{"notes.txt": "\x00\x01\x02binary now\x00"})
	_, err = newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.Zero(t, chunkCountFor(t, f, "notes.txt"),
		"a file that turned binary must have ZERO chunks: it is in the reparse set, not DropFiles, so only a zero-unit entry can drop them")
	assert.Zero(t, chunkTextMentioning(t, f, "quarterly"),
		"the stale text must no longer be searchable anywhere in the index")
	assert.Positive(t, chunkCountFor(t, f, "a.go"), "the untouched file's chunks must not be collateral damage")
}

// TestIngest_FullRebuildDropsRowsForFilesNoLongerInTheTree proves why the
// full-rebuild drop has to be repo-scoped rather than a loop over the new
// tree's files. An unrelated-history force-push is the case that creates
// it: git merge-base fails, diffplan escalates to a full rebuild, and
// there is no diff naming the files that disappeared -- so nothing keyed
// on the NEW tree could ever drop gone.go's rows.
func TestIngest_FullRebuildDropsRowsForFilesNoLongerInTheTree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "first", map[string]string{
		"kept.go": "package a\n\nfunc Kept() {}\n",
		"gone.go": "package a\n\nfunc Gone() {}\n",
	})
	_, err := newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Contains(t, symbolNames(t, f), "Gone")
	require.Positive(t, chunkCountFor(t, f, "gone.go"))

	// An unrelated-history rewrite: a fresh root commit force-pushed over
	// main, exactly what an upstream history rewrite looks like to the
	// mirror internal/mirrorsync maintains.
	f.git(t, f.work, "checkout", "--orphan", "rewritten")
	f.git(t, f.work, "rm", "-rf", ".")
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "kept.go"), []byte("package a\n\nfunc Kept() {}\n"), 0o644))
	f.git(t, f.work, "add", "-A")
	f.git(t, f.work, "commit", "-m", "rewritten history")
	f.git(t, f.work, "push", "--force", f.mirror, "rewritten:main")

	stats, err := newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesParsed, "the rewritten tree has exactly one file")
	assert.NotContains(t, symbolNames(t, f), "Gone",
		"a file absent from the rewritten tree must lose its symbols: no per-file loop over the NEW tree could have named it")
	assert.Contains(t, symbolNames(t, f), "Kept")
	assert.Zero(t, chunkCountFor(t, f, "gone.go"), "the vanished file's chunks must go with its symbols")
	assert.Positive(t, chunkCountFor(t, f, "kept.go"))
	assert.Equal(t, []string{"kept.go"}, symbolFiles(t, f))
}

// TestIngest_IncrementalDeleteDropsTheFilesRowsAndRecomputesEdges covers
// the incremental drop path and the edge recompute together: deleting the
// file that DEFINED a symbol must remove both its rows and every edge that
// resolved to it, even though the file that REFERENCES it did not change
// and is therefore not in the reparse set at all.
func TestIngest_IncrementalDeleteDropsTheFilesRowsAndRecomputesEdges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "first", map[string]string{
		"auth.go":    "package auth\n\nfunc Login() {}\n",
		"handler.go": "package auth\n\nfunc Serve() { Login() }\n",
	})
	_, err := newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Positive(t, edgeCount(t, f), "Serve -> Login must resolve into a graph edge for this test to mean anything")
	require.Contains(t, symbolFiles(t, f), "auth.go")

	f.commit(t, "delete auth.go", map[string]string{
		"handler.go": "package auth\n\nfunc Serve() {}\n",
	}, "auth.go")
	_, err = newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.NotContains(t, symbolFiles(t, f), "auth.go", "a deleted file's symbols must be dropped by the plan's DropFiles")
	assert.NotContains(t, symbolNames(t, f), "Login")
	assert.Zero(t, chunkCountFor(t, f, "auth.go"), "a deleted file's chunks must be dropped in the same transaction")
	assert.Zero(t, edgeCount(t, f),
		"graph_edges is recomputed for the whole repo after the drops, so an edge into a deleted symbol cannot survive")
	assert.Contains(t, symbolFiles(t, f), "handler.go")
}

// --- one rejected chunk file does not cost the ingest (loam-c94.24) ---

// nanVectorEmbedder wraps the deterministic test embedder and poisons the
// vector of every chunk whose text contains marker, by setting one
// coordinate to NaN. pgvector rejects NaN at INSERT ("NaN not allowed in
// vector", SQLSTATE 22P02) -- a real per-statement error raised by the
// SERVER, in the same class as a constraint or a type error, which is
// exactly what internal/ingest/vectors.Persist classifies as a per-file
// rejection.
//
// Invalid UTF-8, the shape the original production incident took, can no
// longer be used to provoke this: loam-c94.20 sanitises it in the chunker,
// so it never reaches the store. That is the right fix for that cause, and
// it is why this test needs a different one -- the point here is the
// pipeline's response to ANY store rejection, not to that one.
//
// Dimension() is left alone at testembed's real width, so
// vectors.Prepare's own dimension check passes and the failure genuinely
// happens at the INSERT rather than before it.
type nanVectorEmbedder struct {
	*testembed.Embedder
	marker string
}

func (e nanVectorEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out, err := e.Embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	for i, text := range texts {
		if strings.Contains(text, e.marker) {
			poisoned := append([]float32(nil), out[i]...)
			poisoned[0] = float32(math.NaN())
			out[i] = poisoned
		}
	}
	return out, nil
}

// TestIngest_OneRejectedChunkFile_StillCommitsTheRestOfTheIngest is
// loam-c94.24 end to end, through the production collaborator graph, and
// it is the test that answers the bead's question about the GRAPH track
// rather than reasoning about it.
//
// All three writers share ONE transaction: the graph track's symbols and
// edges, the chunk track's vectors, and AdvanceIngestedRef. Before this
// change, one rejected chunk file aborted that transaction, so the very
// next statement -- AdvanceIngestedRef, which always runs immediately
// after vectors.Persist (writeSwap) -- failed with SQLSTATE 25P02 and the
// whole ingest was lost. Not an edge case reachable only when the bad file
// landed last: in production it was every case.
//
// So the assertions are deliberately spread across all three writers. The
// graph track's rows must be present for EVERY file including the rejected
// one (a chunk rejection says nothing about that file's symbols, and
// ROLLBACK TO SAVEPOINT only unwinds statements issued after its savepoint
// -- the graph writes ran before any of them). The chunk track must have
// rows for the good files and none for the rejected one. And the ingested
// ref must have advanced, which is the single fact that says the
// transaction committed rather than rolled back.
func TestIngest_OneRejectedChunkFile_StillCommitsTheRestOfTheIngest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "poisoned", map[string]string{
		"alpha.go":  "package alpha\n\nfunc AlphaOnly() {}\n",
		"poison.go": "package poison\n\nfunc PoisonedSymbol() {}\n",
		"omega.go":  "package omega\n\nfunc OmegaOnly() { AlphaOnly() }\n",
	})
	embedder := nanVectorEmbedder{Embedder: testembed.New(), marker: "PoisonedSymbol"}

	_, err := newOrchestratorWithEmbedder(t, f, realTransactor(), embedder).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err, "a single rejected chunk file must not fail the job")

	assert.Equal(t, []string{"alpha.go", "omega.go", "poison.go"}, symbolFiles(t, f),
		"the graph track writes before the chunk track and is not savepoint-wrapped, so a chunk rejection must leave every file's symbols -- including the rejected file's own -- committed")
	assert.Contains(t, symbolNames(t, f), "PoisonedSymbol",
		"the rejected file's SYMBOL is unaffected: only its vector was unwritable")
	assert.Positive(t, edgeCount(t, f), "the edge recompute ran after the graph writes and before the rejection, and must have committed with them")

	assert.Positive(t, chunkCountFor(t, f, "alpha.go"), "the chunk file written BEFORE the rejection must survive it")
	assert.Positive(t, chunkCountFor(t, f, "omega.go"), "the chunk file written AFTER the rejection must have been writable at all")
	assert.Zero(t, chunkCountFor(t, f, "poison.go"), "the rejected file must have no chunks -- unsearchable until a later ingest, which is the documented partial-degrade, not a half-written row")
	assert.Zero(t, chunkTextMentioning(t, f, "PoisonedSymbol"), "no partial content from the rejected file may reach the index")

	assert.NotEmpty(t, ingestedRef(t, f), "the ingested ref must have advanced -- this is what proves the shared transaction COMMITTED rather than rolling back the way it did before loam-c94.24")
}
