package graph

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/parser"
)

// Symbol/reference kind vocabulary this package writes. symbols.kind and
// symbol_references.kind are both open text columns (docs/persistence-spec.
// md "symbols": "kind (function/type/module/...)"), not a closed enum
// (unlike graph_edges.kind, which 0002_code_intel.up.sql DOES constrain via
// a CHECK) -- these are this package's own consistent choices within that
// open vocabulary, not the full set every future extractor must use.
const (
	// kindModule is the one file-level symbol ExtractFile emits per
	// successfully parsed file, Line always nil (docs/persistence-spec.md
	// "symbols": "line (null for file-level)"; codegraph.SymbolInput's own
	// doc comment).
	kindModule = "module"
	// kindFunction covers every callable declaration this package's
	// queries capture: a plain function, a Go method (receiver-bound), and
	// a TS/JS/Python class method alike. There is no separate "method"
	// kind: docs/persistence-spec.md's kind examples list "function", not
	// "method", and a receiver/class binding is not a fact any current
	// consumer (LookupSymbolsByName, ResolveGraphEdgeCandidates) needs to
	// distinguish.
	kindFunction = "function"
	// kindType covers a Go struct/interface type_spec, a TS class/
	// interface/type alias, a JS class, and a Python class.
	kindType = "type"
	// kindReferenceCall is the only reference kind ExtractFile produces:
	// every reference these queries capture is a call-expression's callee
	// name (see the per-language query source comments in queries.go).
	kindReferenceCall = "call"
)

// errNoSuchQuery is an internal precondition failure, not a caller-facing
// condition: a parser.Language reached ExtractFile without a compiled
// Query, which can only happen if querySources and internal/parser's own
// grammars map (parser.go) ever drift apart -- LanguageForPath only ever
// returns a Language this package's querySources also registers, by
// construction of the two maps sharing the same MVP grammar set (see
// querySources' doc comment).
var errNoSuchQuery = errors.New("graph: no extraction query compiled for language")

// FileInput is one file's content awaiting extraction: a path -- typically
// one entry of diffplan.Plan.ReparseFiles -- paired with its blob content
// at the plan's new_ref. This package never reads git itself, mirroring
// internal/diffplan's own "pure producer" boundary (diffplan.Planner's own
// doc comment): the caller (loam-c94.12) resolves and reads that content
// before calling IngestFiles.
type FileInput struct {
	Path    string
	Content []byte
}

// FileResult is what ExtractFile found for one file.
type FileResult struct {
	// Symbols always starts with exactly one file-level "module" symbol
	// (see kindModule) when ok is true, followed by every function/type
	// declaration the query found, in the order Tree-sitter's query cursor
	// produced them (not necessarily document order across different
	// patterns -- see parser.Match's own doc comment).
	Symbols []codegraph.SymbolInput
	// References is every call-site reference the query found.
	References []codegraph.ReferenceInput
	// HasSyntaxError reports whether the parsed tree contained at least
	// one ERROR/MISSING node (parser.Tree.HasError). Symbols/References
	// still reflect a genuine, best-effort extraction over whatever
	// well-formed constructs the tree does contain when this is true --
	// see ExtractFile's doc comment for why a broken construct simply
	// contributes nothing, rather than making extraction fail outright.
	HasSyntaxError bool
}

// ExtractFile parses content as path's Language (parser.LanguageForPath)
// and extracts the symbols it defines and the references it makes.
//
// ok is false, with a zero FileResult and a nil error, when path's
// extension has no registered Tree-sitter grammar (parser.ErrNoGrammar):
// docs/ingestion-spec.md "Files with no grammar are skipped for the graph"
// -- the caller (IngestFiles) counts this as a deliberate skip via
// Stats.FilesSkippedUnsupportedLanguage, never silently doing nothing with
// no visible trace.
//
// err non-nil (ok always false) means Tree-sitter produced no tree at all
// -- not the same as a tree containing syntax errors, see below. In
// practice this is only reachable via ctx's own cancellation/deadline
// (propagated wrapped) or parser.Parse's internal "no tree returned"
// precondition failure (parser.errParseFailed), since Tree-sitter is
// error-tolerant and returns a partial tree for ordinary broken source
// rather than failing Parse outright (internal/parser/parser_test.go's
// TestParse_SyntaxErrorIsReportedOnTreeAndNode pins exactly this: "Tree-
// sitter always returns a (partial) tree for broken input; the error is
// signaled via HasError, never through err"). The caller must not treat a
// non-ctx err as fatal to the whole batch (see IngestFiles' doc comment),
// but must also not paper over it by writing an empty symbol set for this
// file: doing so would assert "this file now defines nothing", which is
// actively wrong, not merely incomplete, so IngestFiles makes no store
// call at all for this file and leaves its existing rows exactly as they
// were.
//
// loam-1z0 (the graph-track half of loam-8uo) considered and rejected
// dropping this file's rows anyway to mirror the chunk track's binary-sniff
// fix: symbol_history references symbols(id) ON DELETE CASCADE, so dropping
// a file's symbols destroys its history once loam-c94.7 lands, and the
// trigger here is a transient TOOL fault (the parser itself failing), not a
// fact about the file's content the way a binary sniff is -- there is
// nothing true to assert about what the file now defines. The decision is
// to KEEP this behaviour: a file's rows can go stale for exactly as long as
// this failure keeps recurring across reparses of the same file -- a known,
// bounded staleness window that closes the next time the file parses
// successfully (or the file is dropped via a later diffplan.Plan.DropFiles,
// e.g. a rename or deletion), never an unbounded one.
// TestIngestFiles_HardParseFailure_LeavesExistingSymbolsRowsUntouched
// (ingest_test.go) pins this: a file's pre-existing symbols row set is
// asserted to survive, byte-for-byte, a batch in which that file's reparse
// hits this exact path. That test injects the failure at the fileParser
// seam rather than driving it with real source: Tree-sitter's error
// tolerance (see above) means this branch has no known real-input trigger
// in production -- parser.Parse's own "no tree returned" case is reachable
// only via ctx cancellation (which ExtractFiles handles separately, never
// reaching here) or its internal errParseFailed precondition failure, which
// this repo's grammars never actually return for real, even maximally
// broken or binary, source.
//
// A syntax error inside an otherwise-usable tree (tree.HasError) is
// different: Tree-sitter still produced real structure, so ExtractFile
// returns ok=true with FileResult.HasSyntaxError=true and whatever
// well-formed symbols/references the tree's clean subtrees do contain -- a
// broken construct simply fails to match this package's queries and
// contributes nothing, the same way any other non-matching syntax would
// (see TestExtractFile_PartialSyntaxError_StillExtractsCleanConstructs).
func (e *Extractor) ExtractFile(ctx context.Context, path string, content []byte) (result FileResult, ok bool, err error) {
	lang, err := parser.LanguageForPath(path)
	if err != nil {
		if errors.Is(err, parser.ErrNoGrammar) {
			return FileResult{}, false, nil
		}
		return FileResult{}, false, fmt.Errorf("resolving language for %s: %w", path, err)
	}
	query, found := e.queries[lang]
	if !found {
		return FileResult{}, false, fmt.Errorf("extracting %s: %w: %q", path, errNoSuchQuery, lang)
	}
	tree, err := e.parser.Parse(ctx, lang, content)
	if err != nil {
		return FileResult{}, false, fmt.Errorf("parsing %s: %w", path, err)
	}
	defer tree.Close()
	matches, err := query.Captures(ctx, tree)
	if err != nil {
		return FileResult{}, false, fmt.Errorf("extracting symbols from %s: %w", path, err)
	}
	result.HasSyntaxError = tree.HasError()
	result.Symbols = append(result.Symbols, moduleSymbol(path))
	for _, m := range matches {
		appendMatch(&result, m)
	}
	return result, true, nil
}

// moduleSymbol builds the one file-level "module" symbol every
// successfully parsed file gets: Line nil (file-level, per kindModule's
// doc comment), Name the file's base name without its extension --
// deliberately language-agnostic (no per-language package/module-path
// resolution) since no current consumer needs more than a stable,
// human-readable anchor for the file itself.
func moduleSymbol(path string) codegraph.SymbolInput {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	return codegraph.SymbolInput{Name: name, Kind: kindModule}
}

// appendMatch classifies one Match by which capture name its pattern
// produced (function.name / type.name / reference.name -- see queries.go's
// per-language query sources) and appends the corresponding
// SymbolInput/ReferenceInput to result. A Match's captures all come from
// the same pattern occurrence (parser.Match's own doc comment), so exactly
// one of the three cases fires per Match; line is always the capture
// node's own 1-indexed start row (Tree-sitter rows are 0-indexed --
// parser.Point's doc comment).
func appendMatch(result *FileResult, m parser.Match) {
	for _, c := range m.Captures {
		line := int32(c.Node.StartPoint().Row) + 1
		switch c.Name {
		case "function.name":
			result.Symbols = append(result.Symbols, codegraph.SymbolInput{Line: &line, Name: c.Node.Text(), Kind: kindFunction})
		case "type.name":
			result.Symbols = append(result.Symbols, codegraph.SymbolInput{Line: &line, Name: c.Node.Text(), Kind: kindType})
		case "reference.name":
			result.References = append(result.References, codegraph.ReferenceInput{Name: c.Node.Text(), Kind: kindReferenceCall, Line: line})
		}
	}
}
