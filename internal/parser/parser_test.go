package parser_test

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

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
