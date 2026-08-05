package vectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/ingest/chunker"
)

const (
	testBranch = "main"
	testFileA  = "pkg/a/a.go"
	testFileB  = "pkg/b/b.go"
)

func TestIngestFileChunks_WritesOneReplacePerFileWithItsOwnUnitsAndVectors(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	st, calls := newFakeStore()
	repoID := uuid.Must(uuid.NewV7())
	files := []chunker.FileChunks{
		unitsFor(testFileA, "func Alpha() {}", "func Beta() {}"),
		unitsFor(testFileB, "func Gamma() {}"),
	}

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, repoID, testBranch, files)
	require.NoError(t, err)

	require.Len(t, *calls, 2, "exactly one ReplaceFileChunks call per input file, no more and no fewer")
	assert.Equal(t, testFileA, (*calls)[0].file, "files must be replaced in input order")
	assert.Equal(t, testFileB, (*calls)[1].file)
	assert.Equal(t, []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func Alpha() {}", Embedding: vectorFor("func Alpha() {}")},
		{StartLine: 2, EndLine: 2, Content: "func Beta() {}", Embedding: vectorFor("func Beta() {}")},
	}, (*calls)[0].inputs, "each chunk must carry its own line range, verbatim content, and the vector embedded from THAT content")
	assert.Equal(t, []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "func Gamma() {}", Embedding: vectorFor("func Gamma() {}")},
	}, (*calls)[1].inputs)

	assert.Equal(t, Stats{FilesReplaced: 2, ChunksWritten: 3, EmbedCalls: 1}, stats)
}

func TestIngestFileChunks_PassesRepoIDAndTargetBranchToEveryFile(t *testing.T) {
	t.Parallel()
	st, calls := newFakeStore()
	repoID := uuid.Must(uuid.NewV7())
	files := []chunker.FileChunks{
		unitsFor(testFileA, "one"),
		unitsFor(testFileB, "two"),
	}

	_, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(t.Context(), st, repoID, "release/1.2", files)
	require.NoError(t, err)

	require.Len(t, *calls, 2)
	for _, c := range *calls {
		assert.Equal(t, repoID, c.repoID, "every file's chunks must be scoped to the caller's repo id")
		assert.Equal(t, "release/1.2", c.targetBranch, "every file's chunks must be scoped to the caller's target branch, not a default")
	}
}

// The whole batch's chunk texts must reach the Embedder in one flattened,
// input-ordered request while the batch fits: this is the "embedding is the
// expensive, network-bound step, so batch it" contract, and it is also what
// makes the offset arithmetic in embedAll/IngestFileChunks meaningful.
func TestIngestFileChunks_SendsOneBatchedEmbedCallInFlattenedInputOrder(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	st, _ := newFakeStore()
	files := []chunker.FileChunks{
		unitsFor(testFileA, "alpha", "beta"),
		unitsFor(testFileB, "gamma"),
	}

	_, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)
	require.NoError(t, err)

	require.Len(t, e.EmbedCalls(), 1, "three chunks well under maxEmbedBatch must cost exactly one request, not one per chunk and not one per file")
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, e.EmbedCalls()[0].Texts, "texts must be flattened across files in input order")
}

func TestIngestFileChunks_SplitsEmbedCallsAtMaxEmbedBatch(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	st, calls := newFakeStore()
	file := syntheticUnits(t, testFileA, maxEmbedBatch+1)

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, []chunker.FileChunks{file})
	require.NoError(t, err)

	require.Len(t, e.EmbedCalls(), 2, "maxEmbedBatch+1 chunks must be split into two requests, not sent as one oversized one")
	assert.Len(t, e.EmbedCalls()[0].Texts, maxEmbedBatch, "the first request must be filled to exactly maxEmbedBatch")
	assert.Len(t, e.EmbedCalls()[1].Texts, 1, "the remainder must go in a second request")
	assert.Equal(t, 2, stats.EmbedCalls)

	require.Len(t, *calls, 1)
	inputs := (*calls)[0].inputs
	require.Len(t, inputs, maxEmbedBatch+1)
	last := file.Units[maxEmbedBatch]
	assert.Equal(t, last.Content, inputs[maxEmbedBatch].Content)
	assert.Equal(t, vectorFor(last.Content), inputs[maxEmbedBatch].Embedding,
		"the chunk that landed in the SECOND request must be stored with its own vector -- an offset bug would pair it with a first-request vector")
}

// A batch boundary that falls in the middle of the file list is the case
// most likely to mis-pair a chunk with a neighbour's vector, so it gets its
// own test: file A fills the first request short by one, file B's first
// unit completes it, and file B's second unit starts the next request.
func TestIngestFileChunks_BatchBoundaryInsideFileList_KeepsEveryChunkWithItsOwnVector(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	st, calls := newFakeStore()
	fileA := syntheticUnits(t, testFileA, maxEmbedBatch-1)
	fileB := syntheticUnits(t, testFileB, 2)

	_, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, []chunker.FileChunks{fileA, fileB})
	require.NoError(t, err)

	require.Len(t, e.EmbedCalls(), 2)
	assert.Equal(t, fileB.Units[0].Content, e.EmbedCalls()[0].Texts[maxEmbedBatch-1], "the first request must span the file boundary -- batching is by text count, not by file")

	require.Len(t, *calls, 2)
	for _, c := range *calls {
		for i, in := range c.inputs {
			assert.Equalf(t, vectorFor(in.Content), in.Embedding, "%s chunk %d must be stored with the vector embedded from its own content", c.file, i)
		}
	}
}

// A reparsed file that now chunks to nothing must still be replaced: its
// stale chunks would otherwise stay searchable against content that no
// longer exists.
func TestIngestFileChunks_FileWithNoUnits_StillReplacesToDropStaleChunks(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{{Path: testFileA}})
	require.NoError(t, err)

	require.Len(t, *calls, 1, "a zero-unit file must still reach the store, so its prior chunks are dropped")
	assert.Empty(t, (*calls)[0].inputs, "with no units the call must carry an empty inputs slice: delete, insert nothing")
	assert.Empty(t, e.EmbedCalls(), "an empty batch must not cost an embed request")
	assert.Equal(t, Stats{FilesReplaced: 1, FilesWithoutChunks: 1, ChunksWritten: 0, EmbedCalls: 0}, stats)
}

func TestIngestFileChunks_ZeroUnitFilesInterleaved_DoNotShiftOtherFilesVectors(t *testing.T) {
	t.Parallel()
	st, calls := newFakeStore()
	files := []chunker.FileChunks{
		{Path: "empty/first.go"},
		unitsFor(testFileA, "alpha", "beta"),
		{Path: "empty/second.go"},
		unitsFor(testFileB, "gamma"),
	}

	stats, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)
	require.NoError(t, err)

	require.Len(t, *calls, 4, "every file gets a replace call, zero-unit ones included")
	assert.Empty(t, (*calls)[0].inputs)
	assert.Empty(t, (*calls)[2].inputs)
	assert.Equal(t, vectorFor("alpha"), (*calls)[1].inputs[0].Embedding)
	assert.Equal(t, vectorFor("beta"), (*calls)[1].inputs[1].Embedding)
	assert.Equal(t, vectorFor("gamma"), (*calls)[3].inputs[0].Embedding, "a zero-unit file consumes no vector, so the file after it must not be shifted")
	assert.Equal(t, Stats{FilesReplaced: 4, FilesWithoutChunks: 2, ChunksWritten: 3, EmbedCalls: 1}, stats)
}

func TestIngestFileChunks_EmptyFileBatch_MakesNoCallAndReportsZeroStats(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, nil)
	require.NoError(t, err)

	assert.Empty(t, e.EmbedCalls())
	assert.Empty(t, *calls)
	assert.Equal(t, Stats{}, stats)
}

// A vector narrower than the Embedder's own Dimension() must abort, never
// be zero-padded out to the column width: a padded vector inserts cleanly
// and misranks forever with nothing to notice it.
func TestIngestFileChunks_VectorNarrowerThanDimension_FailsWithoutWriting(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = make([]float32, testDimension-1)
		}
		return out, nil
	}
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha")})

	require.Error(t, err)
	assert.ErrorIs(t, err, errDimensionMismatch)
	assert.Contains(t, err.Error(), "width 3, want 4", "the error must name both widths so an operator can see which side is wrong")
	assert.Empty(t, *calls, "not one chunk may be written once any vector's width is wrong")
	assert.Zero(t, stats.ChunksWritten)
}

// The mirror case: a vector WIDER than Dimension() must abort too, never be
// truncated to fit.
func TestIngestFileChunks_VectorWiderThanDimension_FailsWithoutWriting(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = make([]float32, testDimension+1)
		}
		return out, nil
	}
	st, calls := newFakeStore()

	_, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha")})

	require.Error(t, err)
	assert.ErrorIs(t, err, errDimensionMismatch)
	assert.Contains(t, err.Error(), "width 5, want 4")
	assert.Empty(t, *calls)
}

// A single bad vector anywhere in the batch must abort the whole batch, not
// just skip its own chunk -- the good vectors around it are worthless if
// the offsets after the bad one can no longer be trusted.
func TestIngestFileChunks_OneBadVectorAmongGoodOnes_AbortsTheWholeBatch(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = vectorFor(text)
		}
		out[1] = make([]float32, testDimension+2)
		return out, nil
	}
	st, calls := newFakeStore()

	_, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha", "beta", "gamma")})

	require.Error(t, err)
	assert.ErrorIs(t, err, errDimensionMismatch)
	assert.Contains(t, err.Error(), "vector 1", "the error must name which vector was wrong")
	assert.Empty(t, *calls)
}

// groupByFile is the guard that keeps a chunk-list/vector-list mismatch
// from becoming an index-out-of-range panic partway through the write loop.
// It gets direct tests because it is unreachable through IngestFileChunks
// once embedAll's per-call checks are in place, and a panic here would be
// far worse than an error: it would unwind past the orchestrator's deferred
// rollback rather than failing the ingest cleanly.
func TestGroupByFile_FewerVectorsThanItems_ErrorsInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	items := []flatUnit{
		{fileIdx: 0, unit: chunk.Unit{StartLine: 1, EndLine: 1, Content: "alpha"}},
		{fileIdx: 0, unit: chunk.Unit{StartLine: 2, EndLine: 2, Content: "beta"}},
		{fileIdx: 1, unit: chunk.Unit{StartLine: 1, EndLine: 1, Content: "gamma"}},
	}

	var got [][]chunkstore.ChunkInput
	var err error
	require.NotPanics(t, func() { got, err = groupByFile(2, items, [][]float32{vectorFor("alpha")}) },
		"a short vector list must be rejected up front, never indexed past the end mid-write")

	assert.Nil(t, got)
	require.ErrorIs(t, err, errVectorCount)
	assert.Contains(t, err.Error(), "3 chunks to pair with 1 vectors")
}

func TestGroupByFile_MoreVectorsThanItems_Errors(t *testing.T) {
	t.Parallel()
	items := []flatUnit{{fileIdx: 0, unit: chunk.Unit{StartLine: 1, EndLine: 1, Content: "alpha"}}}

	_, err := groupByFile(1, items, [][]float32{vectorFor("alpha"), vectorFor("beta")})

	assert.ErrorIs(t, err, errVectorCount, "a surplus vector means the alignment is unknowable too, not merely wasteful")
}

func TestGroupByFile_PairsEachFilesUnitsWithItsOwnVectorsInOrder(t *testing.T) {
	t.Parallel()
	items := []flatUnit{
		{fileIdx: 1, unit: chunk.Unit{StartLine: 1, EndLine: 1, Content: "alpha"}},
		{fileIdx: 1, unit: chunk.Unit{StartLine: 2, EndLine: 2, Content: "beta"}},
		{fileIdx: 2, unit: chunk.Unit{StartLine: 1, EndLine: 1, Content: "gamma"}},
	}

	got, err := groupByFile(3, items, [][]float32{vectorFor("alpha"), vectorFor("beta"), vectorFor("gamma")})
	require.NoError(t, err)

	require.Len(t, got, 3, "one inputs slice per file, zero-unit files included")
	assert.Empty(t, got[0])
	assert.NotNil(t, got[0], "a zero-unit file must get an empty non-nil slice, the delete-without-inserting case")
	assert.Equal(t, []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "alpha", Embedding: vectorFor("alpha")},
		{StartLine: 2, EndLine: 2, Content: "beta", Embedding: vectorFor("beta")},
	}, got[1])
	assert.Equal(t, []chunkstore.ChunkInput{
		{StartLine: 1, EndLine: 1, Content: "gamma", Embedding: vectorFor("gamma")},
	}, got[2])
}

func TestIngestFileChunks_FewerVectorsThanTexts_FailsWithoutWriting(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, texts []string) ([][]float32, error) {
		return [][]float32{vectorFor(texts[0])}, nil
	}
	st, calls := newFakeStore()

	_, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha", "beta")})

	require.Error(t, err)
	assert.ErrorIs(t, err, errVectorCount, "a short vector list must abort rather than pair chunks with the wrong embeddings")
	assert.Contains(t, err.Error(), "sent 2 texts at offset 0, got 1 vectors", "the per-batch check must be what fires, naming the offset -- chunkInputsFor's total-count backstop reports a different shape")
	assert.Empty(t, *calls)
}

func TestIngestFileChunks_MoreVectorsThanTexts_FailsWithoutWriting(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, 0, len(texts)+1)
		for _, text := range texts {
			out = append(out, vectorFor(text))
		}
		return append(out, vectorFor("extra")), nil
	}
	st, calls := newFakeStore()

	_, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha")})

	assert.ErrorIs(t, err, errVectorCount)
	assert.Empty(t, *calls)
}

func TestIngestFileChunks_NonPositiveDimension_FailsBeforeSpendingAnEmbedCall(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	e.DimensionFunc = func() int { return 0 }
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha")})

	assert.ErrorIs(t, err, errBadDimension)
	assert.Empty(t, e.EmbedCalls(), "a dimension no vector can ever match must be caught before any network work")
	assert.Empty(t, *calls)
	assert.Equal(t, Stats{}, stats)
}

// Embedding runs before any store call precisely so an embedder failure
// costs zero writes -- docs/ingestion-spec.md's "on any failure, including
// an unreachable embedder, nothing commits".
func TestIngestFileChunks_EmbedError_AbortsBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	embedErr := errors.New("ollama unreachable")
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, _ []string) ([][]float32, error) { return nil, embedErr }
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha"), unitsFor(testFileB, "beta")})

	assert.ErrorIs(t, err, embedErr, "the embedder's own error must be wrapped, not replaced")
	assert.Empty(t, *calls, "no file may be written when the batch's embedding failed")
	assert.Equal(t, 1, stats.EmbedCalls, "the call that failed is still a call the operator paid for")
	assert.Zero(t, stats.FilesReplaced)
}

// A batch too big for one request must stop at the first failing request,
// not keep hammering the embedder with the remaining batches.
func TestIngestFileChunks_EmbedErrorMidBatch_StopsAtTheFailingRequest(t *testing.T) {
	t.Parallel()
	embedErr := errors.New("ollama out of memory")
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, texts []string) ([][]float32, error) {
		if len(e.EmbedCalls()) > 1 {
			return nil, embedErr
		}
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = vectorFor(text)
		}
		return out, nil
	}
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{syntheticUnits(t, testFileA, 3*maxEmbedBatch)})

	assert.ErrorIs(t, err, embedErr)
	assert.Len(t, e.EmbedCalls(), 2, "the third request must never be made once the second failed")
	assert.Equal(t, 2, stats.EmbedCalls)
	assert.Empty(t, *calls)
}

// The bead's central case (loam-c94.21): a store rejection on ONE file must
// not cost the rest of the batch. Three files with distinct paths AND
// distinct content are seeded (not identical content -- see the package's
// own note on what a fixture makes indistinguishable) so this test can
// prove the RIGHT files landed, not merely that some count of files landed.
func TestIngestFileChunks_StoreRejectsOneFile_SkipsItAndWritesTheRest(t *testing.T) {
	t.Parallel()
	rejectErr := errors.New("invalid byte sequence for encoding \"UTF8\": 0xa5")
	st, calls := newFakeStore()
	succeeding := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if file == testFileB {
			return nil, rejectErr
		}
		return succeeding(ctx, repoID, targetBranch, file, inputs)
	}
	logger, records := newCapturingLogger()
	files := []chunker.FileChunks{
		unitsFor(testFileA, "func Alpha() {}"),
		unitsFor(testFileB, "func Beta() {}"),
		unitsFor("pkg/c/c.go", "func Gamma() {}"),
	}

	stats, err := New(newFakeEmbedder(), logger).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)

	require.NoError(t, err, "a rejection alone must not fail the batch -- see Persist's doc comment for why")
	require.Len(t, *calls, 2, "the rejected file must be skipped, not retried, and the file after it must still be attempted")
	assert.Equal(t, testFileA, (*calls)[0].file)
	assert.Equal(t, "func Alpha() {}", (*calls)[0].inputs[0].Content, "the surviving files must carry THEIR OWN content, not the rejected file's")
	assert.Equal(t, "pkg/c/c.go", (*calls)[1].file)
	assert.Equal(t, "func Gamma() {}", (*calls)[1].inputs[0].Content)
	assert.Equal(t, Stats{FilesReplaced: 2, FilesRejected: 1, ChunksWritten: 2, EmbedCalls: 1}, stats, "the casualty must be counted separately from the files that landed")

	var rejectionLog *slog.Record
	for i, r := range *records {
		if r.Level == slog.LevelError {
			rejectionLog = &(*records)[i]
		}
	}
	require.NotNil(t, rejectionLog, "a rejected file must be logged at ERROR, not only counted")
	assert.Equal(t, testFileB, recordAttr(*rejectionLog, "file"), "the log line must name the file that was rejected")
}

// The same case run through Prepare then Persist separately (the shape the
// swap orchestrator, loam-c94.12, actually uses) rather than the combined
// IngestFileChunks convenience call, so the skip-and-continue behaviour is
// proven at the entry point production wires up, not only the test-only one.
func TestPersist_StoreRejectsOneFile_SkipsItAndWritesTheRest(t *testing.T) {
	t.Parallel()
	rejectErr := errors.New("constraint violation")
	st, calls := newFakeStore()
	succeeding := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if file == testFileA {
			return nil, rejectErr
		}
		return succeeding(ctx, repoID, targetBranch, file, inputs)
	}
	ix := New(newFakeEmbedder(), testLogger())
	repoID := uuid.Must(uuid.NewV7())
	files := []chunker.FileChunks{unitsFor(testFileA, "one"), unitsFor(testFileB, "two")}

	prepared, _, err := ix.Prepare(t.Context(), repoID, testBranch, files)
	require.NoError(t, err)
	stats, err := ix.Persist(t.Context(), st, repoID, testBranch, prepared)

	require.NoError(t, err)
	require.Len(t, *calls, 1, "newFakeStore only records a call that reaches the base function, i.e. one that was actually attempted and succeeded -- the rejected file's own attempt is proven by Stats.FilesRejected below, not by this slice")
	assert.Equal(t, testFileB, (*calls)[0].file, "the first file's rejection must not stop the second from being attempted")
	assert.Equal(t, Stats{FilesReplaced: 1, FilesRejected: 1, ChunksWritten: 1}, stats)
}

// A dead connection pool is exactly the class of failure this bead's own
// DESCRIPTION says must still abort: continuing to call ReplaceFileChunks
// against a pool that cannot serve any connection would just turn one real
// failure into N identical ones. puddle.ErrClosedPool is the actual
// sentinel pgxpool surfaces once its pool is closed (confirmed by reading
// pgxpool.Pool.Begin -> Acquire -> puddle.Pool.Acquire in pgx v5.10.0), so
// this test uses that value rather than an invented one.
func TestIngestFileChunks_DeadPool_AbortsAtThatFileWithoutTouchingLaterOnes(t *testing.T) {
	t.Parallel()
	st, calls := newFakeStore()
	failing := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if file == testFileB {
			return nil, fmt.Errorf("replacing chunks for %s: %w", file, puddle.ErrClosedPool)
		}
		return failing(ctx, repoID, targetBranch, file, inputs)
	}
	files := []chunker.FileChunks{
		unitsFor(testFileA, "alpha"),
		unitsFor(testFileB, "beta"),
		unitsFor("pkg/c/c.go", "gamma"),
	}

	stats, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)

	require.Error(t, err, "a dead pool must abort the batch, not be counted as a per-file rejection")
	assert.ErrorIs(t, err, puddle.ErrClosedPool)
	require.Len(t, *calls, 1, "the third file must never be attempted once the pool was found dead -- only file A's successful call is recorded")
	assert.Equal(t, testFileA, (*calls)[0].file)
	assert.Equal(t, 1, stats.FilesReplaced, "stats must report only the file that actually landed")
	assert.Zero(t, stats.FilesRejected, "a fatal, infrastructure-class failure is not a rejection")
}

// A transaction already poisoned by an earlier statement's error (Postgres
// SQLSTATE 25P02, in_failed_sql_transaction) must abort rather than being
// treated as yet another independent rejection, and the returned error
// must still name the EARLIER rejection that actually caused it, not just
// the immediate symptom.
//
// NARROWED by loam-c94.24, not deleted. This test was written when 25P02
// was the EXPECTED sequel to any rejection in a shared transaction --
// chunkstore took no savepoint, so one bad file poisoned the transaction
// and every later file reported 25P02. It now takes a savepoint and
// unwinds to it, so that cascade no longer happens on this package's own
// write path, and the sibling integration test
// (...RejectionInASharedTransactionSparesTheRestOfTheBatch) proves it
// against a real server.
//
// What survives is a genuinely different and still-reachable claim: 25P02
// remains the code Postgres returns for ANY statement issued on a
// transaction some OTHER participant already aborted -- the caller's
// transaction is shared with the graph track and with AdvanceIngestedRef,
// none of which are savepoint-protected -- and if Persist ever sees it, it
// must stop rather than count N more rejections and report a batch that
// nothing will commit. This is now defence in depth on the classifier
// rather than a description of the common path, which is exactly why it
// keeps its assertions and loses its old framing.
func TestIngestFileChunks_TransactionAlreadyAborted_AbortsAndNamesTheEarlierRejection(t *testing.T) {
	t.Parallel()
	firstRejectErr := errors.New("invalid byte sequence for encoding \"UTF8\": 0xa5 (SQLSTATE 22021)")
	cascadeErr := &pgconn.PgError{Code: pgerrcode.InFailedSQLTransaction, Message: "current transaction is aborted, commands ignored until end of transaction block"}
	st, calls := newFakeStore()
	base := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		switch file {
		case testFileB:
			return nil, firstRejectErr
		case "pkg/c/c.go":
			return nil, cascadeErr
		default:
			return base(ctx, repoID, targetBranch, file, inputs)
		}
	}
	files := []chunker.FileChunks{
		unitsFor(testFileA, "alpha"),
		unitsFor(testFileB, "beta"),
		unitsFor("pkg/c/c.go", "gamma"),
		unitsFor("pkg/d/d.go", "delta"),
	}

	stats, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)

	require.Error(t, err)
	assert.ErrorIs(t, err, cascadeErr, "the immediate fatal error must still be in the chain")
	assert.Contains(t, err.Error(), "pkg/c/c.go", "the error must name the file whose call actually failed")
	assert.Contains(t, err.Error(), testFileB, "the error must also name the EARLIER rejection, or an operator reading only the final error sees a misleading culprit")
	require.Len(t, *calls, 1, "the fourth file must never be attempted once the transaction was found unusable")
	assert.Equal(t, 1, stats.FilesRejected, "only the genuine rejection is counted, not the cascade symptom")
}

// chunkstore.ErrTransactionUnusable is the classification loam-c94.24
// folds in, and it is the one that closes the hole every check around it
// leaves open. A raw dead TCP connection -- the socket simply gone, no
// server response at all -- carries no *pgconn.PgError and matches none of
// pgx.ErrTxClosed, pgx.ErrTxCommitRollback or puddle.ErrClosedPool, so
// under this package's deliberate "unclassified means rejection" default
// it was counted as a per-file rejection and the loop carried on.
//
// That was harmless only by accident: the orchestrator's AdvanceIngestedRef
// ran immediately after Persist on the same doomed transaction and forced a
// rollback regardless. Savepoints removed that accident -- rejections are
// now genuinely survivable -- so a misclassified dead connection would be
// re-attempted once per remaining file, turning one infrastructure failure
// into N. The store reports the sentinel because it OBSERVED the failure:
// its own ROLLBACK TO SAVEPOINT is a statement that had to reach the
// server.
//
// The error here is wrapped exactly as chunkstore wraps it, so this test
// fails if that wrapping ever stops preserving the sentinel in the chain.
func TestIngestFileChunks_StoreReportsTransactionUnusable_AbortsWithoutTouchingLaterFiles(t *testing.T) {
	t.Parallel()
	deadConn := errors.New("write tcp 127.0.0.1:55000->127.0.0.1:5432: write: broken pipe")
	st, calls := newFakeStore()
	base := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if file == testFileB {
			return nil, fmt.Errorf("replacing chunks for repo %s file %s: %w: rolling back to savepoint after boom: %w",
				repoID, file, chunkstore.ErrTransactionUnusable, deadConn)
		}
		return base(ctx, repoID, targetBranch, file, inputs)
	}
	files := []chunker.FileChunks{
		unitsFor(testFileA, "alpha"),
		unitsFor(testFileB, "beta"),
		unitsFor("pkg/c/c.go", "gamma"),
	}

	stats, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)

	require.Error(t, err, "a transaction the store could not even unwind must abort the batch, not be counted as a per-file rejection")
	assert.ErrorIs(t, err, chunkstore.ErrTransactionUnusable)
	assert.ErrorIs(t, err, deadConn, "the driver's own error must still be in the chain for the operator")
	require.Len(t, *calls, 1, "the third file must never be attempted once the transaction was found unusable -- retrying a dead connection per file is exactly what this classification prevents")
	assert.Equal(t, testFileA, (*calls)[0].file)
	assert.Zero(t, stats.FilesRejected, "an unusable transaction is not a rejection, however much it looks like one from the call site")
}

// ctx cancellation observed BETWEEN two store calls (rather than before the
// loop starts, already covered elsewhere) must also stop the loop
// immediately, matching the top-of-loop ctx.Err() check that is this
// package's existing precedent for "this class aborts."
func TestIngestFileChunks_ContextCanceledBetweenStoreCalls_StopsWithoutTouchingLaterFiles(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	st, calls := newFakeStore()
	succeeding := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(c context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if file == testFileA {
			cancel()
		}
		return succeeding(c, repoID, targetBranch, file, inputs)
	}
	files := []chunker.FileChunks{unitsFor(testFileA, "alpha"), unitsFor(testFileB, "beta")}

	stats, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(ctx, st, uuid.Must(uuid.NewV7()), testBranch, files)

	assert.ErrorIs(t, err, context.Canceled)
	require.Len(t, *calls, 1, "the second file must never be attempted once ctx was canceled after the first")
	assert.Equal(t, 1, stats.FilesReplaced)
}

func TestIngestFileChunks_CanceledContext_MakesNoEmbedOrStoreCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e := newFakeEmbedder()
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(ctx, st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha")})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, e.EmbedCalls())
	assert.Empty(t, *calls)
	assert.Equal(t, Stats{}, stats)
}

// An already-canceled ctx must short-circuit even an empty batch, matching
// internal/ingest/graph.IngestFiles's own contract.
func TestIngestFileChunks_CanceledContextWithEmptyBatch_StillReturnsCtxError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	st, _ := newFakeStore()

	_, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(ctx, st, uuid.Must(uuid.NewV7()), testBranch, nil)

	assert.ErrorIs(t, err, context.Canceled)
}

// Cancellation observed AFTER embedding (the long network step) must stop
// the write loop rather than pressing on into a transaction the caller is
// about to abandon.
func TestIngestFileChunks_ContextCanceledDuringEmbed_StopsBeforeTheWriteLoop(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	e := newFakeEmbedder()
	e.EmbedFunc = func(_ context.Context, texts []string) ([][]float32, error) {
		cancel()
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = vectorFor(text)
		}
		return out, nil
	}
	st, calls := newFakeStore()

	stats, err := New(e, testLogger()).IngestFileChunks(ctx, st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, "alpha")})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, *calls, "no chunk may be written after ctx was canceled mid-flight")
	assert.Zero(t, stats.FilesReplaced)
}

// Two files whose chunk texts are byte-identical must each get their own
// row, not be deduplicated into one: a chunk is identified by (file, line
// range), and dropping one would make the second file unsearchable.
func TestIngestFileChunks_IdenticalTextInTwoFiles_WritesBothFilesChunks(t *testing.T) {
	t.Parallel()
	e := newFakeEmbedder()
	st, calls := newFakeStore()
	const shared = "func Same() {}"

	stats, err := New(e, testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch,
		[]chunker.FileChunks{unitsFor(testFileA, shared), unitsFor(testFileB, shared)})
	require.NoError(t, err)

	require.Len(t, *calls, 2)
	assert.Equal(t, shared, (*calls)[0].inputs[0].Content)
	assert.Equal(t, shared, (*calls)[1].inputs[0].Content)
	assert.Equal(t, 2, stats.ChunksWritten)
	assert.Equal(t, []string{shared, shared}, e.EmbedCalls()[0].Texts, "no deduplication: identical texts are embedded once per chunk, keeping the vector list positionally aligned with the chunk list")
}
