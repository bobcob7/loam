package graph

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/codegraph"
)

// Stats reports what one IngestFiles call did across a batch of files, so a
// caller (eventually loam-c94.13's ingest_jobs.stats writer) can log and
// persist per-ingest counts rather than only a pass/fail bit. Every file in
// the input batch is accounted for in exactly one of the four counters
// below plus, independently, FilesWithSyntaxErrors (a non-exclusive subset
// of FilesExtracted -- see its own doc comment).
type Stats struct {
	// FilesExtracted is how many files were successfully parsed (ok=true
	// from ExtractFile) and had their symbols/references written.
	FilesExtracted int
	// FilesWithSyntaxErrors is how many of FilesExtracted parsed with at
	// least one syntax error (FileResult.HasSyntaxError) -- extraction
	// still ran and wrote a best-effort result for these; this counter is
	// purely for operator visibility, not a failure count.
	FilesWithSyntaxErrors int
	// FilesSkippedUnsupportedLanguage is how many files had no registered
	// Tree-sitter grammar for their extension (docs/ingestion-spec.md
	// "Files with no grammar are skipped for the graph") -- skipped and
	// counted, never silently dropped with no trace.
	FilesSkippedUnsupportedLanguage int
	// FilesFailed is how many files had no usable extraction result: a
	// registered grammar that could not actually parse into any tree, or a
	// Captures query failing over a tree that DID parse (see ExtractFile's
	// doc comment for both sub-cases and how reachable each is in
	// practice). No store call was made for these files; their existing
	// rows, if any, are left untouched -- a deliberate, known, and bounded
	// staleness window (loam-1z0), not an oversight.
	FilesFailed int
	// SymbolsWritten and ReferencesWritten are the total row counts
	// inserted across every successfully extracted file (the sum of each
	// ReplaceFileSymbols/ReplaceFileReferences call's input length, which
	// equals the row count each call inserts).
	SymbolsWritten    int
	ReferencesWritten int
	// EdgesRecomputed is the graph_edges row count RecomputeGraphEdges
	// inserted for (repoID, targetBranch) after this batch's per-file writes
	// landed (loam-c94.6) -- the whole repo+branch's edge set, not just
	// edges touched by this batch's files, since RecomputeGraphEdges always
	// rebuilds from scratch (see this file's IngestFiles doc comment).
	EdgesRecomputed int64
}

// IngestFiles extracts and persists symbols/references for each of files
// (typically diffplan.Plan.ReparseFiles paired with content read at
// new_ref) via st's per-file delete-and-replace pair (codegraph.Store.
// ReplaceFileSymbols / ReplaceFileReferences), then resolves graph_edges for
// the WHOLE (repoID, targetBranch) via one st.RecomputeGraphEdges call
// (loam-c94.6) once every file's writes have landed. st is expected to be
// constructed over the transaction the swap orchestrator (loam-c94.12)
// owns -- IngestFiles opens no transaction and commits nothing itself,
// mirroring codegraph.Store's own transactional-scope contract.
//
// Per-file delete-then-insert ordering is already guaranteed by
// ReplaceFileSymbols/ReplaceFileReferences themselves (each deletes the
// file's existing rows, then inserts the fresh set, as two sequential
// calls against the same querier before returning -- internal/codegraph/
// store.go) -- IngestFiles does not need to, and does not, add any
// ordering of its own beyond calling them. RecomputeGraphEdges runs AFTER
// the per-file loop completes, not per file and not interleaved with it:
// resolving edges against a partially-written batch would miss symbols the
// later files in the same batch are about to define, exactly the "unchanged
// file references a symbol that moved" case this bead exists to get right.
//
// A single file's extraction trouble never aborts the batch:
//   - An unsupported language (ExtractFile's ok=false, err=nil) is counted
//     in Stats.FilesSkippedUnsupportedLanguage and makes no store call.
//   - A hard parse failure (ok=false, err!=nil, not a ctx error) is logged,
//     counted in Stats.FilesFailed, and also makes no store call -- see
//     ExtractFile's doc comment for why leaving the file's existing rows
//     untouched is safer than asserting it now defines nothing.
//   - A syntax error inside an otherwise-usable tree (FileResult.
//     HasSyntaxError) still writes its best-effort symbols/references and
//     is only counted (Stats.FilesWithSyntaxErrors), never treated as a
//     failure.
//
// Three things stop the batch (and skip RecomputeGraphEdges) outright, all
// returned wrapped and immediately: ctx's own error (checked before each
// file and once more before the recompute call, so an already-canceled ctx
// short-circuits even an empty files batch; also surfacing from
// ExtractFile/store calls that observe it mid-flight), a per-file store
// write failing, and RecomputeGraphEdges itself failing -- in every case the
// enclosing transaction is going to roll back regardless, so there is
// nothing to gain from continuing.
func (e *Extractor) IngestFiles(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, files []FileInput) (Stats, error) {
	extracted, stats, err := e.ExtractFiles(ctx, files)
	if err != nil {
		return stats, err
	}
	writeStats, err := e.PersistFiles(ctx, st, repoID, targetBranch, extracted)
	stats.merge(writeStats)
	return stats, err
}

// Extracted is one batch's parse work, already done: per file, the symbols
// and references extraction found, ready to be written with no further
// Tree-sitter work. It is deliberately opaque -- a caller obtains one only
// from ExtractFiles and can only spend it on PersistFiles -- so the
// per-file skip policy (unsupported language and hard parse failures never
// produce an entry, and therefore never produce a store call) cannot be
// bypassed by hand-assembling one.
type Extracted struct {
	files []extractedFile
}

// extractedFile is one successfully parsed file's share of an Extracted.
type extractedFile struct {
	path       string
	symbols    []codegraph.SymbolInput
	references []codegraph.ReferenceInput
}

// Files reports how many files this Extracted will write, so the swap
// orchestrator (loam-c94.12) can log the size of the write phase it is
// about to enter without Extracted having to leak its contents.
func (e Extracted) Files() int { return len(e.files) }

// ExtractFiles does the whole TREE-SITTER half of IngestFiles and none of
// the database half: it parses and extracts every file in files, applying
// exactly the per-file resilience policy IngestFiles documents, and
// returns an Extracted that PersistFiles can write with no further
// parsing.
//
// It exists as its own entry point because the swap orchestrator
// (loam-c94.12) must do its heavy compute BEFORE opening the transaction
// that its store is bound to, and must run this track's compute
// concurrently with the chunk->embed track's -- neither of which is
// possible through IngestFiles, which interleaves parsing with store
// writes file by file and so pins all of it inside whatever transaction
// the store belongs to. A pgx.Tx is not goroutine-safe, so anything that
// touches the transaction cannot run in parallel with anything else that
// does; separating the phases is what makes the parallelism the design
// asks for legal at all.
//
// Only ctx's own error stops the batch (checked before each file, and
// again from any ExtractFile call that observes it mid-flight), returned
// wrapped immediately.
func (e *Extractor) ExtractFiles(ctx context.Context, files []FileInput) (Extracted, Stats, error) {
	var stats Stats
	extracted := Extracted{files: make([]extractedFile, 0, len(files))}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return Extracted{}, stats, fmt.Errorf("ingesting %s: %w", f.Path, err)
		}
		result, ok, err := e.ExtractFile(ctx, f.Path, f.Content)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Extracted{}, stats, fmt.Errorf("ingesting %s: %w", f.Path, err)
			}
			stats.FilesFailed++
			e.logger.ErrorContext(ctx, "skipping file after parse failure", "file", f.Path, "error", err)
			continue
		}
		if !ok {
			stats.FilesSkippedUnsupportedLanguage++
			continue
		}
		if result.HasSyntaxError {
			stats.FilesWithSyntaxErrors++
			e.logger.WarnContext(ctx, "file parsed with syntax errors; extraction is best-effort", "file", f.Path)
		}
		extracted.files = append(extracted.files, extractedFile{path: f.Path, symbols: result.Symbols, references: result.References})
		stats.FilesExtracted++
		stats.SymbolsWritten += len(result.Symbols)
		stats.ReferencesWritten += len(result.References)
	}
	return extracted, stats, nil
}

// PersistFiles does the whole DATABASE half of IngestFiles and none of the
// parsing half: st's per-file delete-and-replace pair for every file in
// ex, in the order ExtractFiles received them, then exactly one
// st.RecomputeGraphEdges for the whole (repoID, targetBranch) once they
// have all landed. This is the only part of this package that belongs
// inside the swap orchestrator's transaction (see ExtractFiles's doc
// comment).
//
// st is expected to be constructed over the transaction the orchestrator
// owns: PersistFiles opens no transaction and commits nothing.
//
// The final ctx.Err() check before RecomputeGraphEdges guards the
// empty-batch path, where the per-file loop never runs at all -- an
// already-canceled context must abort rather than reach the recompute.
//
// # Why this track needs no SAVEPOINT (loam-c94.24, established not assumed)
//
// The sibling chunk track wraps every store call in a savepoint, because
// its own loop (internal/ingest/vectors.Persist) SKIPS a rejected file and
// keeps going: without a savepoint, Postgres had already aborted the
// shared transaction those later writes were landing in, so the loop's
// verdict ("one file lost") and the commit's ("everything lost")
// disagreed, and the commit won.
//
// No such gap exists here, and the reason is structural rather than
// fortunate. Every per-file tolerance this package has -- unsupported
// language, hard parse failure, syntax errors -- lives in ExtractFiles,
// which runs BEFORE the transaction is even open and makes no store call
// at all for the files it skips. The loop below, the only part inside the
// transaction, has no `continue`: the first ReplaceFileSymbols,
// ReplaceFileReferences or RecomputeGraphEdges error returns, the swap
// orchestrator rolls the whole transaction back, and nothing was staged
// past the failure to be surprised by. Loop and commit agree, so a
// savepoint would isolate a failure nothing continues past. Adding one
// would ALSO be a behaviour change, not a safety net: it would only start
// mattering if this loop grew a skip-and-continue policy, and that is the
// change that should carry it (docs/ingestion-spec.md "Consistency &
// Failure" records the resulting asymmetry as intended).
//
// The ORDER dependency runs the other way and is worth stating: the swap
// runs this track's writes before the chunk track's, and ROLLBACK TO
// SAVEPOINT only unwinds statements issued after its savepoint, so a
// chunk-file rejection cannot disturb anything written here.
func (e *Extractor) PersistFiles(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, ex Extracted) (Stats, error) {
	var stats Stats
	for _, f := range ex.files {
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("ingesting %s: %w", f.path, err)
		}
		if _, err := st.ReplaceFileSymbols(ctx, repoID, targetBranch, f.path, f.symbols); err != nil {
			return stats, fmt.Errorf("replacing symbols for %s: %w", f.path, err)
		}
		if _, err := st.ReplaceFileReferences(ctx, repoID, targetBranch, f.path, f.references); err != nil {
			return stats, fmt.Errorf("replacing references for %s: %w", f.path, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return stats, fmt.Errorf("recomputing graph edges for %s@%s: %w", repoID, targetBranch, err)
	}
	edgeCount, err := st.RecomputeGraphEdges(ctx, repoID, targetBranch)
	if err != nil {
		return stats, fmt.Errorf("recomputing graph edges for %s@%s: %w", repoID, targetBranch, err)
	}
	stats.EdgesRecomputed = edgeCount
	return stats, nil
}

// merge folds other's write-phase counters into s, which already carries
// the extract-phase ones. ExtractFiles and PersistFiles populate disjoint
// sets of Stats fields, so this is an addition, never an overwrite.
func (s *Stats) merge(other Stats) {
	s.FilesExtracted += other.FilesExtracted
	s.FilesWithSyntaxErrors += other.FilesWithSyntaxErrors
	s.FilesSkippedUnsupportedLanguage += other.FilesSkippedUnsupportedLanguage
	s.FilesFailed += other.FilesFailed
	s.SymbolsWritten += other.SymbolsWritten
	s.ReferencesWritten += other.ReferencesWritten
	s.EdgesRecomputed += other.EdgesRecomputed
}
