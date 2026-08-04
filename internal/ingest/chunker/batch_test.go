package chunker

import (
	"context"
	"errors"
	"testing"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChunkFiles_TracksStatsAcrossMixedBatch mirrors internal/ingest/graph's
// own batch-stats tests: a binary file, a Go file, and a markdown file in
// one call must each land in the right Stats bucket, and every one of the
// three -- binary included -- must produce a FileChunks entry so its prior
// chunks are replaced downstream (loam-8uo).
func TestChunkFiles_TracksStatsAcrossMixedBatch(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	files := []FileInput{
		{Path: "logo.png", Content: append([]byte("\x89PNG"), 0, 0, 0, 0)},
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n\nfunc B() {}\n")},
		{Path: "README.md", Content: []byte("# Hi\n\nSome text.\n")},
	}
	out, stats, err := c.ChunkFiles(t.Context(), files, fixedBudgeter(2048))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesSkippedBinary)
	assert.Equal(t, 2, stats.FilesChunked)
	assert.Equal(t, 0, stats.FilesFailed)
	require.Len(t, out, 3, "every input file needs an entry, so every one gets its chunks replaced")
	byPath := map[string][]chunk.Unit{}
	for _, fc := range out {
		byPath[fc.Path] = fc.Units
	}
	require.Contains(t, byPath, "logo.png")
	assert.Empty(t, byPath["logo.png"], "a binary file's entry carries zero units: replace-with-nothing, i.e. drop")
	assert.Len(t, byPath["a.go"], 2, "func A and func B")
	assert.Len(t, byPath["README.md"], 1)
	assert.Equal(t, stats.UnitsProduced, len(byPath["a.go"])+len(byPath["README.md"]))
}

// TestChunkFiles_BinaryFileStillYieldsAnEntryToDropItsStaleChunks is the
// unit-level half of loam-8uo: a file that was text at the previous ingest
// and is binary at this one is in the plan's REPARSE set (it changed), not
// its DropFiles, so the only thing that can ever delete its stale chunks
// is a FileChunks entry reaching internal/ingest/vectors. The integration
// half (a real text->binary mutation across two ingests, asserting zero
// surviving chunk rows) lives in internal/ingest/orchestrator's
// integration_test.go.
func TestChunkFiles_BinaryFileStillYieldsAnEntryToDropItsStaleChunks(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	files := []FileInput{{Path: "notes.txt", Content: append([]byte("was text"), 0, 1, 2)}}
	out, stats, err := c.ChunkFiles(t.Context(), files, fixedBudgeter(2048))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesSkippedBinary)
	require.Len(t, out, 1, "a now-binary file must not vanish from the batch: nothing else can drop its chunks")
	assert.Equal(t, "notes.txt", out[0].Path)
	assert.Empty(t, out[0].Units)
}

// TestChunkFiles_InvalidUTF8FileCountedNotSkipped is loam-c94.20's Stats-level
// proof: a file that is not binary (no NUL byte) but carries an invalid
// UTF-8 byte is chunked -- it still gets real units and lands in
// FilesChunked, distinct from FilesSkippedBinary -- and is additionally
// counted in FilesSanitizedInvalidUTF8, so an operator can tell it was
// altered without it being missing from the index.
func TestChunkFiles_InvalidUTF8FileCountedNotSkipped(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	files := []FileInput{
		{Path: "queries/legacy.sql", Content: []byte("SELECT 1; -- old comment \xa5 bullet\n")},
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
	}
	out, stats, err := c.ChunkFiles(t.Context(), files, fixedBudgeter(2048))
	require.NoError(t, err)
	assert.Equal(t, 2, stats.FilesChunked)
	assert.Equal(t, 0, stats.FilesSkippedBinary)
	assert.Equal(t, 1, stats.FilesSanitizedInvalidUTF8, "only the .sql file carried an invalid byte")
	require.Len(t, out, 2)
	assert.NotEmpty(t, out[0].Units, "the sanitized file must still be chunked, not dropped")
}

// TestChunkFiles_HardFailureCountedNotFatal proves one file's genuine parse
// failure does not abort the rest of the batch, mirroring internal/ingest/
// graph's TestIngestFiles-equivalent resilience -- and that the failed file
// still gets a zero-unit entry so its stale chunks are dropped rather than
// left searchable (loam-8uo).
func TestChunkFiles_HardFailureCountedNotFatal(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	real := parser.NewParser(testLogger())
	t.Cleanup(real.Close)
	mock := &fileParserMock{
		ParseFunc: func(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error) {
			if lang == parser.LanguagePython {
				return nil, boom
			}
			return real.Parse(ctx, lang, src)
		},
	}
	c := NewChunker(mock, testLogger())
	files := []FileInput{
		{Path: "broken.py", Content: []byte("def broken():\n    pass\n")},
		{Path: "fine.go", Content: []byte("package a\n\nfunc Fine() {}\n")},
	}
	out, stats, err := c.ChunkFiles(t.Context(), files, fixedBudgeter(2048))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesFailed)
	assert.Equal(t, 1, stats.FilesChunked)
	require.Len(t, out, 2, "the failed file still needs an entry so its stale chunks are dropped")
	assert.Equal(t, "broken.py", out[0].Path)
	assert.Empty(t, out[0].Units, "a file that could not be chunked replaces its chunks with nothing")
	assert.Equal(t, "fine.go", out[1].Path)
	assert.NotEmpty(t, out[1].Units)
}

// TestChunkFiles_ContextCanceled_StopsBatchAndReturnsError proves ctx
// cancellation aborts the whole batch rather than silently skipping the
// remaining files as if they were binary or failed.
func TestChunkFiles_ContextCanceled_StopsBatchAndReturnsError(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	files := []FileInput{{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")}}
	out, _, err := c.ChunkFiles(ctx, files, fixedBudgeter(2048))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, out)
}
