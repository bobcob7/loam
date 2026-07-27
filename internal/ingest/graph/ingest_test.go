package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFakeStore returns a storeMock that records every ReplaceFileSymbols/
// ReplaceFileReferences call it receives (in order, interleaved) plus the
// counts a caller would see back -- every method has a configured Func, per
// this codebase's "unconfigured mock method panicking means real
// assertions never run" trap, so a test that reaches a call this mock does
// not expect fails loudly via the mock's own panic, not silently.
type recordedCall struct {
	method string
	file   string
	names  []string
}

func newFakeStore(t *testing.T) (*storeMock, *[]recordedCall) {
	t.Helper()
	calls := &[]recordedCall{}
	mock := &storeMock{
		ReplaceFileSymbolsFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error) {
			names := make([]string, len(symbols))
			for i, s := range symbols {
				names[i] = s.Name
			}
			*calls = append(*calls, recordedCall{method: "symbols", file: file, names: names})
			return nil, nil
		},
		ReplaceFileReferencesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []codegraph.ReferenceInput) (int64, error) {
			names := make([]string, len(refs))
			for i, r := range refs {
				names[i] = r.Name
			}
			*calls = append(*calls, recordedCall{method: "references", file: file, names: names})
			return int64(len(refs)), nil
		},
	}
	return mock, calls
}

// realParserFileParser wraps a real *parser.Parser behind the fileParser
// interface -- IngestFiles' tests want genuine extraction results (so
// Stats/store-call assertions mean something), not a hand-stubbed tree,
// while still going through the moq-mockable seam this package's own
// go-standards convention calls for.
func realParserFileParser(t *testing.T) fileParser {
	t.Helper()
	p := parser.NewParser(testLogger())
	t.Cleanup(p.Close)
	return &fileParserMock{
		ParseFunc: func(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error) {
			return p.Parse(ctx, lang, src)
		},
	}
}

func newRealIngestExtractor(t *testing.T) *Extractor {
	t.Helper()
	e, err := New(realParserFileParser(t), testLogger())
	require.NoError(t, err)
	t.Cleanup(e.Close)
	return e
}

// TestIngestFiles_WritesBothSymbolsAndReferences is MUTATION 1's kill
// switch: if IngestFiles only wrote symbols and forgot references (or vice
// versa), this fails -- go.ts's Validate call must show up as a
// ReplaceFileReferences call with "Validate" in it, not merely as a
// ReplaceFileSymbols call.
func TestIngestFiles_WritesBothSymbolsAndReferences(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	files := []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc Caller() { Callee() }\n\nfunc Callee() {}\n")},
	}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesExtracted)
	var symbolsCall, referencesCall *recordedCall
	for i := range *calls {
		c := &(*calls)[i]
		switch c.method {
		case "symbols":
			symbolsCall = c
		case "references":
			referencesCall = c
		}
	}
	require.NotNil(t, symbolsCall, "IngestFiles must call ReplaceFileSymbols")
	require.NotNil(t, referencesCall, "IngestFiles must call ReplaceFileReferences")
	assert.Contains(t, symbolsCall.names, "Caller")
	assert.Contains(t, symbolsCall.names, "Callee")
	assert.Contains(t, referencesCall.names, "Callee")
}

// TestIngestFiles_UnsupportedLanguage_SkipsWithoutStoreCall proves an
// unsupported-language file makes no store call at all (docs/ingestion-
// spec.md "skipped for the graph") and is counted, not silently dropped.
func TestIngestFiles_UnsupportedLanguage_SkipsWithoutStoreCall(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	files := []FileInput{{Path: "README.md", Content: []byte("# hi\n")}}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesSkippedUnsupportedLanguage)
	assert.Equal(t, 0, stats.FilesExtracted)
	assert.Empty(t, *calls, "an unsupported-language file must never reach the store")
}

// TestIngestFiles_HardParseFailure_SkipsFileNotBatch proves a single file's
// hard parse failure is logged/counted and does not stop the rest of the
// batch from being processed and written -- "a file that fails to parse
// must not fail the whole ingest".
func TestIngestFiles_HardParseFailure_SkipsFileNotBatch(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	mock := &fileParserMock{
		ParseFunc: func(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error) {
			if lang == parser.LanguageGo {
				return nil, boom
			}
			p := parser.NewParser(testLogger())
			defer p.Close()
			return p.Parse(ctx, lang, src)
		},
	}
	e, err := New(mock, testLogger())
	require.NoError(t, err)
	defer e.Close()
	st, calls := newFakeStore(t)
	files := []FileInput{
		{Path: "broken.go", Content: []byte("package a\n")},
		{Path: "fine.py", Content: []byte("def add(a, b):\n    return a + b\n")},
	}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.NoError(t, err, "a single file's hard parse failure must not abort the batch")
	assert.Equal(t, 1, stats.FilesFailed)
	assert.Equal(t, 1, stats.FilesExtracted)
	found := false
	for _, c := range *calls {
		if c.method == "symbols" && c.file == "fine.py" {
			found = true
		}
		assert.NotEqual(t, "broken.go", c.file, "the file that failed to parse must never reach the store")
	}
	assert.True(t, found, "the file after the failed one must still be processed and written")
}

// TestIngestFiles_SyntaxError_StillWritesBestEffort proves a file with a
// syntax error still gets its clean constructs written (not skipped like a
// hard failure), and is only counted via FilesWithSyntaxErrors.
func TestIngestFiles_SyntaxError_StillWritesBestEffort(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	files := []FileInput{
		{Path: "broken.go", Content: []byte("package broken\n\nfunc Clean() int {\n\treturn 1\n}\n\nfunc Broken(a, b int (\n\treturn a + b\n}\n")},
	}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesExtracted)
	assert.Equal(t, 1, stats.FilesWithSyntaxErrors)
	assert.Equal(t, 0, stats.FilesFailed)
	require.Len(t, *calls, 2, "both ReplaceFileSymbols and ReplaceFileReferences must still run for a best-effort partial extraction")
	for _, c := range *calls {
		if c.method == "symbols" {
			assert.Contains(t, c.names, "Clean")
			assert.NotContains(t, c.names, "Broken")
		}
	}
}

// TestIngestFiles_StoreErrorAbortsBatch proves a store write failure stops
// the batch immediately (the enclosing transaction is done for), rather
// than continuing to process later files as if nothing happened.
func TestIngestFiles_StoreErrorAbortsBatch(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	boom := errors.New("db boom")
	var symbolsCalls int
	st := &storeMock{
		ReplaceFileSymbolsFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error) {
			symbolsCalls++
			return nil, boom
		},
	}
	files := []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
		{Path: "b.go", Content: []byte("package a\n\nfunc B() {}\n")},
	}
	_, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, 1, symbolsCalls, "the second file must never be attempted once a store write fails")
}

// TestIngestFiles_ContextCanceledStopsImmediately proves ctx cancellation
// between files aborts the batch rather than plowing through the rest.
func TestIngestFiles_ContextCanceledStopsImmediately(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	files := []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
	}
	_, err := e.IngestFiles(ctx, st, uuid.Must(uuid.NewV7()), "main", files)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, *calls, "an already-canceled context must stop before any store call")
}

// TestIngestFiles_MultipleFiles_EachGetsItsOwnStoreCallWithItsOwnPath is
// MUTATION 2's kill switch: if the file path were accidentally hardcoded
// or reused across iterations (a classic loop-variable-capture bug), two
// distinct files would collide on one store call/path -- this proves each
// file's symbols land under its OWN path, not the last file's.
func TestIngestFiles_MultipleFiles_EachGetsItsOwnStoreCallWithItsOwnPath(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	files := []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
		{Path: "b.go", Content: []byte("package a\n\nfunc B() {}\n")},
	}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.FilesExtracted)
	seenFiles := map[string][]string{}
	for _, c := range *calls {
		if c.method == "symbols" {
			seenFiles[c.file] = c.names
		}
	}
	require.Contains(t, seenFiles, "a.go")
	require.Contains(t, seenFiles, "b.go")
	assert.Contains(t, seenFiles["a.go"], "A")
	assert.NotContains(t, seenFiles["a.go"], "B")
	assert.Contains(t, seenFiles["b.go"], "B")
	assert.NotContains(t, seenFiles["b.go"], "A")
}
