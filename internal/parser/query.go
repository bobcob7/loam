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
// Close still needs external discipline, but less than Parser/Tree require:
// Captures and Close synchronize with each other, so a Close racing with an
// in-flight Captures call is a checked ErrQueryClosed, not a silent
// use-after-free. Close must still only be called once the caller is done
// with the Query -- there is no way to "reopen" it.
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

// Captures runs q over tree's root node and returns every capture in
// document order, as a flat (name, Node) sequence. Callers that need to
// know which pattern produced a capture should compile one query per
// construct rather than inspecting pattern indices, which this package does
// not expose alongside captures.
//
// Captures creates and releases its own Tree-sitter query cursor for the
// duration of this one call; no cursor is ever exposed to the caller, so
// there is nothing beyond the Query itself for a caller to leak or
// double-free. tree must not have been Closed -- like Parser and Tree, that
// remains an external discipline this method does not check.
//
// Captures returns ctx's error, wrapped, if ctx is already done, or becomes
// done before the query finishes; a large tree with many matches is walked
// incrementally rather than in one uninterruptible call, so cancellation is
// checked between every capture rather than needing Tree-sitter's C-level
// progress callback (contrast Parser.parseBytes, which cannot check
// incrementally because a single Tree-sitter parse call is uninterruptible).
// It returns ErrQueryClosed if q has been Closed.
func (q *Query) Captures(ctx context.Context, tree *Tree) ([]Capture, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return nil, ErrQueryClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("running query for %q: %w", q.lang, err)
	}
	cursor := ts.NewQueryCursor()
	defer cursor.Close()
	root := tree.inner.RootNode()
	it := cursor.Captures(q.inner, root, tree.src)
	var out []Capture
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("running query for %q: %w", q.lang, err)
		}
		match, idx := it.Next()
		if match == nil {
			break
		}
		capture := match.Captures[idx]
		node := capture.Node
		out = append(out, Capture{Name: q.names[capture.Index], Node: Node{inner: &node, src: tree.src}})
	}
	return out, nil
}
