//go:build integration

// See internal/db/migrations/integration_test.go's header for the
// podman/ryuk workaround note; it applies equally here. Run explicitly
// with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/ingest/vectors/... -v
package vectors

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
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/testembed"
	"github.com/bobcob7/loam/internal/testfixture"
)

// sharedPool is one migrated pgvector Postgres for the whole test binary,
// built through internal/db.NewPool so pgvector types are registered on
// every connection exactly as production does it (internal/db/pool.go's
// AfterConnect). Mirrors internal/chunkstore's and internal/ingest/graph's
// own shared-container pattern: isolation between tests comes from each
// seeding its own repos row (and, by FK, its own chunks), not from separate
// databases.
var sharedPool *pgxpool.Pool

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
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: dsn}, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "opening shared registered pool:", err)
		os.Exit(1)
	}
	sharedPool = pool
	code := m.Run()
	pool.Close()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared pgvector container:", err)
	}
	os.Exit(code)
}

// newIntegrationRepo seeds a repos row this test alone owns, satisfying
// chunks.repo_id's FK.
func newIntegrationRepo(t *testing.T) uuid.UUID {
	t.Helper()
	repoID := uuid.Must(uuid.NewV7())
	_, err := sharedPool.Exec(t.Context(),
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, 'https://example.com/repo.git', 'example.com', 'main')`,
		repoID, "group/vectors-"+repoID.String(),
	)
	require.NoError(t, err)
	return repoID
}

// storedChunk is one persisted chunks row read back by querying the table
// DIRECTLY, bypassing this package and chunkstore entirely, so every
// assertion below is about what actually landed rather than what
// IngestFileChunks reported in memory.
type storedChunk struct {
	id        uuid.UUID
	file      string
	startLine int
	endLine   int
	content   string
	embedding []float32
}

// storedChunks reads every chunks row for repoID (optionally narrowed to
// one file), ordered deterministically so two runs are directly
// comparable.
func storedChunks(t *testing.T, repoID uuid.UUID, files ...string) []storedChunk {
	t.Helper()
	query := `SELECT id, file, start_line, end_line, content, embedding FROM chunks WHERE repo_id = $1`
	args := []any{repoID}
	if len(files) == 1 {
		query += ` AND file = $2`
		args = append(args, files[0])
	}
	query += ` ORDER BY file, start_line, end_line`
	rows, err := sharedPool.Query(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []storedChunk
	for rows.Next() {
		var c storedChunk
		var vec pgvector.Vector
		require.NoError(t, rows.Scan(&c.id, &c.file, &c.startLine, &c.endLine, &c.content, &vec))
		c.embedding = vec.Slice()
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

// ingestInTx runs one IngestFileChunks call inside a transaction this test
// opens and commits itself -- exactly the arrangement the swap
// orchestrator (loam-c94.12) will use, via chunkstore.NewInTx. commit=false
// rolls back instead, so a test can prove nothing was auto-committed
// underneath.
//
// A Commit failure is returned to the caller rather than asserted away
// here (require.NoError used to sit on this line): since loam-c94.21,
// IngestFileChunks can return a nil error for a batch that still contained
// a rejected file (see Persist's doc comment for why), so a test's ingest
// call and its commit can disagree, and a caller of this helper must be
// able to see that rather than have it turn into a test process abort
// three frames away from the assertion that actually cares. loam-c94.24's
// savepoints make that commit succeed where it used to fail; the helper
// still returns the error instead of asserting it, because the tests that
// care about the commit outcome now assert it in BOTH directions and each
// one should say so at its own call site.
func ingestInTx(t *testing.T, e embedder, repoID uuid.UUID, files []chunker.FileChunks, commit bool) (Stats, error) {
	t.Helper()
	ctx := t.Context()
	tx, err := sharedPool.Begin(ctx)
	require.NoError(t, err)
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	stats, ingestErr := New(e, testLogger()).IngestFileChunks(ctx, chunkstore.NewInTx(tx, testLogger()), repoID, testBranch, files)
	if ingestErr != nil || !commit {
		return stats, ingestErr
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return stats, commitErr
	}
	committed = true
	return stats, nil
}

// realChunks runs the REAL chunker (real Tree-sitter grammars, real
// budget enforcement against testembed's context window) over dir's files,
// so the integration tests below consume genuine chunk units rather than
// hand-written stand-ins.
func realChunks(t *testing.T, dir string, paths ...string) []chunker.FileChunks {
	t.Helper()
	p := parser.NewParser(testLogger())
	t.Cleanup(p.Close)
	files := make([]chunker.FileInput, len(paths))
	for i, rel := range paths {
		content, err := os.ReadFile(filepath.Join(dir, rel))
		require.NoError(t, err)
		files[i] = chunker.FileInput{Path: rel, Content: content}
	}
	out, stats, err := chunker.NewChunker(p, testLogger()).ChunkFiles(t.Context(), files, testembed.New())
	require.NoError(t, err)
	require.Equal(t, len(paths), stats.FilesChunked, "every fixture file must chunk cleanly")
	return out
}

// TestIngestFileChunks_FixturePolyglot_PersistsEveryChunkThroughTheCallersTransaction
// is this bead's central end-to-end proof: real chunks from real grammars,
// embedded by the real deterministic Embedder double, written through
// chunkstore.NewInTx into a transaction this test owns, and read back from
// the real vector(768) column.
func TestIngestFileChunks_FixturePolyglot_PersistsEveryChunkThroughTheCallersTransaction(t *testing.T) {
	t.Parallel()
	repo := testfixture.NewT(t.Context(), t)
	repoID := newIntegrationRepo(t)
	e := testembed.New()
	files := realChunks(t, repo.Dir(), "pkg/validate/validate.go", "src/validate.ts", "scripts/parity.py", "docs/OVERVIEW.md")

	stats, err := ingestInTx(t, e, repoID, files, true)
	require.NoError(t, err)
	require.Equal(t, len(files), stats.FilesReplaced)
	require.Positive(t, stats.ChunksWritten)
	require.Equal(t, 1, stats.EmbedCalls, "the fixture's chunk count is well under maxEmbedBatch, so the whole batch is one request")

	rows := storedChunks(t, repoID)
	require.Len(t, rows, stats.ChunksWritten, "the row count in Postgres must equal what Stats reported")

	// Every persisted row must carry its own file's chunk verbatim and the
	// vector embedded from THAT content -- the pairing an offset bug would
	// silently break, checked here against the real column rather than a
	// mock's recorded arguments. Rows are matched to units by (file, line
	// range) rather than by position, since chunkSymbols emits units in
	// Tree-sitter query-cursor order, which is not guaranteed to be
	// document order (parser.Match's own doc comment).
	type spanKey struct {
		file               string
		startLine, endLine int
	}
	wanted := map[spanKey]string{}
	for _, f := range files {
		for _, u := range f.Units {
			wanted[spanKey{f.Path, u.StartLine, u.EndLine}] = u.Content
		}
	}
	require.Len(t, wanted, stats.ChunksWritten, "the fixture's units must be uniquely identified by (file, line range) for this comparison to be exhaustive")
	for _, row := range rows {
		key := spanKey{row.file, row.startLine, row.endLine}
		content, ok := wanted[key]
		require.Truef(t, ok, "no unit was ever produced for the persisted row %s:%d-%d", row.file, row.startLine, row.endLine)
		delete(wanted, key)
		assert.Equalf(t, content, row.content, "%s:%d-%d content must round-trip byte for byte", row.file, row.startLine, row.endLine)
		require.Len(t, row.embedding, testembed.Dimension, "the stored vector must be the full column width, never truncated or padded")
		want, err := e.Embed(t.Context(), []string{content})
		require.NoError(t, err)
		assert.Equalf(t, want[0], row.embedding, "%s:%d-%d must be stored with the vector embedded from its own content", row.file, row.startLine, row.endLine)
	}
	assert.Empty(t, wanted, "every chunk unit the chunker produced must have landed as a row")
}

// Nothing this package writes may become visible without the CALLER's
// commit: it is handed a transaction, and if it ever opened or committed
// one of its own the atomic swap (loam-c94.12) would be broken.
func TestIngestFileChunks_TransactionRolledBack_LeavesNoRowsBehind(t *testing.T) {
	t.Parallel()
	repo := testfixture.NewT(t.Context(), t)
	repoID := newIntegrationRepo(t)
	files := realChunks(t, repo.Dir(), "pkg/validate/validate.go")

	stats, err := ingestInTx(t, testembed.New(), repoID, files, false)
	require.NoError(t, err)
	require.Positive(t, stats.ChunksWritten, "the call itself must have staged rows, or this test proves nothing")

	assert.Empty(t, storedChunks(t, repoID), "a rolled-back transaction must leave zero chunks: IngestFileChunks must never commit on its own")
}

// Idempotence, the acceptance criterion: re-embedding an UNCHANGED file
// must leave the same chunks, not accumulate a second copy, and the stored
// payload must be byte-identical run to run.
//
// Identity is a different matter and deliberately not asserted here: each
// row gets a fresh uuid.NewV7 and a fresh created_at from
// chunkstore.ReplaceFileChunks, so chunks.id is NOT stable across
// re-ingests of identical content. Anything downstream that needs stable
// chunk identity (loam-li0.8's incremental-equals-full property) must
// compare (file, start_line, end_line, content), which this test proves IS
// stable, not ids.
func TestIngestFileChunks_ReEmbedUnchangedFile_IsIdempotentAndByteIdentical(t *testing.T) {
	t.Parallel()
	repo := testfixture.NewT(t.Context(), t)
	repoID := newIntegrationRepo(t)
	files := realChunks(t, repo.Dir(), "pkg/validate/validate.go")

	first, err := ingestInTx(t, testembed.New(), repoID, files, true)
	require.NoError(t, err)
	before := storedChunks(t, repoID)
	require.Len(t, before, first.ChunksWritten)
	require.NotEmpty(t, before)

	second, err := ingestInTx(t, testembed.New(), repoID, files, true)
	require.NoError(t, err)
	after := storedChunks(t, repoID)

	assert.Equal(t, first.ChunksWritten, second.ChunksWritten)
	require.Len(t, after, len(before), "a second ingest of identical content must REPLACE the file's chunks, not double them")
	for i := range before {
		assert.Equal(t, before[i].file, after[i].file)
		assert.Equal(t, before[i].startLine, after[i].startLine)
		assert.Equal(t, before[i].endLine, after[i].endLine)
		assert.Equal(t, before[i].content, after[i].content, "chunk %d's stored content must be byte-identical across re-embeds", i)
		assert.Equal(t, before[i].embedding, after[i].embedding, "chunk %d's stored vector must be byte-identical across re-embeds", i)
	}
}

// A reparsed file that now yields FEWER chunks must leave none of the old
// surplus behind: this is the delete half of delete-and-replace, proved
// through this package's own call path against the real table.
func TestIngestFileChunks_ReparsedFileWithFewerChunks_DropsTheStaleRows(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	e := testembed.New()
	const path = "pkg/shrink/shrink.go"

	_, err := ingestInTx(t, e, repoID, []chunker.FileChunks{unitsFor(path, "func Alpha() {}", "func Beta() {}", "func Gamma() {}")}, true)
	require.NoError(t, err)
	require.Len(t, storedChunks(t, repoID, path), 3)

	_, err = ingestInTx(t, e, repoID, []chunker.FileChunks{unitsFor(path, "func Delta() {}")}, true)
	require.NoError(t, err)

	rows := storedChunks(t, repoID, path)
	require.Len(t, rows, 1, "exactly the current chunk set must remain -- nothing stale, nothing accumulated")
	assert.Equal(t, "func Delta() {}", rows[0].content)
}

// The zero-unit case end to end: a reparsed file that chunks to nothing
// must have ALL its prior chunks dropped, not left searchable against
// content that no longer exists.
func TestIngestFileChunks_FileChunkedToZeroUnits_DropsAllItsPriorChunks(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	e := testembed.New()
	const path = "pkg/emptied/emptied.go"

	_, err := ingestInTx(t, e, repoID, []chunker.FileChunks{unitsFor(path, "func Alpha() {}", "func Beta() {}")}, true)
	require.NoError(t, err)
	require.Len(t, storedChunks(t, repoID, path), 2)

	stats, err := ingestInTx(t, e, repoID, []chunker.FileChunks{{Path: path}}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesWithoutChunks)
	assert.Zero(t, stats.EmbedCalls, "a batch with no chunk text must cost no embed request")

	assert.Empty(t, storedChunks(t, repoID, path), "a file that now chunks to nothing must keep no rows at all")
}

// Per-file replace must be exactly that: re-ingesting one file must not
// disturb another file's rows, ids included. This is the difference between
// an incremental re-embed and an accidental full rebuild.
func TestIngestFileChunks_ReplacingOneFile_LeavesOtherFilesRowsUntouched(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	e := testembed.New()
	const changed = "pkg/changed/changed.go"
	const untouched = "pkg/stable/stable.go"

	_, err := ingestInTx(t, e, repoID, []chunker.FileChunks{
		unitsFor(changed, "func Old() {}"),
		unitsFor(untouched, "func Stable() {}"),
	}, true)
	require.NoError(t, err)
	stableBefore := storedChunks(t, repoID, untouched)
	require.Len(t, stableBefore, 1)

	_, err = ingestInTx(t, e, repoID, []chunker.FileChunks{unitsFor(changed, "func New() {}")}, true)
	require.NoError(t, err)

	changedRows := storedChunks(t, repoID, changed)
	require.Len(t, changedRows, 1)
	assert.Equal(t, "func New() {}", changedRows[0].content)
	assert.Equal(t, stableBefore, storedChunks(t, repoID, untouched), "the file that was not reparsed must keep its exact rows, same ids included")
}

// The other half of the dimension invariant: this package checks a vector
// against the Embedder's own Dimension(), and Postgres checks Dimension()
// against the vector(768) chunks.embedding is pinned to. A model whose
// width disagrees with the column must fail loudly at INSERT and leave
// nothing behind -- never a truncated or padded vector that inserts
// cleanly and misranks forever.
// An 8-wide vector against a vector(768) column is, from this package's own
// dimension check's point of view, no different from any other embedder --
// Embed and Dimension() agree with each other, so errDimensionMismatch
// never fires; only Postgres itself, at INSERT, can catch it. That makes it
// a genuine per-file STORE rejection under loam-c94.21's policy (a "type
// error," in the bead's own words, same bucket as a constraint or a size
// limit) rather than a Prepare-time abort, so IngestFileChunks now reports
// it as Stats.FilesRejected and returns no error of its own -- Postgres's
// dimension complaint survives in the ERROR log instead of the return
// value. Since loam-c94.24 the shared transaction survives that rejection
// (the savepoint unwinds the one file), so the assertion that matters here
// is the stronger one: the commit succeeds AND the table is still empty --
// a rejected width must not be able to reach the column by way of a
// transaction that no longer aborts. See
// TestIngestFileChunks_RejectionInASharedTransactionSparesTheRestOfTheBatch
// for the survivability itself in isolation.
func TestIngestFileChunks_EmbedderWidthDisagreesWithColumn_FailsLoudlyAndWritesNothing(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	narrow := &embedderMock{
		DimensionFunc: func() int { return 8 },
		EmbedFunc: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range out {
				out[i] = make([]float32, 8)
			}
			return out, nil
		},
	}
	logger, records := newCapturingLogger()
	ctx := t.Context()
	tx, err := sharedPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	stats, ingestErr := New(narrow, logger).IngestFileChunks(ctx, chunkstore.NewInTx(tx, testLogger()), repoID, testBranch,
		[]chunker.FileChunks{unitsFor("pkg/narrow/narrow.go", "func Alpha() {}")})
	require.NoError(t, ingestErr, "a per-file store rejection must not abort IngestFileChunks on its own")
	assert.Equal(t, 1, stats.FilesRejected, "an 8-wide vector against a 768-wide column must never reach the table silently -- it must be counted as rejected")

	var rejectionLog *slog.Record
	for i, r := range *records {
		if r.Level == slog.LevelError {
			rejectionLog = &(*records)[i]
		}
	}
	require.NotNil(t, rejectionLog, "the rejection must be logged, not only counted")
	assert.Contains(t, recordAttr(*rejectionLog, "error"), "768", "Postgres's own dimension complaint must survive to the log")

	require.NoError(t, tx.Commit(ctx), "since loam-c94.24 the savepoint unwinds the rejected file alone, so the shared transaction still commits")
	assert.Empty(t, storedChunks(t, repoID), "a rejected width must leave the table exactly as it was -- the commit succeeding must not mean a half-written row got through")
}

// The last acceptance criterion: rows this package writes must be
// queryable by the cosine search the RAG path actually runs
// (chunkstore.Search -> the chunks_embedding HNSW index), with the chunk
// whose text matches the query ranking first.
func TestIngestFileChunks_PersistedChunks_AreSearchableByCosineNearestNeighbour(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	e := testembed.New()
	const authText = "func Authenticate(token string) bool { return validate(token) }"
	const reportText = "func Summarize(rows []Row) Report { return render(rows) }"
	require.Empty(t, testembed.CollidingTokens("authenticate token validate", authText, reportText),
		"the co-ranked vocabulary must be collision-free at the real dimension, or the ranking below means nothing")

	_, err := ingestInTx(t, e, repoID, []chunker.FileChunks{
		unitsFor("pkg/auth/auth.go", authText),
		unitsFor("pkg/report/report.go", reportText),
	}, true)
	require.NoError(t, err)

	query, err := e.Embed(t.Context(), []string{"authenticate token validate"})
	require.NoError(t, err)
	found, err := chunkstore.New(sharedPool, testLogger()).Search(t.Context(), []uuid.UUID{repoID}, testBranch, query[0], 2)
	require.NoError(t, err)

	require.Len(t, found, 2, "both persisted chunks must be reachable by the vector search")
	assert.Equal(t, "pkg/auth/auth.go", found[0].File, "the chunk sharing the query's tokens must rank first")
	assert.Equal(t, authText, found[0].Content)
	assert.Equal(t, "pkg/report/report.go", found[1].File)
}

// A batch large enough to span several Embed requests must land completely
// and in the right order against the real table -- the batching arithmetic
// checked end to end, not only against a mock.
func TestIngestFileChunks_BatchLargerThanMaxEmbedBatch_PersistsEveryChunkWithItsOwnVector(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	e := testembed.New()
	const path = "pkg/big/big.go"
	file := syntheticUnits(t, path, maxEmbedBatch+3)

	stats, err := ingestInTx(t, e, repoID, []chunker.FileChunks{file}, true)
	require.NoError(t, err)
	require.Equal(t, 2, stats.EmbedCalls)
	require.Equal(t, maxEmbedBatch+3, stats.ChunksWritten)

	rows := storedChunks(t, repoID, path)
	require.Len(t, rows, maxEmbedBatch+3)
	for i, row := range rows {
		unit := file.Units[i]
		require.Equal(t, unit.Content, row.content, "row %d must hold unit %d's content", i, i)
		want, err := e.Embed(t.Context(), []string{unit.Content})
		require.NoError(t, err)
		assert.Equal(t, want[0], row.embedding, "chunk %d, which crossed a request boundary at %d, must keep its own vector", i, maxEmbedBatch)
	}
}

// This test is the INVERSE of the one loam-c94.21 left here, deliberately
// kept as the same scenario with the opposite expected outcome rather than
// deleted. It used to be named
// ...RejectionInASharedTransactionStillDoomsTheWholeCommit and asserted
// require.Error on the commit plus an empty table: at that point Persist's
// per-file skip-and-continue kept the loop running while Postgres had
// already aborted the whole transaction, so the good file Stats reported
// as replaced was discarded along with the bad one. loam-c94.24 put a
// SAVEPOINT around each ReplaceFileChunks call (chunkstore's
// savepointTransactor), which is what changes the outcome; nothing in THIS
// package changed. Keeping the scenario and flipping the assertion is what
// makes the test still fail if those savepoints are ever removed.
//
// It is a four-file batch with the rejection in the MIDDLE, not the
// two-file batch with the rejection last that the old version used. Last
// was the right shape for the old claim (nothing after it to blame for the
// poisoning); it is the wrong shape for this one, which has to show both
// that a file written BEFORE the rejection survives and that a file
// written AFTER it is still writable at all.
//
// Every file's content is distinct, and the assertion is on the exact
// file->content map read back after commit. Identical content, or a row
// count, would not distinguish "the right files landed" from "some files
// landed".
func TestIngestFileChunks_RejectionInASharedTransactionSparesTheRestOfTheBatch(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	ctx := t.Context()
	tx, err := sharedPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	badContent := string([]byte{0x66, 0x6f, 0x6f, 0xff, 0x62, 0x61, 0x72}) // "foo\xffbar": 0xff alone is not valid UTF-8
	files := []chunker.FileChunks{
		unitsFor("pkg/first/first.go", "func First() {}"),
		unitsFor("pkg/bad/bad.go", badContent),
		unitsFor("pkg/second/second.go", "func Second() {}"),
		unitsFor("pkg/third/third.go", "func Third() {}"),
	}

	stats, ingestErr := New(testembed.New(), testLogger()).IngestFileChunks(ctx, chunkstore.NewInTx(tx, testLogger()), repoID, testBranch, files)
	require.NoError(t, ingestErr, "Persist's own per-file policy must treat one bad-byte rejection as survivable and return no error on its own")
	assert.Equal(t, 3, stats.FilesReplaced, "every file but the rejected one must have gone through")
	assert.Equal(t, 1, stats.FilesRejected, "the bad-byte file must be counted as rejected, not silently dropped")

	require.NoError(t, tx.Commit(ctx), "the savepoint confines the rejection to its own file, so the shared transaction is still committable -- before loam-c94.24 this failed with \"commit unexpectedly resulted in rollback\"")

	// map[string][]string, not map[string]string, and deliberately so: a
	// map keyed by file with a single content value collapses N copies of a
	// row into one entry, so a botched unwind that kept the INSERTs and
	// rolled back only the DELETE would still satisfy it. Keeping the
	// per-file SLICE makes row cardinality visible, matching the shape
	// chunkstore's sibling fixture (chunkContents) already uses for the same
	// reason -- two fixtures over the same data must not disagree about what
	// they are able to see.
	byFile := map[string][]string{}
	for _, row := range storedChunks(t, repoID) {
		byFile[row.file] = append(byFile[row.file], row.content)
	}
	assert.Equal(t, map[string][]string{
		"pkg/first/first.go":   {"func First() {}"},
		"pkg/second/second.go": {"func Second() {}"},
		"pkg/third/third.go":   {"func Third() {}"},
	}, byFile, "exactly the three good files, one chunk each, with their own contents -- the one written before the rejection and the two written after it -- and nothing at all for the rejected file")
}

// Decision #3 from loam-c94.21's bead, re-verified WITH savepoints in place
// rather than carried over (loam-c94.24 acceptance criterion 3): a reparse
// the store rejects must leave the file's PRIOR chunks exactly as they
// were. Stale-but-present, matching docs/ingestion-spec.md's
// stale-but-consistent rule, never emptied and never half-replaced.
//
// The old version of this test rolled the transaction back, where "prior
// chunks intact" was true for the trivial reason that nothing committed at
// all. This one COMMITS, which is now the production outcome, so the claim
// has real content: ROLLBACK TO SAVEPOINT must undo the DELETE that
// ReplaceFileChunks issues before its inserts, not only the insert that
// failed. If it did not, the file would commit with zero chunks --
// silently unsearchable, strictly worse than stale.
//
// A second, unrelated file rides along and its content is asserted too, so
// the test also shows the commit was a real commit and not a no-op.
func TestIngestFileChunks_RejectedReparse_LeavesTheFilesPriorChunksIntact(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	e := testembed.New()
	const path = "pkg/stale/stale.go"

	_, err := ingestInTx(t, e, repoID, []chunker.FileChunks{unitsFor(path, "func Old() {}")}, true)
	require.NoError(t, err)
	before := storedChunks(t, repoID, path)
	require.Len(t, before, 1)

	ctx := t.Context()
	tx, err := sharedPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	badContent := string([]byte{0x62, 0x61, 0x64, 0xff}) // "bad\xff"
	stats, ingestErr := New(e, testLogger()).IngestFileChunks(ctx, chunkstore.NewInTx(tx, testLogger()), repoID, testBranch, []chunker.FileChunks{
		unitsFor(path, badContent),
		unitsFor("pkg/fresh/fresh.go", "func Fresh() {}"),
	})
	require.NoError(t, ingestErr)
	require.Equal(t, 1, stats.FilesRejected)
	require.NoError(t, tx.Commit(ctx), "the batch commits now -- which is what makes the assertion below say something about the savepoint's unwind rather than about a rollback")

	assert.Equal(t, before, storedChunks(t, repoID, path), "a rejected reparse must leave the file's PRIOR chunks byte-for-byte as they were, never emptied and never left half-replaced")
	fresh := storedChunks(t, repoID, "pkg/fresh/fresh.go")
	require.Len(t, fresh, 1, "the unrelated file in the same batch must have committed, or the assertion above would pass on an empty commit")
	assert.Equal(t, "func Fresh() {}", fresh[0].content)
}

// Guard the seam itself: *chunkstore.Store built over a real transaction
// must satisfy this package's store interface with no adapter, since that
// is exactly how loam-c94.12 will wire it.
var _ store = (*chunkstore.Store)(nil)
