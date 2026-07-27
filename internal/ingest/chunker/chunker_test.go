package chunker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChunkFile_Go_TopLevelDeclarations_OneChunkEachWithLeadingComment is
// this bead's golden case for Go: internal/parser/testdata/sample.go
// (already loam-c94.5's own golden fixture for type/method/function
// extraction) has a type declaration, a receiver method, and a plain
// function, each preceded by its own one-line doc comment except the
// last. Line ranges are pinned from the file's own line numbers (see the
// investigation dump this bead's author ran via internal/parser's ToSexp),
// not inferred from the code under test.
func TestChunkFile_Go_TopLevelDeclarations_OneChunkEachWithLeadingComment(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	content, err := os.ReadFile("../../parser/testdata/sample.go")
	require.NoError(t, err)
	units, result, ok, err := c.ChunkFile(t.Context(), "sample.go", content, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, chunk.Result{}, result, "sample.go's declarations are all well under a 2048-token budget")
	require.Len(t, units, 3, "type Greeter, method Greet, function add -- package_clause is not a declaration")
	assert.Equal(t, chunk.Unit{StartLine: 3, EndLine: 6}, stripContent(units[0]), "type Greeter, plus its one-line leading doc comment")
	assert.Contains(t, units[0].Content, "// Greeter says hello.")
	assert.Contains(t, units[0].Content, "type Greeter struct")
	assert.Equal(t, chunk.Unit{StartLine: 8, EndLine: 11}, stripContent(units[1]), "method Greet, plus its leading doc comment")
	assert.Contains(t, units[1].Content, "// Greet returns a greeting")
	assert.Contains(t, units[1].Content, "func (g Greeter) Greet() string")
	assert.Equal(t, chunk.Unit{StartLine: 13, EndLine: 15}, stripContent(units[2]), "function add has no leading comment")
	assert.Contains(t, units[2].Content, "func add(a, b int) int")
	assert.NotContains(t, units[2].Content, "package sample", "the package clause is not part of any symbol chunk")
	assertNoBlankUnits(t, units)
}

// TestChunkFile_Go_DocCommentNotAdjacent_IsNotAttached proves
// leadingCommentStart's contiguity check is load-bearing: a comment
// separated from the following declaration by a blank line must NOT be
// swept into that declaration's chunk.
//
// The fixture is shaped deliberately. The comment is the declaration's
// IMMEDIATELY preceding named sibling (package_clause comes before the
// comment, not between them), so the walk's first guard -- prev.Kind() !=
// "comment" -- does not fire, and the gap check is the only thing standing
// between the orphan comment and Standalone's chunk. Put the comment above
// the package clause instead and this test passes with the gap check
// deleted entirely, since the walk then stops on package_clause's kind and
// the contiguity branch is never reached at all. Verified by mutation:
// replacing the row+1 comparison with a constant false turns this test red.
func TestChunkFile_Go_DocCommentNotAdjacent_IsNotAttached(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	src := []byte("package lonely\n\n// orphan note, deliberately separated by a blank line\n\nfunc Standalone() int {\n\treturn 1\n}\n")
	units, _, ok, err := c.ChunkFile(t.Context(), "lonely.go", src, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 1)
	assert.Equal(t, 5, units[0].StartLine, "the gap before Standalone must stop the comment walk; chunk starts at 'func', not at the orphan comment on line 3")
	assert.NotContains(t, units[0].Content, "orphan note")
}

// TestChunkFile_Go_AdjacentDocComment_IsAttached is the positive twin of
// the test above, over the same fixture shape minus the blank line. Without
// it, the gap check could be strengthened into "never attach a comment at
// all" and the negative test alone would stay green -- so the pair pins the
// walk from both sides.
func TestChunkFile_Go_AdjacentDocComment_IsAttached(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	src := []byte("package lonely\n\n// Standalone returns one.\nfunc Standalone() int {\n\treturn 1\n}\n")
	units, _, ok, err := c.ChunkFile(t.Context(), "lonely.go", src, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 1)
	assert.Equal(t, 3, units[0].StartLine, "an adjacent doc comment is part of the symbol's chunk")
	assert.Contains(t, units[0].Content, "// Standalone returns one.")
}

// TestChunkFile_Go_TypeDeclarationBlock_IsOneChunk proves chunking happens
// at type_declaration (the node grammar wraps every type_spec in, whether
// there is one spec or several), not at the inner type_spec -- a grouped
// `type ( ... )` block must stay one chunk, not split per spec.
func TestChunkFile_Go_TypeDeclarationBlock_IsOneChunk(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	src := []byte("package p\n\ntype (\n\tA int\n\tB string\n)\n")
	units, _, ok, err := c.ChunkFile(t.Context(), "types.go", src, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 1, "a grouped type block is one declaration, one chunk")
	assert.Contains(t, units[0].Content, "A int")
	assert.Contains(t, units[0].Content, "B string")
}

// TestChunkFile_Python_OneChunkPerFunction is the fixture's golden Python
// case: scripts/parity.py's is_even/is_odd have no leading "#" comment
// (their docstring is the function body's first statement, not a
// preceding sibling), so each is a plain two-declaration file.
func TestChunkFile_Python_OneChunkPerFunction(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	content := readFixtureFile(t, "scripts/parity.py")
	units, _, ok, err := c.ChunkFile(t.Context(), "scripts/parity.py", content, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 2, "is_even and is_odd -- the module docstring is not a declaration")
	assert.Equal(t, 8, units[0].StartLine)
	assert.Equal(t, 12, units[0].EndLine)
	assert.Contains(t, units[0].Content, "def is_even(n: int) -> bool:")
	assert.Equal(t, 15, units[1].StartLine)
	assert.Equal(t, 19, units[1].EndLine)
	assert.Contains(t, units[1].Content, "def is_odd(n: int) -> bool:")
	assertNoBlankUnits(t, units)
}

// TestChunkFile_TypeScript_ExportedFunction_IncludesExportKeywordAndJSDoc
// is the fixture's golden TypeScript case, and the export_statement-
// wrapping regression test: src/validate.ts's Validate is declared as
// `export function Validate(...) {...}`, wrapped in an export_statement
// node whose own span is what must become the chunk -- chunking at the
// inner function_declaration alone would silently drop the leading
// "export " keyword from the persisted, embedded chunk text.
func TestChunkFile_TypeScript_ExportedFunction_IncludesExportKeywordAndJSDoc(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	content := readFixtureFile(t, "src/validate.ts")
	units, _, ok, err := c.ChunkFile(t.Context(), "src/validate.ts", content, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 1)
	assert.Equal(t, 1, units[0].StartLine, "the JSDoc block comment is the file's very first line")
	assert.Equal(t, 11, units[0].EndLine)
	assert.Contains(t, units[0].Content, "/**", "the leading JSDoc comment must be attached")
	assert.Contains(t, units[0].Content, "export function Validate")
	assertNoBlankUnits(t, units)
}

// TestChunkFile_TypeScript_ImportStatementExcludedFromChunk proves a
// top-level import is not itself chunked and does not leak into the
// following declaration's chunk: src/index.ts imports Validate on line 1,
// then declares its own exported summarize function starting at its JSDoc
// comment on line 3.
func TestChunkFile_TypeScript_ImportStatementExcludedFromChunk(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	content := readFixtureFile(t, "src/index.ts")
	units, _, ok, err := c.ChunkFile(t.Context(), "src/index.ts", content, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 1)
	assert.Equal(t, 3, units[0].StartLine)
	assert.Equal(t, 9, units[0].EndLine)
	assert.NotContains(t, units[0].Content, "import { Validate }")
	assert.Contains(t, units[0].Content, "export function summarize")
}

// TestChunkFile_TypeScript_ClassIsOneChunkIncludingMethods proves a class
// declaration (with its constructor and methods nested inside) becomes a
// single symbol chunk, not one chunk per method -- "top-level symbol"
// means the class itself, per loam-c94.10's own DESIGN.
func TestChunkFile_TypeScript_ClassIsOneChunkIncludingMethods(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	content, err := os.ReadFile("../../parser/testdata/sample.ts")
	require.NoError(t, err)
	units, _, ok, err := c.ChunkFile(t.Context(), "sample.ts", content, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 3, "interface Greeting, class Greeter (whole, incl. constructor+greet), function add")
	assert.Equal(t, chunk.Unit{StartLine: 1, EndLine: 3}, stripContent(units[0]))
	assert.Equal(t, chunk.Unit{StartLine: 5, EndLine: 15}, stripContent(units[1]))
	assert.Contains(t, units[1].Content, "constructor(name: string)")
	assert.Contains(t, units[1].Content, "greet(): string")
	assert.Equal(t, chunk.Unit{StartLine: 17, EndLine: 19}, stripContent(units[2]))
}

// TestChunkFile_JavaScript_ClassAndFunction_ExpressionStatementExcluded
// mirrors the TypeScript class case for plain JavaScript, and additionally
// proves a top-level expression statement (module.exports = ...) is not a
// declaration and gets no chunk of its own.
func TestChunkFile_JavaScript_ClassAndFunction_ExpressionStatementExcluded(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	content, err := os.ReadFile("../../parser/testdata/sample.js")
	require.NoError(t, err)
	units, _, ok, err := c.ChunkFile(t.Context(), "sample.js", content, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 2, "class Greeter, function add -- module.exports=... is an expression_statement, not chunked")
	assert.Equal(t, chunk.Unit{StartLine: 1, EndLine: 9}, stripContent(units[0]))
	assert.Equal(t, chunk.Unit{StartLine: 11, EndLine: 13}, stripContent(units[1]))
	for _, u := range units {
		assert.NotContains(t, u.Content, "module.exports")
	}
}

// TestChunkFile_TypeScript_NonDeclarationExportStatement_NotChunked proves
// declarationSpan correctly leaves an export_statement alone when it does
// not wrap one of lang's chunkable declaration kinds (`export { a };` has
// no "declaration" field at all), and that a bare top-level const is
// likewise not a declaration this package chunks.
func TestChunkFile_TypeScript_NonDeclarationExportStatement_NotChunked(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	src := []byte("const answer = 42;\nexport { answer };\n")
	units, _, ok, err := c.ChunkFile(t.Context(), "reexport.ts", src, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, units, "neither a bare const nor a re-export statement is a chunkable declaration")
}

// TestChunkFile_Markdown_OneChunkPerHeading is the fixture's golden docs
// case: docs/OVERVIEW.md has four "#"/"##" headings (an H1 title plus
// three H2 sections), so it must yield exactly four chunks, boundaries
// pinned to the file's own known line numbers.
func TestChunkFile_Markdown_OneChunkPerHeading(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	content := readFixtureFile(t, "docs/OVERVIEW.md")
	units, _, ok, err := c.ChunkFile(t.Context(), "docs/OVERVIEW.md", content, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 4, "# title + ## Validation + ## Reporting + ## Recursion Cycle")
	assert.Equal(t, chunk.Unit{StartLine: 1, EndLine: 5}, stripContent(units[0]))
	assert.Contains(t, units[0].Content, "# Fixture Polyglot Overview")
	assert.Equal(t, chunk.Unit{StartLine: 6, EndLine: 12}, stripContent(units[1]))
	assert.Contains(t, units[1].Content, "## Validation")
	assert.Equal(t, chunk.Unit{StartLine: 13, EndLine: 18}, stripContent(units[2]))
	assert.Contains(t, units[2].Content, "## Reporting")
	assert.Equal(t, chunk.Unit{StartLine: 19, EndLine: 23}, stripContent(units[3]))
	assert.Contains(t, units[3].Content, "## Recursion Cycle")
	assertNoBlankUnits(t, units)
}

// TestChunkFile_PlainTextFile_UsesOverlappingSlidingWindows is the golden
// sliding-window case: a plain .txt file (no grammar, not markdown) with
// enough lines to need three windows. Window boundaries are asserted
// exactly, including the 20-line overlap between consecutive windows --
// the specific property a mutation that dropped the overlap (stride ==
// window size) would break: with no overlap, window 2 would start at line
// 101, not 81.
func TestChunkFile_PlainTextFile_UsesOverlappingSlidingWindows(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	lines := make([]string, 250)
	for i := range lines {
		lines[i] = "line content"
	}
	src := []byte(strings.Join(lines, "\n") + "\n")
	units, _, ok, err := c.ChunkFile(t.Context(), "notes.txt", src, fixedBudgeter(1_000_000))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 3)
	assert.Equal(t, chunk.Unit{StartLine: 1, EndLine: 100}, stripContent(units[0]))
	assert.Equal(t, chunk.Unit{StartLine: 81, EndLine: 180}, stripContent(units[1]), "window 2 must start at 81 (100-line window, 20-line overlap), not 101 (no overlap)")
	assert.Equal(t, chunk.Unit{StartLine: 161, EndLine: 250}, stripContent(units[2]), "the final window is clipped to the file's last line")
	assert.Equal(t, 20, units[0].EndLine-units[1].StartLine+1, "window 1 and window 2 overlap by exactly 20 lines")
	assert.Equal(t, 20, units[1].EndLine-units[2].StartLine+1, "window 2 and window 3 overlap by exactly 20 lines")
}

// TestChunkFile_PlainTextFile_ShorterThanOneWindow_IsSingleChunk proves the
// small-file edge of the sliding-window strategy.
func TestChunkFile_PlainTextFile_ShorterThanOneWindow_IsSingleChunk(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	src := []byte("one\ntwo\nthree\n")
	units, _, ok, err := c.ChunkFile(t.Context(), "short.txt", src, fixedBudgeter(2048))
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, units, 1)
	assert.Equal(t, chunk.Unit{StartLine: 1, EndLine: 3}, stripContent(units[0]))
}

// TestChunkFile_BinaryFile_SkippedWithNoChunksAndNoError is the
// binary/non-text acceptance criterion: a file whose content contains a
// NUL byte is skipped entirely -- ok is false, not merely "zero chunks",
// so a caller can tell "deliberately skipped" apart from "an empty text
// file chunked to nothing."
func TestChunkFile_BinaryFile_SkippedWithNoChunksAndNoError(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	src := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	units, result, ok, err := c.ChunkFile(t.Context(), "logo.png", src, fixedBudgeter(2048))
	require.NoError(t, err)
	assert.False(t, ok, "binary content must be reported as skipped, not chunked")
	assert.Empty(t, units)
	assert.Equal(t, chunk.Result{}, result)
}

// TestChunkFile_EnforceBudgetSplitsEveryStrategysOutput is this bead's
// central integration proof: whichever strategy produced the raw units --
// symbol, section, or sliding-window -- every unit ChunkFile actually
// returns must fit budgeter's token budget. This is MUTATION 1's kill
// switch: if ChunkFile ever stopped calling chunk.EnforceBudget (or called
// it and discarded the result), the raw, oversized unit each strategy
// naturally produces here would escape untouched and this test would fail
// on the LessOrEqual assertion, not merely on a byte-count mismatch.
func TestChunkFile_EnforceBudgetSplitsEveryStrategysOutput(t *testing.T) {
	t.Parallel()
	tinyBudgeter := fixedBudgeter(1) // TokenBudgetChars(1) == 2 bytes
	budget := chunk.TokenBudgetChars(1)
	cases := []struct {
		name string
		path string
		src  []byte
	}{
		{"go symbol chunk", "big.go", []byte("package p\n\nfunc LongEnoughToExceedTheTinyBudget() int {\n\treturn 1\n}\n")},
		{"markdown section chunk", "big.md", []byte("# Title\n\nThis paragraph is long enough on its own to exceed a two-byte budget easily.\n")},
		{"sliding-window plain chunk", "big.txt", []byte("this is a plain text file with more than two bytes of content on one single unbroken line\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newRealChunker(t)
			units, result, ok, err := c.ChunkFile(t.Context(), tc.path, tc.src, tinyBudgeter)
			require.NoError(t, err)
			require.True(t, ok)
			require.NotEmpty(t, units)
			assert.Greater(t, result.UnitsSplit, 0, "the tiny budget must force at least one split")
			for i, u := range units {
				assert.LessOrEqualf(t, len(u.Content), budget, "%s: piece %d exceeds the enforced budget -- EnforceBudget was bypassed", tc.name, i)
			}
		})
	}
}

// TestChunkFile_HardParseFailure_ReturnsErrorNotOK mirrors internal/ingest/
// graph's identical test: a fileParser that fails to produce any tree at
// all is a genuine error, not a silent fallback to another strategy.
func TestChunkFile_HardParseFailure_ReturnsErrorNotOK(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	mock := &fileParserMock{
		ParseFunc: func(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error) { return nil, boom },
	}
	c := NewChunker(mock, testLogger())
	units, result, ok, err := c.ChunkFile(t.Context(), "a.go", []byte("package a\n"), fixedBudgeter(2048))
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.False(t, ok)
	assert.Empty(t, units)
	assert.Equal(t, chunk.Result{}, result)
}

// TestChunkFile_ContextCanceled_ReturnsWrappedError proves ctx cancellation
// on the symbol-chunking path (the only path that does real, potentially
// slow work) aborts with a wrapped context.Canceled rather than proceeding
// as if nothing happened.
func TestChunkFile_ContextCanceled_ReturnsWrappedError(t *testing.T) {
	t.Parallel()
	c := newRealChunker(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	units, _, ok, err := c.ChunkFile(ctx, "a.go", []byte("package a\n\nfunc A() {}\n"), fixedBudgeter(2048))
	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, units)
	assert.ErrorIs(t, err, context.Canceled)
}

// stripContent zeroes a unit's Content so require.Equal can compare just
// the line-range fields without also needing to spell out the exact
// source text at the call site -- the content itself is checked separately
// via targeted Contains/NotContains assertions.
func stripContent(u chunk.Unit) chunk.Unit {
	return chunk.Unit{StartLine: u.StartLine, EndLine: u.EndLine}
}
