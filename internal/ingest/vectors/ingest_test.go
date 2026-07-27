package vectors

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/chunkstore"
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

// chunkInputsFor is the guard that keeps a chunk-list/vector-list mismatch
// from becoming an index-out-of-range panic partway through the write loop.
// It gets direct tests because it is unreachable through IngestFileChunks
// once embedAll's per-batch check is in place, and a panic here would be
// far worse than an error: it would unwind past the orchestrator's deferred
// rollback rather than failing the ingest cleanly.
func TestChunkInputsFor_FewerVectorsThanChunks_ErrorsInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	files := []chunker.FileChunks{unitsFor(testFileA, "alpha", "beta"), unitsFor(testFileB, "gamma")}

	var got [][]chunkstore.ChunkInput
	var err error
	require.NotPanics(t, func() { got, err = chunkInputsFor(files, [][]float32{vectorFor("alpha")}) },
		"a short vector list must be rejected up front, never indexed past the end mid-write")

	assert.Nil(t, got)
	require.ErrorIs(t, err, errVectorCount)
	assert.Contains(t, err.Error(), "3 chunks to pair with 1 vectors")
}

func TestChunkInputsFor_MoreVectorsThanChunks_Errors(t *testing.T) {
	t.Parallel()
	files := []chunker.FileChunks{unitsFor(testFileA, "alpha")}

	_, err := chunkInputsFor(files, [][]float32{vectorFor("alpha"), vectorFor("beta")})

	assert.ErrorIs(t, err, errVectorCount, "a surplus vector means the alignment is unknowable too, not merely wasteful")
}

func TestChunkInputsFor_PairsEachFilesUnitsWithItsOwnVectorsInOrder(t *testing.T) {
	t.Parallel()
	files := []chunker.FileChunks{
		{Path: "empty.go"},
		unitsFor(testFileA, "alpha", "beta"),
		unitsFor(testFileB, "gamma"),
	}

	got, err := chunkInputsFor(files, [][]float32{vectorFor("alpha"), vectorFor("beta"), vectorFor("gamma")})
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

func TestIngestFileChunks_StoreError_AbortsAtThatFileWithoutTouchingLaterOnes(t *testing.T) {
	t.Parallel()
	storeErr := errors.New("deadlock detected")
	st, calls := newFakeStore()
	failing := st.ReplaceFileChunksFunc
	st.ReplaceFileChunksFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
		if file == testFileB {
			return nil, storeErr
		}
		return failing(ctx, repoID, targetBranch, file, inputs)
	}
	files := []chunker.FileChunks{
		unitsFor(testFileA, "alpha"),
		unitsFor(testFileB, "beta"),
		unitsFor("pkg/c/c.go", "gamma"),
	}

	stats, err := New(newFakeEmbedder(), testLogger()).IngestFileChunks(t.Context(), st, uuid.Must(uuid.NewV7()), testBranch, files)

	require.Error(t, err)
	assert.ErrorIs(t, err, storeErr)
	assert.Contains(t, err.Error(), testFileB, "the error must name the file that failed")
	require.Len(t, *calls, 1, "the third file must never be attempted once the second failed")
	assert.Equal(t, testFileA, (*calls)[0].file)
	assert.Equal(t, 1, stats.FilesReplaced, "stats must report only the file that actually landed")
	assert.Equal(t, 1, stats.ChunksWritten)
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
