package graph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/bobcob7/loam/internal/testfixture"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// symbolInput is a small literal-building helper for readable expectations
// in assert.Contains calls below -- reflect.DeepEqual (which Contains uses
// per-element) dereferences the Line pointer, so two symbolInput values
// built from equal ints compare equal even though they are distinct
// pointers.
func symbolInput(line *int32, name, kind string) codegraph.SymbolInput {
	return codegraph.SymbolInput{Line: line, Name: name, Kind: kind}
}

// readFixtureFile materializes a fresh copy of fixture-polyglot (internal/
// testfixture, loam-li0.4 -- the realistic polyglot input this bead's
// ACCEPTANCE work is meant to prove itself against) and reads rel from it,
// so these tests exercise ExtractFile against the same real source the
// integration golden test (integration_test.go) and loam-li0.8 will also
// use, not a hand-copied snippet that could silently drift from it.
func readFixtureFile(t *testing.T, rel string) []byte {
	t.Helper()
	repo := testfixture.NewT(t.Context(), t)
	content, err := os.ReadFile(filepath.Join(repo.Dir(), rel))
	require.NoError(t, err)
	return content
}

func newRealExtractor(t *testing.T) *Extractor {
	t.Helper()
	p := parser.NewParser(testLogger())
	t.Cleanup(p.Close)
	e, err := New(p, testLogger())
	require.NoError(t, err)
	t.Cleanup(e.Close)
	return e
}

func TestExtractFile_UnsupportedLanguageIsSkippedNotError(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	result, ok, err := e.ExtractFile(t.Context(), "docs/OVERVIEW.md", []byte("# Fixture Polyglot Overview\n"))
	require.NoError(t, err)
	assert.False(t, ok, "a .md file has no registered grammar and must be skipped, not extracted")
	assert.Empty(t, result.Symbols)
	assert.Empty(t, result.References)
}

// TestExtractFile_Go_ModuleFunctionAndCrossFileReference is the fixture's
// central cross-file case: pkg/report/report.go declares Summarize and
// calls validate.Validate -- a selector-expression call, not a bare
// identifier call, proving the selector_expression query pattern (not just
// the plain-identifier one) actually fires.
func TestExtractFile_Go_ModuleFunctionAndCrossFileReference(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	content := readFixtureFile(t, "pkg/report/report.go")
	result, ok, err := e.ExtractFile(t.Context(), "pkg/report/report.go", content)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, result.HasSyntaxError)
	require.Len(t, result.Symbols, 2, "one module symbol plus one function symbol")
	assert.Contains(t, result.Symbols, symbolInput(nil, "report", kindModule))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(9), "Summarize", kindFunction))
	require.Len(t, result.References, 1)
	assert.Equal(t, "Validate", result.References[0].Name)
	assert.Equal(t, kindReferenceCall, result.References[0].Kind)
	assert.Equal(t, int32(10), result.References[0].Line)
}

// TestExtractFile_Go_TypeAndMethodDeclarations uses internal/parser's own
// sample.go (a Greeter struct plus a receiver method plus a plain
// function), proving type_spec and method_declaration extraction, which
// fixture-polyglot's own Go files never exercise (no types, no methods).
func TestExtractFile_Go_TypeAndMethodDeclarations(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	content, err := os.ReadFile("../../parser/testdata/sample.go")
	require.NoError(t, err)
	result, ok, err := e.ExtractFile(t.Context(), "sample.go", content)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, result.HasSyntaxError)
	assert.Empty(t, result.References, "sample.go's Greet/add bodies make no calls")
	require.Len(t, result.Symbols, 4, "module + Greeter type + Greet method + add function")
	assert.Contains(t, result.Symbols, symbolInput(nil, "sample", kindModule))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(4), "Greeter", kindType))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(9), "Greet", kindFunction), "a receiver method is kindFunction, not a separate 'method' kind -- see kindFunction's doc comment")
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(13), "add", kindFunction))
}

// TestExtractFile_Python_MutualRecursionReferences is the fixture's cycle
// fixture: is_even and is_odd call each other, giving the cycle-safety CTE
// (loam-c94.6/its consumers) something real to resolve.
func TestExtractFile_Python_MutualRecursionReferences(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	content := readFixtureFile(t, "scripts/parity.py")
	result, ok, err := e.ExtractFile(t.Context(), "scripts/parity.py", content)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, result.HasSyntaxError)
	require.Len(t, result.Symbols, 3, "module + is_even + is_odd")
	assert.Contains(t, result.Symbols, symbolInput(nil, "parity", kindModule))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(8), "is_even", kindFunction))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(15), "is_odd", kindFunction))
	require.Len(t, result.References, 2)
	assert.ElementsMatch(t, []string{"is_odd", "is_even"}, []string{result.References[0].Name, result.References[1].Name})
}

// TestExtractFile_TypeScript_InterfaceClassAndMethods uses internal/
// parser's own sample.ts (Greeting interface, Greeter class implementing
// it, a constructor and a method), proving interface_declaration and
// class_declaration/method_definition extraction, which fixture-polyglot's
// own TS files never exercise (plain functions only).
func TestExtractFile_TypeScript_InterfaceClassAndMethods(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	content, err := os.ReadFile("../../parser/testdata/sample.ts")
	require.NoError(t, err)
	result, ok, err := e.ExtractFile(t.Context(), "sample.ts", content)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, result.HasSyntaxError)
	assert.Empty(t, result.References)
	require.Len(t, result.Symbols, 6, "module + Greeting interface + Greeter class + constructor + greet + add")
	assert.Contains(t, result.Symbols, symbolInput(nil, "sample", kindModule))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(1), "Greeting", kindType))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(5), "Greeter", kindType))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(8), "constructor", kindFunction))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(12), "greet", kindFunction))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(17), "add", kindFunction))
}

// TestExtractFile_TypeScript_FixtureExportedFunctionAndCall is
// fixture-polyglot's own TS side of the cross-language ambiguous-name
// case: src/index.ts calls Validate (a bare identifier call, exercising the
// plain-identifier call pattern, complementing the Go selector-expression
// case above).
func TestExtractFile_TypeScript_FixtureExportedFunctionAndCall(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	content := readFixtureFile(t, "src/index.ts")
	result, ok, err := e.ExtractFile(t.Context(), "src/index.ts", content)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, result.Symbols, 2)
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(7), "summarize", kindFunction))
	require.Len(t, result.References, 1)
	assert.Equal(t, "Validate", result.References[0].Name)
	assert.Equal(t, int32(8), result.References[0].Line)
}

func TestExtractFile_JavaScript_ClassAndFunction(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	content, err := os.ReadFile("../../parser/testdata/sample.js")
	require.NoError(t, err)
	result, ok, err := e.ExtractFile(t.Context(), "sample.js", content)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, result.Symbols, 5, "module + Greeter class + constructor + greet + add")
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(1), "Greeter", kindType))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(2), "constructor", kindFunction))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(6), "greet", kindFunction))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(11), "add", kindFunction))
}

func TestExtractFile_TSX_FunctionAndInterface(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	content, err := os.ReadFile("../../parser/testdata/sample.tsx")
	require.NoError(t, err)
	result, ok, err := e.ExtractFile(t.Context(), "sample.tsx", content)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, result.Symbols, 3, "module + GreetingProps interface + Greeting function")
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(1), "GreetingProps", kindType))
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(5), "Greeting", kindFunction))
}

// TestExtractFile_PartialSyntaxError_StillExtractsCleanConstructs is the
// bead's "a file that fails to parse must not fail the whole ingest"
// requirement, at the single-file granularity: Tree-sitter's error
// recovery leaves Clean's function_declaration intact while Broken's
// malformed signature/body is swallowed into an ERROR subtree the query
// simply does not match (verified empirically: internal/parser/parser_
// test.go's TestQuery_CapturesOverTreeWithSyntaxErrorSkipsTheBrokenConstruct
// pins the identical shape for sample_broken.go.txt alone; this test adds
// a clean declaration alongside the broken one to prove the clean one
// survives, not just that the broken one is absent).
func TestExtractFile_PartialSyntaxError_StillExtractsCleanConstructs(t *testing.T) {
	t.Parallel()
	e := newRealExtractor(t)
	src := []byte("package broken\n\nfunc Clean() int {\n\treturn 1\n}\n\nfunc Broken(a, b int (\n\treturn a + b\n}\n")
	result, ok, err := e.ExtractFile(t.Context(), "broken.go", src)
	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, result.HasSyntaxError)
	require.Len(t, result.Symbols, 2, "module symbol plus exactly the one clean function -- Broken must not appear")
	assert.Contains(t, result.Symbols, symbolInput(int32Ptr(3), "Clean", kindFunction))
	for _, s := range result.Symbols {
		assert.NotEqual(t, "Broken", s.Name, "the malformed declaration must not surface as a symbol")
	}
}

func TestExtractFile_HardParseFailure_ReturnsErrorNotOK(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	mock := &fileParserMock{
		ParseFunc: func(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error) { return nil, boom },
	}
	e, err := New(mock, testLogger())
	require.NoError(t, err)
	defer e.Close()
	result, ok, err := e.ExtractFile(t.Context(), "a.go", []byte("package a\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.False(t, ok)
	assert.Empty(t, result.Symbols)
}

func TestExtractFile_ContextCanceled_ReturnsWrappedError(t *testing.T) {
	t.Parallel()
	mock := &fileParserMock{
		ParseFunc: func(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error) {
			return nil, ctx.Err()
		},
	}
	e, err := New(mock, testLogger())
	require.NoError(t, err)
	defer e.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, ok, err := e.ExtractFile(ctx, "a.go", []byte("package a\n"))
	require.Error(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestNew_QueryCompileErrorWraps proves New surfaces a bad query source as
// a wrapped compile error rather than panicking or silently dropping the
// language. It mutates the package-level querySources map (there is no
// other seam to inject a broken source through New's real signature) and
// restores it before returning, so it deliberately does NOT call
// t.Parallel(): every non-parallel test in this package's binary runs to
// completion, in order, before any t.Parallel() sibling resumes
// concurrently, so this mutate-then-restore window never overlaps a
// concurrent reader of querySources.
func TestNew_QueryCompileErrorWraps(t *testing.T) {
	original := querySources[parser.LanguageGo]
	querySources[parser.LanguageGo] = "(not_a_real_tree_sitter_go_node_kind) @x"
	defer func() { querySources[parser.LanguageGo] = original }()
	_, err := New(&fileParserMock{}, testLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compiling extraction query")
}
