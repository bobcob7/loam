// Package parser is the only place in this repository that uses cgo. It
// wraps Tree-sitter's C library behind a small, pure-Go-facing surface:
// callers get a Language lookup, a Parse call, and a minimal Tree/Node view
// with just enough of the syntax tree (kind, byte/line ranges, source text,
// child/field navigation) for the symbol extractor and the chunker to do
// their work. The underlying Tree-sitter tree, node, and language types
// never leak past this package.
package parser

import ts "github.com/tree-sitter/go-tree-sitter"

// Point is a zero-indexed row/column position in a source file.
type Point struct {
	Row    uint
	Column uint
}

func pointFrom(p ts.Point) Point {
	return Point{Row: p.Row, Column: p.Column}
}

// Tree is a parsed syntax tree for one file. It owns Tree-sitter's
// underlying C memory and must be released with Close once the caller is
// done reading it.
type Tree struct {
	inner *ts.Tree
	src   []byte
	lang  Language
}

// Language reports which grammar produced this tree.
func (t *Tree) Language() Language {
	return t.lang
}

// RootNode returns the tree's root node.
func (t *Tree) RootNode() Node {
	return Node{inner: t.inner.RootNode(), src: t.src}
}

// HasError reports whether the tree contains any syntax error, including
// missing or unexpected tokens. Callers can use this to decide whether to
// trust the parse for symbol extraction.
func (t *Tree) HasError() bool {
	return t.inner.RootNode().HasError()
}

// Close releases the tree's underlying C memory. Safe to call once; it must
// be called for every Tree returned by Parse.
func (t *Tree) Close() {
	t.inner.Close()
}

// Node is a read-only view onto one node of a parsed Tree. It is a thin
// wrapper: it never leaks the underlying Tree-sitter node type, and it stays
// valid only as long as the owning Tree has not been Closed.
type Node struct {
	inner *ts.Node
	src   []byte
}

// Kind returns the grammar's node type name, e.g. "function_declaration".
func (n Node) Kind() string {
	return n.inner.Kind()
}

// IsNamed reports whether this is a named node (as opposed to an anonymous
// token like a punctuation mark).
func (n Node) IsNamed() bool {
	return n.inner.IsNamed()
}

// IsError reports whether this node represents a syntax error.
func (n Node) IsError() bool {
	return n.inner.IsError()
}

// IsMissing reports whether this node was inserted by the parser's error
// recovery to stand in for a token it expected but did not find.
func (n Node) IsMissing() bool {
	return n.inner.IsMissing()
}

// ByteRange returns the [start, end) byte offsets of this node in the
// source that produced it.
func (n Node) ByteRange() (start, end uint) {
	return n.inner.StartByte(), n.inner.EndByte()
}

// StartPoint returns the node's zero-indexed starting row/column.
func (n Node) StartPoint() Point {
	return pointFrom(n.inner.StartPosition())
}

// EndPoint returns the node's zero-indexed ending row/column.
func (n Node) EndPoint() Point {
	return pointFrom(n.inner.EndPosition())
}

// Text returns the source text this node spans.
func (n Node) Text() string {
	return n.inner.Utf8Text(n.src)
}

// ChildCount returns the number of children, named and anonymous.
func (n Node) ChildCount() int {
	return int(n.inner.ChildCount())
}

// Child returns the i'th child, and false if there is none at that index.
func (n Node) Child(i int) (Node, bool) {
	c := n.inner.Child(uint(i))
	if c == nil {
		return Node{}, false
	}
	return Node{inner: c, src: n.src}, true
}

// NamedChildCount returns the number of named children (anonymous tokens
// such as punctuation are excluded).
func (n Node) NamedChildCount() int {
	return int(n.inner.NamedChildCount())
}

// NamedChild returns the i'th named child, and false if there is none at
// that index.
func (n Node) NamedChild(i int) (Node, bool) {
	c := n.inner.NamedChild(uint(i))
	if c == nil {
		return Node{}, false
	}
	return Node{inner: c, src: n.src}, true
}

// ChildByFieldName returns the child bound to the given grammar field (e.g.
// "name" on a function_declaration), and false if the field is absent.
func (n Node) ChildByFieldName(name string) (Node, bool) {
	c := n.inner.ChildByFieldName(name)
	if c == nil {
		return Node{}, false
	}
	return Node{inner: c, src: n.src}, true
}

// Parent returns this node's parent, and false at the root.
func (n Node) Parent() (Node, bool) {
	p := n.inner.Parent()
	if p == nil {
		return Node{}, false
	}
	return Node{inner: p, src: n.src}, true
}
