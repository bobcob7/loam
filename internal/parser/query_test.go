package parser_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/bobcob7/loam/internal/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseFixture reads and parses a testdata fixture as lang, registering
// Close for both the Parser and the Tree via t.Cleanup so callers don't have
// to.
func parseFixture(t *testing.T, fixture string, lang parser.Language) *parser.Tree {
	t.Helper()
	src := readFixture(t, fixture)
	p := parser.NewParser(testLogger())
	t.Cleanup(p.Close)
	tree, err := p.Parse(t.Context(), lang, src)
	require.NoError(t, err)
	t.Cleanup(tree.Close)
	return tree
}

// namesFor returns, in order, the text of every capture in captures whose
// @capture-name equals name.
func namesFor(captures []parser.Capture, name string) []string {
	var out []string
	for _, c := range captures {
		if c.Name == name {
			out = append(out, c.Node.Text())
		}
	}
	return out
}

func TestQuery_CapturesFunctionDeclarationsAcrossMVPGrammars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		fixture   string
		lang      parser.Language
		querySrc  string
		wantNames []string // @name capture text, in document order
	}{
		{
			name:      "go",
			fixture:   "sample.go",
			lang:      parser.LanguageGo,
			querySrc:  `(function_declaration name: (identifier) @name) @decl`,
			wantNames: []string{"add"},
		},
		{
			name:      "python",
			fixture:   "sample.py",
			lang:      parser.LanguagePython,
			querySrc:  `(function_definition name: (identifier) @name) @decl`,
			wantNames: []string{"__init__", "greet", "add"},
		},
		{
			name:      "typescript",
			fixture:   "sample.ts",
			lang:      parser.LanguageTypeScript,
			querySrc:  `(function_declaration name: (identifier) @name) @decl`,
			wantNames: []string{"add"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tree := parseFixture(t, tt.fixture, tt.lang)
			q, err := parser.NewQuery(tt.lang, tt.querySrc)
			require.NoError(t, err)
			defer q.Close()
			assert.Equal(t, tt.lang, q.Language())
			captures, err := q.Captures(t.Context(), tree)
			require.NoError(t, err)
			require.Len(t, captures, len(tt.wantNames)*2, "expected one @decl and one @name capture per match")
			assert.Equal(t, tt.wantNames, namesFor(captures, "name"))
			assert.Equal(t, len(tt.wantNames), len(namesFor(captures, "decl")))
		})
	}
}

func TestQuery_MethodDeclarationCapturesGoReceiverMethod(t *testing.T) {
	t.Parallel()
	// sample.go's Greet is a method_declaration (it has a receiver), a
	// distinct node kind from function_declaration in tree-sitter-go, so it
	// is invisible to the function_declaration query above and needs its own
	// pattern -- this pins that the two constructs really are distinguished
	// by the grammar, not merged by this package's wrapper.
	tree := parseFixture(t, "sample.go", parser.LanguageGo)
	q, err := parser.NewQuery(parser.LanguageGo, `(method_declaration name: (field_identifier) @name) @decl`)
	require.NoError(t, err)
	defer q.Close()
	captures, err := q.Captures(t.Context(), tree)
	require.NoError(t, err)
	require.Len(t, captures, 2)
	assert.Equal(t, []string{"Greet"}, namesFor(captures, "name"))
}

func TestNewQuery_InvalidSyntaxReturnsClearErrorNotPanic(t *testing.T) {
	t.Parallel()
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration name: (identifier) @name`)
	require.Error(t, err)
	assert.Nil(t, q)
	assert.Contains(t, err.Error(), "compiling query")
}

func TestNewQuery_NonexistentNodeKindReturnsClearError(t *testing.T) {
	t.Parallel()
	q, err := parser.NewQuery(parser.LanguageGo, `(not_a_real_tree_sitter_go_node_kind) @x`)
	require.Error(t, err)
	assert.Nil(t, q)
	assert.Contains(t, err.Error(), "compiling query")
	assert.Contains(t, err.Error(), "not_a_real_tree_sitter_go_node_kind", "the grammar's own diagnostic should name the bad node kind")
}

func TestNewQuery_UnsupportedLanguageIsAnError(t *testing.T) {
	t.Parallel()
	q, err := parser.NewQuery(parser.Language("cobol"), `(x) @y`)
	require.Error(t, err)
	assert.Nil(t, q)
	assert.False(t, errors.Is(err, parser.ErrNoGrammar), "unregistered Language values are a caller bug, not the no-grammar signal")
}

func TestQuery_CapturesOverTreeWithSyntaxErrorSkipsTheBrokenConstruct(t *testing.T) {
	t.Parallel()
	// sample_broken.go.txt's one function has malformed parameters/body and
	// is not recognized as a function_declaration by tree-sitter's error
	// recovery, so the query legitimately finds nothing -- this pins that
	// behavior (rather than merely asserting "no error") against
	// TestQuery_CapturesFunctionDeclarationsAcrossMVPGrammars's "go" case,
	// which proves the same query returns a non-empty, correct result on
	// clean input.
	tree := parseFixture(t, "sample_broken.go.txt", parser.LanguageGo)
	require.True(t, tree.HasError(), "fixture must actually contain a syntax error")
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration name: (identifier) @name) @decl`)
	require.NoError(t, err)
	defer q.Close()
	captures, err := q.Captures(t.Context(), tree)
	require.NoError(t, err, "a syntax error in the tree must not surface as a query error")
	assert.Empty(t, captures)
}

func TestQuery_Captures_AlreadyCancelledContextReturnsError(t *testing.T) {
	t.Parallel()
	tree := parseFixture(t, "sample.go", parser.LanguageGo)
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration) @decl`)
	require.NoError(t, err)
	defer q.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	captures, err := q.Captures(ctx, tree)
	require.Error(t, err)
	assert.Nil(t, captures)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestQuery_CapturesAfterCloseReturnsErrQueryClosed(t *testing.T) {
	t.Parallel()
	tree := parseFixture(t, "sample.go", parser.LanguageGo)
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration) @decl`)
	require.NoError(t, err)
	q.Close()
	captures, err := q.Captures(t.Context(), tree)
	require.Error(t, err)
	assert.Nil(t, captures)
	assert.True(t, errors.Is(err, parser.ErrQueryClosed))
}

func TestQuery_CloseIsSafeToCallTwice(t *testing.T) {
	t.Parallel()
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration) @decl`)
	require.NoError(t, err)
	q.Close()
	q.Close()
}

func TestQuery_CapturesIsSafeForConcurrentUseAcrossGoroutines(t *testing.T) {
	t.Parallel()
	src := readFixture(t, "sample.go")
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration name: (identifier) @name) @decl`)
	require.NoError(t, err)
	defer q.Close()
	const goroutines = 8
	results := make([][]string, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := parser.NewParser(testLogger())
			defer p.Close()
			tree, perr := p.Parse(t.Context(), parser.LanguageGo, src)
			if perr != nil {
				errs[i] = perr
				return
			}
			defer tree.Close()
			captures, cerr := q.Captures(t.Context(), tree)
			if cerr != nil {
				errs[i] = cerr
				return
			}
			// Extract plain strings here, before tree.Close() runs (LIFO
			// defers fire before this goroutine returns): a Capture's Node
			// is only valid while its Tree is open, and the tree is this
			// goroutine's alone to close.
			results[i] = namesFor(captures, "name")
		}(i)
	}
	wg.Wait()
	for i := 0; i < goroutines; i++ {
		require.NoError(t, errs[i])
		assert.Equal(t, []string{"add"}, results[i])
	}
}

func TestQuery_CloseDuringConcurrentCapturesDoesNotRace(t *testing.T) {
	t.Parallel()
	tree := parseFixture(t, "sample.go", parser.LanguageGo)
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration name: (identifier) @name) @decl`)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Either outcome is correct here: a clean result, or
			// ErrQueryClosed if Close won the race. The point of this test
			// is that go test -race finds nothing to complain about.
			_, _ = q.Captures(t.Context(), tree)
		}()
	}
	q.Close()
	wg.Wait()
}
