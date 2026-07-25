package parser

import (
	"context"
	"fmt"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// Query is a compiled Tree-sitter query for one Language. Compiling a query
// validates every node kind, field, and capture name it names against the
// grammar, which is comparatively expensive: callers (symbol extraction,
// loam-c94.5) should compile one Query per language once and reuse it across
// every file of that language, rather than recompiling per file.
//
// A Query holds no per-run state of its own: Captures opens and closes its
// own Tree-sitter query cursor internally for the duration of a single call,
// so a Query never exposes a cursor a caller could leak or double-free.
// Because of that, a Query is safe to share and call Captures on
// concurrently from multiple goroutines -- unlike Parser, which owns mutable
// C parser state and must not be shared across goroutines (see ParserPool).
// This means a query cache or pool keyed by Language, mirroring ParserPool,
// is unnecessary: there is nothing to lease, since nothing mutable is held
// between calls.
//
// The Tree passed to Captures is a different story: Tree-sitter's own C API
// documents that a syntax tree is not safe to use from more than one thread
// at a time without copying it first (tree_sitter/api.h: "You need to copy a
// syntax tree in order to use it on more than one thread at a time, as
// syntax trees are not thread safe."). So while one *Query may be shared
// across concurrent Captures calls, each of those calls must pass its own
// *Tree -- concurrent callers should each hold a Parser (or lease one from a
// ParserPool) and parse their own Tree, never share one Tree across
// goroutines even for read-only querying.
//
// Close still needs external discipline, but less than Parser/Tree require:
// Captures and Close synchronize with each other, so a Close racing with an
// in-flight Captures call is a checked ErrQueryClosed, not a silent
// use-after-free. Close must still only be called once the caller is done
// with the Query -- there is no way to "reopen" it, and the underlying
// ts.Query.Close has no double-close guard of its own, so this package's
// closed flag is load-bearing against a double-free.
type Query struct {
	mu     sync.RWMutex
	closed bool
	inner  *ts.Query
	names  []string
	lang   Language
}

// Capture is one (name, Node) pair produced by running a Query over a Tree:
// Name is the @capture-name written in the query source, and Node is the
// syntax node it was bound to.
type Capture struct {
	Name string
	Node Node
}

// Match groups every Capture produced by one occurrence of a Query's
// pattern matching within a Tree. Captures within the same Match came from
// the same pattern instance and can safely be associated with one another
// -- e.g. pairing a "name" capture with the "decl" capture that encloses it
// for the same declaration. Captures from two different Matches must never
// be paired by adjacency: Tree-sitter does not guarantee Matches arrive in
// document order (only that each Match's own Captures are internally
// consistent), so on nested constructs (a function inside a function) two
// Matches can complete in either order and interleaving by position alone
// silently mis-pairs an inner declaration's name with an outer one's, or
// vice versa.
type Match struct {
	Captures []Capture
}

// NewQuery compiles source as a Tree-sitter query against lang's registered
// grammar. It returns an error -- never panics -- when source has invalid
// query syntax, or names a node kind, field, or capture the grammar does not
// define. NewQuery returns an error wrapping errUnsupportedLanguage if lang
// has no registered grammar (see LanguageForPath).
func NewQuery(lang Language, source string) (*Query, error) {
	grammar, ok := grammars[lang]
	if !ok {
		return nil, fmt.Errorf("compiling query for %q: %w", lang, errUnsupportedLanguage)
	}
	inner, qerr := ts.NewQuery(grammar, source)
	if qerr != nil {
		// %s, not %w: qerr's concrete type is a tree-sitter type, and this
		// package promises none of those escape past its own boundary, even
		// inside an error chain.
		return nil, fmt.Errorf("compiling query for %q: %s", lang, qerr)
	}
	return &Query{inner: inner, names: inner.CaptureNames(), lang: lang}, nil
}

// Language reports which grammar this Query was compiled against.
func (q *Query) Language() Language {
	return q.lang
}

// Close releases the query's underlying Tree-sitter memory. Safe to call
// more than once, and safe to call while another goroutine is inside
// Captures: Close blocks until in-flight Captures calls finish, and any
// Captures call that starts after Close returns ErrQueryClosed.
func (q *Query) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.inner.Close()
}

// Captures runs q over tree's root node and returns every Match -- one per
// pattern occurrence, each carrying its own Captures -- in the order
// Tree-sitter's query cursor produces them.
//
// Captures creates and releases its own Tree-sitter query cursor for the
// duration of this one call via the cursor's Matches iterator (not its
// per-capture Captures iterator: the latter's upstream Next implementation
// mallocs a C TSQueryMatch header on every single capture with no
// corresponding free on any return path -- a real, measured per-capture C
// heap leak that would grow without bound over the hundreds of millions of
// captures a long-lived ingest worker runs across a repo. The Matches
// iterator's Next frees its header on every call, so this package uses it
// and flattens each match's own captures instead). No cursor is ever
// exposed to the caller, so there is nothing beyond the Query itself for a
// caller to leak or double-free. tree must not have been Closed -- like
// Parser and Tree, that remains an external discipline this method does not
// check -- and must not be shared with another concurrent Captures call
// from a different goroutine (see Query's doc comment).
//
// Captures returns ctx's error, wrapped, if ctx is already done, or becomes
// done before the query finishes; a large tree with many matches is walked
// incrementally rather than in one uninterruptible call, so cancellation is
// checked between every match rather than needing Tree-sitter's C-level
// progress callback (contrast Parser.parseBytes, which cannot check
// incrementally because a single Tree-sitter parse call is uninterruptible).
// It returns a wrapped ErrQueryClosed if q has been Closed.
func (q *Query) Captures(ctx context.Context, tree *Tree) ([]Match, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return nil, fmt.Errorf("running query for %q: %w", q.lang, ErrQueryClosed)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("running query for %q: %w", q.lang, err)
	}
	cursor := ts.NewQueryCursor()
	defer cursor.Close()
	root := tree.inner.RootNode()
	it := cursor.Matches(q.inner, root, tree.src)
	var out []Match
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("running query for %q: %w", q.lang, err)
		}
		match := it.Next()
		if match == nil {
			break
		}
		captures := make([]Capture, 0, len(match.Captures))
		for _, c := range match.Captures {
			node := c.Node
			captures = append(captures, Capture{Name: q.names[c.Index], Node: Node{inner: &node, src: tree.src}})
		}
		out = append(out, Match{Captures: captures})
	}
	return out, nil
}
