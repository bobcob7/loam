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
// one call must each land in the right Stats bucket and, for the two
// non-binary files, produce a FileChunks entry.
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
	require.Len(t, out, 2, "the binary file must not appear in the output at all")
	byPath := map[string][]chunk.Unit{}
	for _, fc := range out {
		byPath[fc.Path] = fc.Units
	}
	assert.Len(t, byPath["a.go"], 2, "func A and func B")
	assert.Len(t, byPath["README.md"], 1)
	assert.Equal(t, stats.UnitsProduced, len(byPath["a.go"])+len(byPath["README.md"]))
}

// TestChunkFiles_HardFailureCountedNotFatal proves one file's genuine parse
// failure does not abort the rest of the batch, mirroring internal/ingest/
// graph's TestIngestFiles-equivalent resilience.
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
	require.Len(t, out, 1)
	assert.Equal(t, "fine.go", out[0].Path, "the file that failed to parse must never reach the output")
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
