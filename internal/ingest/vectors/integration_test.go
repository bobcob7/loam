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
// a rejected file (see Persist's doc comment for why), and the shared
// transaction that rejection poisoned can still fail to commit even
// though IngestFileChunks itself reported no error -- exactly the gap
// TestIngestFileChunks_RejectionInASharedTransactionStillDoomsTheWholeCommit
// exists to document. A caller of this helper must be able to see that
// failure, not have it turn into a test process abort three frames away
// from the assertion that actually cares.
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
// value. The shared transaction is still poisoned by the rejection, so the
// caller's own commit fails regardless (see
// TestIngestFileChunks_RejectionInASharedTransactionStillDoomsTheWholeCommit
// for that in isolation); either way, nothing ends up in the table.
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

	commitErr := tx.Commit(ctx)
	require.Error(t, commitErr, "the rejection still poisoned the shared transaction")
	assert.Empty(t, storedChunks(t, repoID), "a failed dimension check must leave the table exactly as it was")
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

// loam-c94.21's central finding, proved against a REAL server rather than
// assumed from documentation: Persist's own per-file skip-and-continue
// (Stats.FilesRejected, no error returned for a survivable rejection) is
// necessary but not sufficient to save a batch when st is bound to the
// swap orchestrator's shared transaction, exactly how production wires it
// (internal/ingest/orchestrator/production.go's vectorAdapter.Persist).
// Postgres aborts the ENTIRE transaction -- not merely the offending
// statement -- the instant one statement in it errors, with no way back
// short of a SAVEPOINT neither this package nor
// chunkstore.ReplaceFileChunks currently takes. The rejected file here is
// deliberately the LAST one Persist touches, so there is no cascade of
// SUBSEQUENT calls to blame: the commit fails purely because of the one
// rejection, proving the gap is inherent to the shared transaction, not to
// how many files came after it.
func TestIngestFileChunks_RejectionInASharedTransactionStillDoomsTheWholeCommit(t *testing.T) {
	t.Parallel()
	repoID := newIntegrationRepo(t)
	ctx := t.Context()
	tx, err := sharedPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	badContent := string([]byte{0x66, 0x6f, 0x6f, 0xff, 0x62, 0x61, 0x72}) // "foo\xffbar": 0xff alone is not valid UTF-8
	files := []chunker.FileChunks{
		unitsFor("pkg/good/good.go", "func Good() {}"),
		unitsFor("pkg/bad/bad.go", badContent),
	}

	stats, ingestErr := New(testembed.New(), testLogger()).IngestFileChunks(ctx, chunkstore.NewInTx(tx, testLogger()), repoID, testBranch, files)
	require.NoError(t, ingestErr, "Persist's own per-file policy must treat one bad-byte rejection as survivable and return no error on its own, even though the rejected file is last in the batch")
	assert.Equal(t, 1, stats.FilesReplaced, "the good file's own ReplaceFileChunks call must have gone through")
	assert.Equal(t, 1, stats.FilesRejected, "the bad-byte file must be counted as rejected, not silently dropped")

	commitErr := tx.Commit(ctx)
	require.Error(t, commitErr, "the shared transaction is unconditionally poisoned once ANY statement inside it errors -- Persist skipping past the rejection cannot undo that, only a SAVEPOINT around each ReplaceFileChunks call (in chunkstore, outside this package) can")
	assert.Contains(t, commitErr.Error(), "rollback")
	assert.Empty(t, storedChunks(t, repoID), "nothing committed, not even the good file Stats reported as replaced, because the whole shared transaction rolled back")
}

// Decision #3 from loam-c94.21's bead (confirmed from ReplaceFileChunks's
// own doc comment -- "a failure partway through leaves the file's prior
// chunks intact rather than half-replaced" -- and here from a real
// rollback, not merely read): a reparse the store rejects must leave the
// file's PRIOR chunks exactly as they were. Stale-but-present, matching
// docs/ingestion-spec.md's stale-but-consistent rule, never emptied and
// never half-replaced.
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
	badContent := string([]byte{0x62, 0x61, 0x64, 0xff}) // "bad\xff"
	_, ingestErr := New(e, testLogger()).IngestFileChunks(ctx, chunkstore.NewInTx(tx, testLogger()), repoID, testBranch, []chunker.FileChunks{unitsFor(path, badContent)})
	_ = ingestErr
	require.NoError(t, tx.Rollback(ctx), "the caller (the swap orchestrator, loam-c94.12, in production) rolls back on any writeSwap failure -- this test rolls back the same way rather than relying on Commit to fail on its own")

	assert.Equal(t, before, storedChunks(t, repoID, path), "a rejected reparse must leave the file's PRIOR chunks byte-for-byte as they were, never emptied and never left half-replaced")
}

// Guard the seam itself: *chunkstore.Store built over a real transaction
// must satisfy this package's store interface with no adapter, since that
// is exactly how loam-c94.12 will wire it.
var _ store = (*chunkstore.Store)(nil)
