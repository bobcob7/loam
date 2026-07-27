package chunker

import (
	"regexp"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/parser"
)

// declKinds maps each supported Language to the set of node kinds that mark
// a top-level chunkable declaration -- one chunk per function/method/
// type/class, not one per statement. Verified empirically against real
// fixture/testdata content (internal/testfixture's fixture-polyglot and
// internal/parser/testdata's sample files) via each node's ToSexp() output,
// the same technique internal/ingest/graph's own query sources cite as
// their verification method:
//   - Go: function_declaration, method_declaration (both distinct node
//     kinds -- internal/parser/parser_test.go's
//     TestQuery_MethodDeclarationCapturesGoReceiverMethod already pins
//     this), and type_declaration (the outer node that wraps one or more
//     type_spec children -- chunking at type_declaration, not type_spec,
//     keeps a `type ( ... )` block as one chunk rather than splitting each
//     spec inside it).
//   - Python: function_definition covers both a module-level function and
//     (nested inside a class_definition's body) a method -- tree-sitter-
//     python does not distinguish the two with a separate node kind, same
//     fact internal/ingest/graph's pythonQuerySource comment already
//     established. class_definition is the type analogue.
//   - TypeScript/TSX: function_declaration, class_declaration,
//     interface_declaration, type_alias_declaration -- shared verbatim
//     with LanguageTSX for the same reason internal/ingest/graph's
//     typescriptQuerySource is: tree-sitter-tsx's grammar is
//     tree-sitter-typescript's plus JSX support, so every node kind here
//     resolves identically in both.
//   - JavaScript: typescriptQuerySource's subset, minus
//     interface_declaration/type_alias_declaration, which
//     tree-sitter-javascript's grammar does not define at all (a query
//     naming them fails to compile against it -- internal/ingest/graph's
//     javascriptQuerySource comment).
var declKinds = map[parser.Language]map[string]struct{}{
	parser.LanguageGo:         {"function_declaration": {}, "method_declaration": {}, "type_declaration": {}},
	parser.LanguagePython:     {"function_definition": {}, "class_definition": {}},
	parser.LanguageTypeScript: {"function_declaration": {}, "class_declaration": {}, "interface_declaration": {}, "type_alias_declaration": {}},
	parser.LanguageTSX:        {"function_declaration": {}, "class_declaration": {}, "interface_declaration": {}, "type_alias_declaration": {}},
	parser.LanguageJavaScript: {"function_declaration": {}, "class_declaration": {}},
}

// isDeclKind reports whether kind is a top-level chunkable declaration for
// lang.
func isDeclKind(lang parser.Language, kind string) bool {
	kinds, ok := declKinds[lang]
	if !ok {
		return false
	}
	_, found := kinds[kind]
	return found
}

// declarationSpan reports the node whose line range should become one
// symbol chunk for a root-level child node, or ok=false if node is not a
// chunkable declaration (a package clause, an import, a floating comment,
// a top-level var/const/expression statement -- none of these carry a
// symbol identity of their own, so none gets a chunk; the file's symbol
// chunks cover only its declarations, not full-file coverage).
//
// TypeScript/JavaScript wrap an exported declaration in an export_statement
// node (`export function Foo() {}` parses as export_statement ->
// declaration: function_declaration, verified empirically against
// fixture-polyglot's src/validate.ts and src/index.ts, both of which
// export their one top-level function this way). Chunking at the
// function_declaration alone would silently drop the "export " keyword
// from the chunk's content, so declarationSpan returns the export_statement
// itself -- the whole node whose span actually starts at "export" -- once
// it confirms the wrapped declaration is one of lang's chunkable kinds.
// `export default identifier;` / `export { a, b };` shaped export_statement
// nodes have no "declaration" field (or one that resolves to something
// other than a declaration kind) and are left un-chunked, same as any other
// non-declaration top-level node.
func declarationSpan(lang parser.Language, node parser.Node) (parser.Node, bool) {
	if node.Kind() == "export_statement" {
		decl, has := node.ChildByFieldName("declaration")
		if has && isDeclKind(lang, decl.Kind()) {
			return node, true
		}
		return parser.Node{}, false
	}
	if isDeclKind(lang, node.Kind()) {
		return node, true
	}
	return parser.Node{}, false
}

// leadingCommentStart walks span's preceding named siblings, extending
// backward through every contiguous "comment" node (no gap line between
// one comment and the next, or between the last comment and span itself),
// and returns the earliest such node -- span itself if there is no
// preceding comment to attach.
//
// This exists because a symbol's doc comment is a syntactically separate
// sibling node, not part of the declaration node's own span -- Go emits
// one "comment" node per "//" line (so a multi-line Go doc comment is
// several contiguous sibling nodes), while TypeScript/JavaScript emit one
// "comment" node for a whole "/** ... */" block. Both shapes are handled
// by the same walk: it does not care how many comment nodes precede span,
// only that each is directly adjacent (endpoint row + 1 == next node's
// start row) with no blank line breaking the chain, which is exactly the
// heuristic that keeps a file-level doc comment (separated from the first
// declaration by a package clause or import, hence not adjacent) from
// being swept into that declaration's chunk. This is precisely the
// navigation internal/parser.Node.NextSibling's own doc comment anticipates
// for this package: "Chunking a symbol together with a doc comment that
// precedes it as a separate sibling node ... is reached through
// PrevSibling/PrevNamedSibling, not through Parent's children."
func leadingCommentStart(span parser.Node) parser.Node {
	start := span
	for {
		prev, ok := start.PrevNamedSibling()
		if !ok || prev.Kind() != "comment" {
			return start
		}
		if prev.EndPoint().Row+1 != start.StartPoint().Row {
			return start
		}
		start = prev
	}
}

// chunkSymbols returns one chunk.Unit per top-level declaration in tree
// (see declKinds/declarationSpan), each extended backward to include its
// contiguous leading comment if any (leadingCommentStart). Only tree's
// direct, named root-level children are considered -- a nested declaration
// (a Python method inside a class_definition, a closure) is not itself
// walked into or chunked separately; it is covered by its enclosing
// top-level declaration's own span instead, exactly as the DESIGN section
// of loam-c94.10 intends ("one chunk per top-level symbol"), and the same
// reason a large class becomes one large chunk (subject to EnforceBudget
// splitting it, same as any other oversized unit) rather than one chunk
// per method.
func chunkSymbols(lang parser.Language, tree *parser.Tree, lines []string) []chunk.Unit {
	root := tree.RootNode()
	units := make([]chunk.Unit, 0, root.NamedChildCount())
	for i := 0; i < root.NamedChildCount(); i++ {
		child, ok := root.NamedChild(i)
		if !ok {
			continue
		}
		span, ok := declarationSpan(lang, child)
		if !ok {
			continue
		}
		start := leadingCommentStart(span)
		startLine := int(start.StartPoint().Row) + 1
		endLine := int(span.EndPoint().Row) + 1
		unit, ok := unitForLines(lines, startLine, endLine)
		if !ok {
			continue
		}
		units = append(units, unit)
	}
	return units
}

// atxHeadingRe matches an ATX-style markdown heading line ("#" through
// "######" followed by at least one space and non-space content). Setext
// headings (a line of text underlined with "===" or "---") are
// deliberately not recognized: fixture-polyglot's own docs/OVERVIEW.md
// (this strategy's golden fixture) uses ATX headings exclusively, ATX is
// the form both GitHub-flavored and CommonMark docs overwhelmingly default
// to, and setext detection would require one line of lookahead this
// single-pass line scan does not otherwise need.
var atxHeadingRe = regexp.MustCompile(`^#{1,6}[ \t]+\S`)

// chunkMarkdownSections splits lines on ATX headings: one chunk per
// heading (from that heading's own line through the line before the next
// heading, or end of file), plus one leading chunk for any non-blank
// content before the first heading, if the file has one. A file with no
// headings at all becomes a single chunk covering the whole file --
// docs-by-section still applies (the file is still markdown), it simply
// has exactly one section.
//
// An oversized section (one alone larger than the embedding model's
// budget) is not given any special handling here: chunkRaw always hands
// this strategy's output to chunk.EnforceBudget, which splits any
// too-large unit regardless of which strategy produced it -- the same
// backstop an oversized function or sliding-window chunk relies on, so a
// markdown section is not a special case at the chunking-strategy level at
// all.
func chunkMarkdownSections(lines []string) []chunk.Unit {
	var headings []int
	for i, line := range lines {
		if atxHeadingRe.MatchString(line) {
			headings = append(headings, i)
		}
	}
	if len(headings) == 0 {
		unit, ok := unitForLines(lines, 1, len(lines))
		if !ok {
			return nil
		}
		return []chunk.Unit{unit}
	}
	units := make([]chunk.Unit, 0, len(headings)+1)
	if headings[0] > 0 {
		if unit, ok := unitForLines(lines, 1, headings[0]); ok {
			units = append(units, unit)
		}
	}
	for i, h := range headings {
		end := len(lines)
		if i+1 < len(headings) {
			end = headings[i+1]
		}
		if unit, ok := unitForLines(lines, h+1, end); ok {
			units = append(units, unit)
		}
	}
	return units
}

// slidingWindowLines and slidingWindowOverlapLines size the fallback
// strategy for a file with neither a registered grammar nor a markdown
// extension -- plain text with no structure this package knows how to
// split semantically.
//
// 100 lines is sized to land in the same regime as a real symbol chunk
// rather than a full file: internal/ingest/chunk's own budget analysis
// (TokenBudgetChars's doc comment) measured this repository's own Go
// functions at a 38.8 bytes/line mean line length and a p99 function size
// of 2001 bytes -- at that density, 100 lines is roughly 3880 bytes,
// comfortably under the 4096-byte budget a 2048-token model yields
// (TokenBudgetChars(2048)) without leaning on EnforceBudget's splitting in
// the common case, while still being close to double a typical large
// function so a plain-text window is not needlessly more fragmented than
// the code chunks it sits alongside in the index.
//
// 20 lines of overlap (20% of the window, so consecutive windows advance
// by an 80-line stride) exists so a paragraph or logical unit straddling a
// window boundary still appears whole in at least one window -- the same
// justification a symbol chunk gets for free from following real syntax
// boundaries, approximated here since a plain-text file has none. 20 lines
// is well above any paragraph or list this package is likely to split
// (most run a handful of lines), so the "still findable in one window"
// property holds in the common case, while staying far short of a
// 50%-overlap scheme that would roughly double the number of indexed
// chunks -- and therefore roughly double embedding cost and search-result
// duplication -- for a fallback path that is, per the budget analysis
// above, meant to be the rare case, not the common one.
const (
	slidingWindowLines        = 100
	slidingWindowOverlapLines = 20
)

// chunkSlidingWindow splits lines into overlapping fixed-size windows (see
// slidingWindowLines/slidingWindowOverlapLines). A file with fewer lines
// than one window becomes a single chunk covering the whole file; the
// final window is clipped to the file's last line rather than reading past
// it, so the last two windows can overlap by more than
// slidingWindowOverlapLines when the file's length is not an exact
// multiple of the stride -- an accepted, minor deviation from the nominal
// overlap, never a gap.
func chunkSlidingWindow(lines []string) []chunk.Unit {
	total := len(lines)
	if total == 0 {
		return nil
	}
	stride := slidingWindowLines - slidingWindowOverlapLines
	var units []chunk.Unit
	for start := 0; start < total; start += stride {
		end := start + slidingWindowLines
		if end > total {
			end = total
		}
		if unit, ok := unitForLines(lines, start+1, end); ok {
			units = append(units, unit)
		}
		if end == total {
			break
		}
	}
	return units
}
