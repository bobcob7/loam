package parser_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bobcob7/loam/internal/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	src, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return src
}

// findNamed does a depth-first search for the first descendant (including n
// itself) whose Kind matches and whose "name" field's Text equals name, so
// tests can locate a known construct (e.g. a specific function declaration)
// without hard-coding the grammar's full shape.
func findNamed(n parser.Node, kind, name string) (parser.Node, bool) {
	if n.Kind() == kind {
		if nameNode, ok := n.ChildByFieldName("name"); ok && nameNode.Text() == name {
			return n, true
		}
	}
	for i := 0; i < n.ChildCount(); i++ {
		child, ok := n.Child(i)
		if !ok {
			continue
		}
		if found, ok := findNamed(child, kind, name); ok {
			return found, true
		}
	}
	return parser.Node{}, false
}

// findKind does a depth-first search for the first descendant (including n
// itself) whose Kind matches, for constructs with no "name" field (e.g.
// binary_expression, ERROR, type_declaration).
func findKind(n parser.Node, kind string) (parser.Node, bool) {
	if n.Kind() == kind {
		return n, true
	}
	for i := 0; i < n.ChildCount(); i++ {
		child, ok := n.Child(i)
		if !ok {
			continue
		}
		if found, ok := findKind(child, kind); ok {
			return found, true
		}
	}
	return parser.Node{}, false
}

// largeGoSource generates a synthetic Go file with n distinct top-level
// functions, comfortably larger than the package's ProgressCallback
// threshold, so a short ctx deadline reliably lands mid-parse rather than
// racing a parse that finishes before the deadline fires.
func largeGoSource(n int) []byte {
	var b strings.Builder
	b.WriteString("package large\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "func f%d(a, b int) int {\n\treturn a + b + %d\n}\n\n", i, i)
	}
	return []byte(b.String())
}

func TestParse_RegisteredLanguagesProduceValidTrees(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		fixture      string
		lang         parser.Language
		funcNodeKind string
		funcName     string
	}{
		{name: "go", fixture: "sample.go", lang: parser.LanguageGo, funcNodeKind: "function_declaration", funcName: "add"},
		{name: "python", fixture: "sample.py", lang: parser.LanguagePython, funcNodeKind: "function_definition", funcName: "add"},
		{name: "typescript", fixture: "sample.ts", lang: parser.LanguageTypeScript, funcNodeKind: "function_declaration", funcName: "add"},
		{name: "tsx", fixture: "sample.tsx", lang: parser.LanguageTSX, funcNodeKind: "function_declaration", funcName: "Greeting"},
		{name: "javascript", fixture: "sample.js", lang: parser.LanguageJavaScript, funcNodeKind: "function_declaration", funcName: "add"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := readFixture(t, tt.fixture)
			p := parser.NewParser(testLogger())
			defer p.Close()
			tree, err := p.Parse(t.Context(), tt.lang, src)
			require.NoError(t, err)
			require.NotNil(t, tree)
			defer tree.Close()
			assert.False(t, tree.HasError(), "expected a clean parse")
			assert.Equal(t, tt.lang, tree.Language())
			root := tree.RootNode()
			assert.NotEmpty(t, root.Kind())
			assert.Positive(t, root.ChildCount())
			_, found := findNamed(root, tt.funcNodeKind, tt.funcName)
			assert.True(t, found, "expected to find a %s named %q", tt.funcNodeKind, tt.funcName)
		})
	}
}

func TestParsePath_MapsExtensionToGrammar(t *testing.T) {
	t.Parallel()
	src := readFixture(t, "sample.go")
	p := parser.NewParser(testLogger())
	defer p.Close()
	tree, err := p.ParsePath(t.Context(), "testdata/sample.go", src)
	require.NoError(t, err)
	defer tree.Close()
	assert.Equal(t, parser.LanguageGo, tree.Language())
	assert.False(t, tree.HasError())
}

func TestParsePath_UnknownExtensionReturnsNoGrammar(t *testing.T) {
	t.Parallel()
	src := readFixture(t, "sample.md")
	p := parser.NewParser(testLogger())
	defer p.Close()
	tree, err := p.ParsePath(t.Context(), "testdata/sample.md", src)
	require.Error(t, err)
	assert.Nil(t, tree)
	assert.True(t, errors.Is(err, parser.ErrNoGrammar))
}

func TestLanguageForPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path    string
		want    parser.Language
		wantErr bool
	}{
		{path: "main.go", want: parser.LanguageGo},
		{path: "script.py", want: parser.LanguagePython},
		{path: "component.tsx", want: parser.LanguageTSX},
		{path: "module.ts", want: parser.LanguageTypeScript},
		{path: "index.js", want: parser.LanguageJavaScript},
		{path: "index.mjs", want: parser.LanguageJavaScript},
		{path: "README.md", wantErr: true},
		{path: "no-extension", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			got, err := parser.LanguageForPath(tt.path)
			if tt.wantErr {
				assert.True(t, errors.Is(err, parser.ErrNoGrammar))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParse_UnsupportedLanguageIsAnError(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(testLogger())
	defer p.Close()
	tree, err := p.Parse(t.Context(), parser.Language("cobol"), []byte("whatever"))
	require.Error(t, err)
	assert.Nil(t, tree)
	assert.False(t, errors.Is(err, parser.ErrNoGrammar), "unregistered Language values are a caller bug, not the no-grammar signal")
}

func TestParse_AlreadyCancelledContextReturnsBeforeParsing(t *testing.T) {
	t.Parallel()
	src := readFixture(t, "sample.go")
	p := parser.NewParser(testLogger())
	defer p.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tree, err := p.Parse(ctx, parser.LanguageGo, src)
	require.Error(t, err)
	assert.Nil(t, tree)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestParse_ContextCancelledMidParseReturnsContextError(t *testing.T) {
	t.Parallel()
	src := largeGoSource(60_000)
	require.Greater(t, len(src), 256*1024, "fixture must exceed the ProgressCallback threshold to exercise mid-parse cancellation")
	p := parser.NewParser(testLogger())
	defer p.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	tree, err := p.Parse(ctx, parser.LanguageGo, src)
	require.Error(t, err)
	assert.Nil(t, tree)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestParse_SyntaxErrorIsReportedOnTreeAndNode(t *testing.T) {
	t.Parallel()
	// Named .go.txt, not .go: it is intentionally invalid Go syntax, parsed
	// here via an explicit LanguageGo rather than extension detection, and
	// the .txt suffix keeps gofmt (which walks every .go file, unlike go
	// build/vet/test's testdata-aware package matching) from choking on it.
	src := readFixture(t, "sample_broken.go.txt")
	p := parser.NewParser(testLogger())
	defer p.Close()
	// Tree-sitter always returns a (partial) tree for broken input; the
	// error is signaled via HasError, never through err.
	tree, err := p.Parse(t.Context(), parser.LanguageGo, src)
	require.NoError(t, err)
	require.NotNil(t, tree)
	defer tree.Close()
	assert.True(t, tree.HasError())
	errNode, found := findKind(tree.RootNode(), "ERROR")
	require.True(t, found, "expected an ERROR node in the broken parse")
	assert.True(t, errNode.IsError())
	assert.True(t, errNode.HasError())
}

func TestNode_ByteAndPointRangesArePinnedToFixture(t *testing.T) {
	t.Parallel()
	src := readFixture(t, "sample.go")
	p := parser.NewParser(testLogger())
	defer p.Close()
	tree, err := p.Parse(t.Context(), parser.LanguageGo, src)
	require.NoError(t, err)
	defer tree.Close()
	fn, found := findNamed(tree.RootNode(), "function_declaration", "add")
	require.True(t, found)
	// testdata/sample.go pins "func add(a, b int) int {\n\treturn a + b\n}" at
	// bytes [193, 233), row 12 col 0 through row 14 col 1 (zero-indexed).
	// This is the exact contract that becomes symbols.line and chunk
	// start_line/end_line downstream — recompute with
	// bytes.Index(src, []byte("func add(")) if the fixture ever changes.
	start, end := fn.ByteRange()
	assert.Equal(t, uint(193), start)
	assert.Equal(t, uint(233), end)
	assert.Equal(t, parser.Point{Row: 12, Column: 0}, fn.StartPoint())
	assert.Equal(t, parser.Point{Row: 14, Column: 1}, fn.EndPoint())
}

func TestNode_NamedChildrenExcludeAnonymousTokensAndParentRoundTrips(t *testing.T) {
	t.Parallel()
	src := readFixture(t, "sample.go")
	p := parser.NewParser(testLogger())
	defer p.Close()
	tree, err := p.Parse(t.Context(), parser.LanguageGo, src)
	require.NoError(t, err)
	defer tree.Close()
	root := tree.RootNode()
	add, found := findNamed(root, "function_declaration", "add")
	require.True(t, found)
	binExpr, found := findKind(add, "binary_expression")
	require.True(t, found, "expected the a + b binary_expression inside add")
	assert.Equal(t, 3, binExpr.ChildCount(), "left identifier, '+' operator, right identifier")
	assert.Equal(t, 2, binExpr.NamedChildCount(), "the '+' operator token is anonymous")
	left, ok := binExpr.NamedChild(0)
	require.True(t, ok)
	assert.Equal(t, "a", left.Text())
	assert.True(t, left.IsNamed())
	assert.False(t, left.IsMissing())
	right, ok := binExpr.NamedChild(1)
	require.True(t, ok)
	assert.Equal(t, "b", right.Text())
	plus, ok := binExpr.Child(1)
	require.True(t, ok)
	assert.False(t, plus.IsNamed(), "the '+' token between the operands is anonymous")
	parent, ok := left.Parent()
	require.True(t, ok)
	assert.True(t, parent.Equals(binExpr))
	_, ok = root.Parent()
	assert.False(t, ok, "the root node has no parent")
}

func TestNode_SiblingNavigationReachesPrecedingDocComment(t *testing.T) {
	t.Parallel()
	src := readFixture(t, "sample.go")
	p := parser.NewParser(testLogger())
	defer p.Close()
	tree, err := p.Parse(t.Context(), parser.LanguageGo, src)
	require.NoError(t, err)
	defer tree.Close()
	root := tree.RootNode()
	typeDecl, found := findKind(root, "type_declaration")
	require.True(t, found)
	// tree-sitter-go emits a Go doc comment as a separate top-level sibling
	// of the declaration it documents, not as part of the declaration node.
	docComment, ok := typeDecl.PrevNamedSibling()
	require.True(t, ok, "expected the Greeter doc comment as a preceding named sibling")
	assert.Equal(t, "comment", docComment.Kind())
	assert.Contains(t, docComment.Text(), "Greeter says hello")
	back, ok := docComment.NextNamedSibling()
	require.True(t, ok)
	assert.True(t, back.Equals(typeDecl))
	_, ok = typeDecl.PrevSibling()
	assert.True(t, ok, "the anonymous-token-inclusive variant should also find the preceding sibling")
	methodDecl, found := findKind(root, "method_declaration")
	require.True(t, found)
	assert.False(t, typeDecl.Equals(methodDecl), "distinct nodes must not compare equal")
}
