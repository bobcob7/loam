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
// ReplaceFileReferences/RecomputeGraphEdges call it receives (in order,
// interleaved) plus the counts a caller would see back -- every method has a
// configured Func, per this codebase's "unconfigured mock method panicking
// means real assertions never run" trap, so a test that reaches a call this
// mock does not expect fails loudly via the mock's own panic, not silently.
// RecomputeGraphEdgesFunc always reports 3 edges recomputed (a stand-in
// nonzero count) so Stats.EdgesRecomputed-propagation tests have something
// distinguishable from the zero value to assert against.
type recordedCall struct {
	method       string
	file         string
	names        []string
	repoID       uuid.UUID
	targetBranch string
}

const fakeRecomputedEdgeCount = int64(3)

func newFakeStore(t *testing.T) (*storeMock, *[]recordedCall) {
	t.Helper()
	calls := &[]recordedCall{}
	mock := &storeMock{
		ReplaceFileSymbolsFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error) {
			names := make([]string, len(symbols))
			for i, s := range symbols {
				names[i] = s.Name
			}
			*calls = append(*calls, recordedCall{method: "symbols", file: file, names: names, repoID: repoID, targetBranch: targetBranch})
			return nil, nil
		},
		ReplaceFileReferencesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []codegraph.ReferenceInput) (int64, error) {
			names := make([]string, len(refs))
			for i, r := range refs {
				names[i] = r.Name
			}
			*calls = append(*calls, recordedCall{method: "references", file: file, names: names, repoID: repoID, targetBranch: targetBranch})
			return int64(len(refs)), nil
		},
		RecomputeGraphEdgesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch string) (int64, error) {
			*calls = append(*calls, recordedCall{method: "recompute", repoID: repoID, targetBranch: targetBranch})
			return fakeRecomputedEdgeCount, nil
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
	for _, c := range *calls {
		assert.NotEqual(t, "symbols", c.method, "an unsupported-language file must never reach ReplaceFileSymbols")
		assert.NotEqual(t, "references", c.method, "an unsupported-language file must never reach ReplaceFileReferences")
	}
	require.Len(t, *calls, 1, "RecomputeGraphEdges still runs once for the batch even though the only file was skipped -- it resolves whatever symbols/references already exist for the repo, not just this batch's files")
	assert.Equal(t, "recompute", (*calls)[0].method)
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
	require.Len(t, *calls, 3, "ReplaceFileSymbols, ReplaceFileReferences, and the batch-final RecomputeGraphEdges must all run for a best-effort partial extraction")
	for _, c := range *calls {
		if c.method == "symbols" {
			assert.Contains(t, c.names, "Clean")
			assert.NotContains(t, c.names, "Broken")
		}
	}
}

// TestIngestFiles_HardParseFailure_LeavesExistingSymbolsRowsUntouched is
// loam-1z0's required test: it drives ExtractFile's hard-failure path
// (err!=nil, ok=false, not a ctx error) through a full IngestFiles call and
// asserts the resulting symbols ROW STATE, not merely that a store call was
// skipped -- TestIngestFiles_HardParseFailure_SkipsFileNotBatch above
// already proves the latter, by call-recording alone.
//
// Two things make the assertions here actually discriminate, both fixed
// during loam-1z0's review round 1 after being caught by mutation:
//
//  1. staleSymbols is a value ExtractFile's real extraction of broken.go
//     could never produce (moduleSymbol("broken.go") is
//     {Name:"broken",Kind:kindModule}; this seed is neither that nor any
//     shape a function/type match would build) -- so a mutation that
//     "helpfully" writes moduleSymbol(f.Path) on the failure path instead of
//     making no call at all is now distinguishable from a genuinely
//     untouched row, not accidentally identical to one.
//  2. The "no call was made" half of the claim is asserted directly against
//     st.ReplaceFileSymbolsCalls() (moq-generated), not inferred from the
//     row map staying put -- the row map alone cannot tell "never called"
//     apart from "called with this exact value again".
//
// fine.py's written symbols are asserted BY CONTENT (module symbol "fine"
// plus function symbol "add"), not merely "some non-nil, non-stale value" --
// otherwise a mutation that writes every successfully extracted file with
// symbols:nil would still pass.
//
// This test injects the failure via a fake fileParser rather than driving
// it with real source, and that choice covers only PART of what err!=nil
// means (see ExtractFile's doc comment for the full breakdown): Parse's own
// "no tree at all" sub-case has been traced, including into the vendored
// Tree-sitter C, and confirmed to have NO real-input trigger in this build,
// so THAT sub-case genuinely needs a fake to pin defensively. But err!=nil
// also covers query.Captures failing with ErrQueryClosed on a tree that
// parsed fine, and unlike the no-tree sub-case, THAT one does not need a
// fake at all: reaching it needs only the shutdown STATE (an
// already-Closed Extractor), not the shutdown TIMING of a real race --
// see TestIngestFiles_ClosedExtractor_LeavesExistingSymbolsRowsUntouched
// below, which reproduces it with the real parser and zero fakes. This
// test is pinning the CONTRACT for "extraction produced no usable result"
// generically, at the one sub-case (no-tree) that has no real-input
// reproduction; the other (ErrQueryClosed) gets its own real-trigger test.
func TestIngestFiles_HardParseFailure_LeavesExistingSymbolsRowsUntouched(t *testing.T) {
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
	staleSymbols := []codegraph.SymbolInput{{Line: int32Ptr(7), Name: "StaleFromPreviousIngest", Kind: kindFunction}}
	symbolRows := map[string][]codegraph.SymbolInput{"broken.go": staleSymbols}
	st := &storeMock{
		ReplaceFileSymbolsFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error) {
			symbolRows[file] = symbols
			return nil, nil
		},
		ReplaceFileReferencesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []codegraph.ReferenceInput) (int64, error) {
			return int64(len(refs)), nil
		},
		RecomputeGraphEdgesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch string) (int64, error) {
			return fakeRecomputedEdgeCount, nil
		},
	}
	files := []FileInput{
		{Path: "broken.go", Content: []byte("package a\n")},
		{Path: "fine.py", Content: []byte("def add(a, b):\n    return a + b\n")},
	}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.NoError(t, err, "a single file's hard parse failure must not abort the batch")
	assert.Equal(t, 1, stats.FilesFailed)
	assert.Equal(t, 0, stats.FilesSkippedUnsupportedLanguage, "a hard failure must be counted once, in FilesFailed only -- Stats documents the four counters as mutually exclusive")
	for _, c := range st.ReplaceFileSymbolsCalls() {
		assert.NotEqual(t, "broken.go", c.File, "no ReplaceFileSymbols call must ever be made for a file whose extraction hard-failed")
	}
	assert.Equal(t, staleSymbols, symbolRows["broken.go"], "broken.go's pre-existing symbols row must be exactly what it was seeded as -- nothing overwrote it")
	require.Contains(t, symbolRows, "fine.py", "the file after the failed one must still be reparsed and written")
	fineNames := make([]string, len(symbolRows["fine.py"]))
	for i, s := range symbolRows["fine.py"] {
		fineNames[i] = s.Name
	}
	assert.ElementsMatch(t, []string{"fine", "add"}, fineNames, "fine.py must be written with its freshly extracted module and function symbols, not an empty or stale set")
}

// TestIngestFiles_ClosedExtractor_LeavesExistingSymbolsRowsUntouched pins
// the OTHER err!=nil sub-case ExtractFile's doc comment describes --
// query.Captures returning parser.ErrQueryClosed on a tree that parsed
// perfectly -- and does so with the REAL parser and no fake fileParser at
// all, unlike the test above. This is possible because reaching
// ErrQueryClosed does not require winning a real shutdown race: Query.
// Captures checks q.closed before ctx.Err() (parser/query.go), so it is
// enough to reproduce the shutdown STATE (an Extractor that has already had
// Close called on it, exactly what run()'s deferred closeIngest does at
// cmd/server/main.go:286) rather than the shutdown TIMING. Constructing the
// Extractor, closing it, and only then calling IngestFiles reproduces the
// real trigger deterministically.
func TestIngestFiles_ClosedExtractor_LeavesExistingSymbolsRowsUntouched(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	e.Close()
	staleSymbols := []codegraph.SymbolInput{{Line: int32Ptr(7), Name: "StaleFromPreviousIngest", Kind: kindFunction}}
	symbolRows := map[string][]codegraph.SymbolInput{"broken.go": staleSymbols}
	st := &storeMock{
		ReplaceFileSymbolsFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error) {
			symbolRows[file] = symbols
			return nil, nil
		},
		ReplaceFileReferencesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []codegraph.ReferenceInput) (int64, error) {
			return int64(len(refs)), nil
		},
		RecomputeGraphEdgesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch string) (int64, error) {
			return fakeRecomputedEdgeCount, nil
		},
	}
	files := []FileInput{{Path: "broken.go", Content: []byte("package a\n")}}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", files)
	require.NoError(t, err, "ErrQueryClosed is not a ctx error: it must not abort the batch")
	assert.Equal(t, 1, stats.FilesFailed)
	assert.Equal(t, 0, stats.FilesSkippedUnsupportedLanguage, "a hard failure must be counted once, in FilesFailed only -- Stats documents the four counters as mutually exclusive")
	assert.Empty(t, st.ReplaceFileSymbolsCalls(), "no store call must be made for a file whose extraction failed via a closed query")
	assert.Equal(t, staleSymbols, symbolRows["broken.go"], "broken.go's pre-existing symbols row must survive untouched, exactly like the no-tree sub-case")
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

// --- loam-c94.6: wiring RecomputeGraphEdges into IngestFiles. ---

// TestIngestFiles_RecomputesGraphEdgesOnceAfterAllFiles is MUTATION A's kill
// switch (dropping the RecomputeGraphEdges call entirely -- this fails with
// zero "recompute" calls recorded and stats.EdgesRecomputed left at its
// zero value) and MUTATION B's kill switch (moving the call inside the
// per-file loop, once per file, instead of once after the whole batch --
// this fails because it asserts the call happens exactly ONCE, as the LAST
// recorded call, for a 2-file batch that would otherwise record it twice or
// interleaved with the per-file symbol/reference calls).
func TestIngestFiles_RecomputesGraphEdgesOnceAfterAllFiles(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	repoID := uuid.Must(uuid.NewV7())
	files := []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
		{Path: "b.go", Content: []byte("package a\n\nfunc B() {}\n")},
	}
	stats, err := e.IngestFiles(t.Context(), st, repoID, "main", files)
	require.NoError(t, err)
	recomputeCalls := 0
	for _, c := range *calls {
		if c.method == "recompute" {
			recomputeCalls++
		}
	}
	assert.Equal(t, 1, recomputeCalls, "RecomputeGraphEdges must be called exactly once per IngestFiles batch, never once per file")
	require.NotEmpty(t, *calls)
	assert.Equal(t, "recompute", (*calls)[len(*calls)-1].method, "RecomputeGraphEdges must run AFTER every file's symbols/references have been written, not interleaved with them")
	assert.Equal(t, fakeRecomputedEdgeCount, stats.EdgesRecomputed, "Stats.EdgesRecomputed must carry through RecomputeGraphEdges' own return count")
}

// TestIngestFiles_RecomputeGraphEdges_ScopedToCorrectRepoAndBranch is
// MUTATION C's kill switch: if RecomputeGraphEdges were ever called with a
// hardcoded or swapped repoID/targetBranch instead of the ones IngestFiles
// was actually given, this catches it -- two distinct repoIDs and a
// non-default branch name make a hardcoded/swapped value observably wrong
// rather than accidentally correct.
func TestIngestFiles_RecomputeGraphEdges_ScopedToCorrectRepoAndBranch(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	repoID := uuid.Must(uuid.NewV7())
	const branch = "feature/edge-scope"
	_, err := e.IngestFiles(t.Context(), st, repoID, branch, []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
	})
	require.NoError(t, err)
	var recompute *recordedCall
	for i := range *calls {
		if (*calls)[i].method == "recompute" {
			recompute = &(*calls)[i]
		}
	}
	require.NotNil(t, recompute, "RecomputeGraphEdges must have been called")
	assert.Equal(t, repoID, recompute.repoID, "RecomputeGraphEdges must be scoped to the exact repoID IngestFiles received")
	assert.Equal(t, branch, recompute.targetBranch, "RecomputeGraphEdges must be scoped to the exact targetBranch IngestFiles received, not a hardcoded default")
}

// TestIngestFiles_RecomputeGraphEdges_RunsEvenWhenBatchIsEmpty proves
// recompute is not gated on FilesExtracted > 0: an ingest whose plan had
// only deleted/renamed-file drops (applied by the orchestrator before
// calling IngestFiles) still needs graph_edges rebuilt from whatever
// symbols/references remain, per this bead's DESIGN ("per-repo recompute",
// not incremental patching).
func TestIngestFiles_RecomputeGraphEdges_RunsEvenWhenBatchIsEmpty(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", nil)
	require.NoError(t, err)
	require.Len(t, *calls, 1, "an empty files batch must still trigger exactly one RecomputeGraphEdges call")
	assert.Equal(t, "recompute", (*calls)[0].method)
	assert.Equal(t, fakeRecomputedEdgeCount, stats.EdgesRecomputed)
}

// TestIngestFiles_RecomputeGraphEdgesErrorWraps is MUTATION D's kill switch:
// if IngestFiles ever swallowed RecomputeGraphEdges' error instead of
// returning it, this fails with require.Error finding none.
func TestIngestFiles_RecomputeGraphEdgesErrorWraps(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	boom := errors.New("recompute boom")
	st := &storeMock{
		ReplaceFileSymbolsFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error) {
			return nil, nil
		},
		ReplaceFileReferencesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []codegraph.ReferenceInput) (int64, error) {
			return 0, nil
		},
		RecomputeGraphEdgesFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch string) (int64, error) {
			return 0, boom
		},
	}
	stats, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, boom, "the underlying RecomputeGraphEdges error must be matchable by identity through the wrap")
	assert.Zero(t, stats.EdgesRecomputed, "a failed recompute must leave EdgesRecomputed at zero, not a partial/stale count")
}

// TestIngestFiles_RecomputeGraphEdges_NotCalledAfterStoreError is MUTATION
// F's kill switch: if IngestFiles ever called RecomputeGraphEdges after a
// per-file store write already failed, this mock -- which configures NO
// RecomputeGraphEdgesFunc -- panics the instant that call happens (this
// codebase's "unconfigured mock method panicking means real assertions
// never run" convention doubling as a hard trip-wire here), so a passing
// run is itself the proof recompute was never reached.
func TestIngestFiles_RecomputeGraphEdges_NotCalledAfterStoreError(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	boom := errors.New("db boom")
	st := &storeMock{
		ReplaceFileSymbolsFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error) {
			return nil, boom
		},
	}
	_, err := e.IngestFiles(t.Context(), st, uuid.Must(uuid.NewV7()), "main", []FileInput{
		{Path: "a.go", Content: []byte("package a\n\nfunc A() {}\n")},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}

// TestIngestFiles_RecomputeGraphEdges_SkippedOnEmptyBatchWithCanceledContext
// proves the final ctx.Err() check guards the empty-files path too: the
// per-file loop's own ctx check (proved by
// TestIngestFiles_ContextCanceledStopsImmediately) never runs at all when
// files is empty, so without a second check immediately before the
// recompute call, an already-canceled context would silently reach
// RecomputeGraphEdges instead of aborting.
func TestIngestFiles_RecomputeGraphEdges_SkippedOnEmptyBatchWithCanceledContext(t *testing.T) {
	t.Parallel()
	e := newRealIngestExtractor(t)
	st, calls := newFakeStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.IngestFiles(ctx, st, uuid.Must(uuid.NewV7()), "main", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, *calls, "an already-canceled context must stop before RecomputeGraphEdges is ever called, even for an empty files batch")
}
