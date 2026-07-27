package graph

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
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
	// FilesFailed is how many files a registered grammar could not
	// actually parse into any tree (see ExtractFile's doc comment for how
	// rare this is in practice). No store call was made for these files;
	// their existing rows, if any, are left untouched.
	FilesFailed int
	// SymbolsWritten and ReferencesWritten are the total row counts
	// inserted across every successfully extracted file (the sum of each
	// ReplaceFileSymbols/ReplaceFileReferences call's input length, which
	// equals the row count each call inserts).
	SymbolsWritten    int
	ReferencesWritten int
}

// IngestFiles extracts and persists symbols/references for each of files
// (typically diffplan.Plan.ReparseFiles paired with content read at
// new_ref) via st's per-file delete-and-replace pair (codegraph.Store.
// ReplaceFileSymbols / ReplaceFileReferences). st is expected to be
// constructed over the transaction the swap orchestrator (loam-c94.12)
// owns -- IngestFiles opens no transaction and commits nothing itself,
// mirroring codegraph.Store's own transactional-scope contract.
//
// Per-file delete-then-insert ordering is already guaranteed by
// ReplaceFileSymbols/ReplaceFileReferences themselves (each deletes the
// file's existing rows, then inserts the fresh set, as two sequential
// calls against the same querier before returning -- internal/codegraph/
// store.go) -- IngestFiles does not need to, and does not, add any
// ordering of its own beyond calling them.
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
// Only two things stop the batch outright, both returned wrapped and
// immediately: ctx's own error (checked before each file, and surfacing
// again from ExtractFile/store calls that observe it mid-flight), and a
// store write failing -- the enclosing transaction is going to roll back
// regardless once one write fails, so there is nothing to gain from
// continuing to process the rest of files.
func (e *Extractor) IngestFiles(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, files []FileInput) (Stats, error) {
	var stats Stats
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("ingesting %s: %w", f.Path, err)
		}
		result, ok, err := e.ExtractFile(ctx, f.Path, f.Content)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return stats, fmt.Errorf("ingesting %s: %w", f.Path, err)
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
		if _, err := st.ReplaceFileSymbols(ctx, repoID, targetBranch, f.Path, result.Symbols); err != nil {
			return stats, fmt.Errorf("replacing symbols for %s: %w", f.Path, err)
		}
		if _, err := st.ReplaceFileReferences(ctx, repoID, targetBranch, f.Path, result.References); err != nil {
			return stats, fmt.Errorf("replacing references for %s: %w", f.Path, err)
		}
		stats.FilesExtracted++
		stats.SymbolsWritten += len(result.Symbols)
		stats.ReferencesWritten += len(result.References)
	}
	return stats, nil
}
