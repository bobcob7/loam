package parser_test

import (
	"context"
	"errors"
	"sort"
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

// flatten concatenates every Match's Captures, in match order, into a
// single slice -- useful for assertions that don't care about pattern
// grouping (e.g. counting or collecting names), as opposed to assertions
// that specifically need the grouping Match provides.
func flatten(matches []parser.Match) []parser.Capture {
	var out []parser.Capture
	for _, m := range matches {
		out = append(out, m.Captures...)
	}
	return out
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
			matches, err := q.Captures(t.Context(), tree)
			require.NoError(t, err)
			require.Len(t, matches, len(tt.wantNames), "expected one Match per function declaration")
			for _, m := range matches {
				require.Len(t, m.Captures, 2, "expected one @decl and one @name capture per match")
			}
			captures := flatten(matches)
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
	matches, err := q.Captures(t.Context(), tree)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Len(t, matches[0].Captures, 2)
	assert.Equal(t, []string{"Greet"}, namesFor(matches[0].Captures, "name"))
}

func TestQuery_MatchesGroupCapturesFromTheSamePatternInstanceEvenWhenNested(t *testing.T) {
	t.Parallel()
	// A synthetic fixture with a function nested inside another: on nested
	// constructs, Tree-sitter's Matches iterator does not guarantee the
	// outer function's Match completes before the inner one's (or vice
	// versa), so pairing a "name" capture with a "decl" capture by adjacency
	// across a flattened stream can mis-pair them. Match grouping sidesteps
	// this: whichever order the two Matches arrive in, each Match's own
	// name+decl pair is self-consistent.
	src := []byte("def outer():\n    def inner():\n        pass\n    return inner\n")
	p := parser.NewParser(testLogger())
	defer p.Close()
	tree, err := p.Parse(t.Context(), parser.LanguagePython, src)
	require.NoError(t, err)
	defer tree.Close()
	q, err := parser.NewQuery(parser.LanguagePython, `(function_definition name: (identifier) @name) @decl`)
	require.NoError(t, err)
	defer q.Close()
	matches, err := q.Captures(t.Context(), tree)
	require.NoError(t, err)
	require.Len(t, matches, 2)
	var names []string
	for _, m := range matches {
		require.Len(t, m.Captures, 2)
		var name, decl parser.Capture
		for _, c := range m.Captures {
			switch c.Name {
			case "name":
				name = c
			case "decl":
				decl = c
			}
		}
		require.NotEmpty(t, name.Name, "each match must carry a name capture")
		require.NotEmpty(t, decl.Name, "each match must carry a decl capture")
		// The name capture must fall within its OWN match's decl capture --
		// proving the pairing is correct regardless of which order the two
		// matches arrived in.
		declStart, declEnd := decl.Node.ByteRange()
		nameStart, nameEnd := name.Node.ByteRange()
		assert.GreaterOrEqual(t, nameStart, declStart)
		assert.LessOrEqual(t, nameEnd, declEnd)
		names = append(names, name.Node.Text())
	}
	sort.Strings(names)
	assert.Equal(t, []string{"inner", "outer"}, names, "both the outer and the nested function must be found, order aside")
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
	assert.Contains(t, err.Error(), "unsupported language", "must be the unsupported-language precondition failure, not a query-syntax error that happens to also fail")
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
	matches, err := q.Captures(t.Context(), tree)
	require.NoError(t, err, "a syntax error in the tree must not surface as a query error")
	assert.Empty(t, matches)
}

func TestQuery_Captures_AlreadyCancelledContextReturnsError(t *testing.T) {
	t.Parallel()
	tree := parseFixture(t, "sample.go", parser.LanguageGo)
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration) @decl`)
	require.NoError(t, err)
	defer q.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	matches, err := q.Captures(ctx, tree)
	require.Error(t, err)
	assert.Nil(t, matches)
	assert.True(t, errors.Is(err, context.Canceled))
}

// cancelAfterNCalls reports ctx.Err() as nil for its first n calls, and as
// context.Canceled after -- letting a test force cancellation to be
// observed mid-loop (after some real progress) rather than only on the
// very first check before any work has happened.
type cancelAfterNCalls struct {
	context.Context
	n     int
	calls int
}

func (c *cancelAfterNCalls) Err() error {
	c.calls++
	if c.calls > c.n {
		return context.Canceled
	}
	return nil
}

func TestQuery_Captures_ContextCancelledMidLoopReturnsErrorNotPartialResults(t *testing.T) {
	t.Parallel()
	// sample.py yields 3 matches for this query (__init__, greet, add), so a
	// context that only cancels after letting some matches through actually
	// exercises the in-loop check between iterations, not just the
	// pre-loop check before the cursor starts. Deleting the in-loop
	// ctx.Err() check must fail this test: without it, the loop ignores
	// cancellation entirely and returns all 3 matches with no error.
	tree := parseFixture(t, "sample.py", parser.LanguagePython)
	q, err := parser.NewQuery(parser.LanguagePython, `(function_definition name: (identifier) @name) @decl`)
	require.NoError(t, err)
	defer q.Close()
	// n=2 lets the pre-loop check (call 1) and the first in-loop check
	// (call 2) both see nil -- so the cursor advances and yields at least
	// one match -- before the second in-loop check (call 3) cancels.
	ctx := &cancelAfterNCalls{Context: t.Context(), n: 2}
	matches, err := q.Captures(ctx, tree)
	require.Error(t, err)
	assert.Nil(t, matches, "cancellation must discard any matches accumulated so far, not return a partial result")
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestQuery_CapturesAfterCloseReturnsErrQueryClosed(t *testing.T) {
	t.Parallel()
	tree := parseFixture(t, "sample.go", parser.LanguageGo)
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration) @decl`)
	require.NoError(t, err)
	q.Close()
	matches, err := q.Captures(t.Context(), tree)
	require.Error(t, err)
	assert.Nil(t, matches)
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
			// Each goroutine parses and owns its own Tree: Tree-sitter
			// syntax trees are not safe to use from more than one thread at
			// a time (tree_sitter/api.h), so concurrent Captures callers
			// must never share one *Tree the way they may share one
			// *Query.
			p := parser.NewParser(testLogger())
			defer p.Close()
			tree, perr := p.Parse(t.Context(), parser.LanguageGo, src)
			if perr != nil {
				errs[i] = perr
				return
			}
			defer tree.Close()
			matches, cerr := q.Captures(t.Context(), tree)
			if cerr != nil {
				errs[i] = cerr
				return
			}
			// Extract plain strings here, before tree.Close() runs (LIFO
			// defers fire before this goroutine returns): a Capture's Node
			// is only valid while its Tree is open, and the tree is this
			// goroutine's alone to close.
			results[i] = namesFor(flatten(matches), "name")
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
	src := readFixture(t, "sample.go")
	q, err := parser.NewQuery(parser.LanguageGo, `(function_declaration name: (identifier) @name) @decl`)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine parses its own Tree -- see the doc comment on
			// TestQuery_CapturesIsSafeForConcurrentUseAcrossGoroutines for
			// why sharing one Tree across goroutines would be a different,
			// C-level bug that -race cannot see.
			p := parser.NewParser(testLogger())
			defer p.Close()
			tree, perr := p.Parse(t.Context(), parser.LanguageGo, src)
			if perr != nil {
				return
			}
			defer tree.Close()
			// Either outcome is correct here: a clean result, or
			// ErrQueryClosed if Close won the race. The point of this test
			// is that go test -race finds nothing to complain about on the
			// Query side while Close and Captures run concurrently.
			_, _ = q.Captures(t.Context(), tree)
		}()
	}
	q.Close()
	wg.Wait()
}
