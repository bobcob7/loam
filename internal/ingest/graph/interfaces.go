// Package graph implements loam-c94.5 and loam-c94.6: per file, extract the
// symbols it DEFINES and the references it MAKES (docs/ingestion-spec.md
// "Parse -> Graph (Tree-sitter)"), persist both into the
// symbols/symbol_references tables via internal/codegraph.Store's existing
// per-file delete-and-replace pair (ReplaceFileSymbols / ReplaceFileReferences),
// then resolve the whole repo+branch's references against symbols into
// graph_edges via one internal/codegraph.Store.RecomputeGraphEdges call after
// every file in the batch has landed -- this package adds no parallel write
// path of its own, and the resolution SQL itself lives entirely in
// internal/codegraph (landed by loam-54o.14); IngestFiles only decides WHEN
// to call it.
//
// This package is the whole parse -> graph track (loam-c94's dependency
// chain): it consumes internal/diffplan.Plan.ReparseFiles (already-decided
// file content, not re-derived here) and internal/parser's Tree-sitter
// bindings, hands its output to internal/codegraph.Store, and leaves
// graph_edges consistent with what it just wrote before returning --
// exactly the one call the swap orchestrator (loam-c94.12) needs to make
// for this track inside its transaction, immediately before or after the
// parallel chunk -> embed track's own writes, then COMMIT.
//
// RecomputeGraphEdges runs exactly ONCE per IngestFiles call, after the
// per-file loop, never per file and never skipped: "per-repo recompute"
// (this bead's own name) means the WHOLE (repoID, targetBranch) edge set is
// deleted and rebuilt from whatever symbols/symbol_references currently
// exist, regardless of whether the file batch itself was an incremental
// reparse (KindIncremental) or a full rebuild (KindFull) -- there is no
// separate incremental-edge-patch mode (Future Work per this bead's DESIGN);
// the incremental/full distinction only ever changes which files this call
// receives, never whether it recomputes edges afterward. Recomputing after
// an empty batch (e.g. an ingest whose plan had only deleted/renamed-file
// drops, applied by the orchestrator before calling this) is deliberate,
// not wasted work: those drops can shrink or shift the edge set as surely
// as a reparse can, and RecomputeGraphEdges is idempotent and correct
// either way.
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
// (internal/codegraph.Store.ReplaceFileSymbols / ReplaceFileReferences),
// plus RecomputeGraphEdges (loam-c94.6), the whole-repo edge rebuild
// IngestFiles calls once after the per-file pair has run for every file in
// the batch. Like codegraph.Store itself, this package never opens or
// commits a transaction -- store is expected to be constructed over the
// transaction the swap orchestrator (loam-c94.12) owns, so every write
// IngestFiles makes, symbols/references AND edges alike, is staged in that
// one transaction, not auto-committed.
type store interface {
	ReplaceFileSymbols(ctx context.Context, repoID uuid.UUID, targetBranch, file string, symbols []codegraph.SymbolInput) ([]codegraph.Symbol, error)
	ReplaceFileReferences(ctx context.Context, repoID uuid.UUID, targetBranch, file string, refs []codegraph.ReferenceInput) (int64, error)
	RecomputeGraphEdges(ctx context.Context, repoID uuid.UUID, targetBranch string) (int64, error)
}
