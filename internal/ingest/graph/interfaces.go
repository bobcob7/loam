// Package graph implements loam-c94.5: per file, extract the symbols it
// DEFINES and the references it MAKES (docs/ingestion-spec.md "Parse ->
// Graph (Tree-sitter)"), and persist both into the symbols/symbol_references
// tables via internal/codegraph.Store's existing per-file delete-and-replace
// pair (ReplaceFileSymbols / ReplaceFileReferences) -- this package adds no
// parallel write path of its own.
//
// This package is the head of the parse -> graph track (loam-c94's
// dependency chain): it consumes internal/diffplan.Plan.ReparseFiles
// (already-decided file content, not re-derived here) and internal/parser's
// Tree-sitter bindings, and hands its output to internal/codegraph.Store.
// References are stored UNRESOLVED (name + kind + line); resolving them
// against symbols into graph_edges is loam-c94.6's job, entirely in-DB, and
// is deliberately out of scope here.
//
// Scope note on DropFiles: per this bead's own DESIGN, "the orchestrator
// owns the deleted/renamed-file drops" (loam-c94.12) -- this package only
// ever processes diffplan.Plan.ReparseFiles (as FileInput values the caller
// has already read), never DropFiles. A file whose extension has no
// registered grammar is left alone entirely: no store call is made for it
// (docs/ingestion-spec.md "Files with no grammar are skipped for the
// graph"), so this package can never have written rows for it to begin
// with.
package graph

import (
	"context"

	"github.com/bobcob7/loam/internal/codegraph"
	"github.com/bobcob7/loam/internal/parser"
	"github.com/google/uuid"
)

//go:generate go tool moq -out moq_test.go . fileParser store

// fileParser is the subset of *parser.Parser / *parser.ParserPool this
// package needs: parsing pre-read file content, as a given Language, into a
// syntax tree. Defined here at the consumer (go-standards: "define
// interfaces where consumed") rather than depending on either concrete
// type, so this package's unit tests can moq it -- including backing the
// mock's ParseFunc with a real *parser.Parser call, which lets a test
// exercise this package's own extraction and error-handling logic against
// real Tree-sitter trees while still being free to swap in a canned error
// for paths that are effectively unreachable in production (e.g.
// parser.Parse's internal "no tree at all" failure).
type fileParser interface {
	Parse(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error)
}

// store is the subset of *codegraph.Store this package writes through: the
// per-file delete-and-replace pair for symbols and symbol_references
// (internal/codegraph.Store.ReplaceFileSymbols / ReplaceFileReferences).
// Like codegraph.Store itself, this package never opens or commits a
// transaction -- store is expected to be constructed over the transaction
// the swap orchestrator (loam-c94.12) owns, so every write IngestFiles
// makes is staged in that transaction, not auto-committed.
type store interface {
	ReplaceFileSymbols(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error)
	ReplaceFileReferences(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []codegraph.ReferenceInput) (int64, error)
}
