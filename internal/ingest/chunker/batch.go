package chunker

import (
	"context"
	"errors"
	"fmt"

	"github.com/bobcob7/loam/internal/ingest/chunk"
)

// FileInput is one file's content awaiting chunking: a path -- typically
// one entry of diffplan.Plan.ReparseFiles -- paired with its blob content
// at the plan's new_ref. Mirrors internal/ingest/graph.FileInput: this
// package never reads git itself, the caller (loam-c94.12) resolves and
// reads content before calling ChunkFiles.
type FileInput struct {
	Path    string
	Content []byte
}

// FileChunks is one file's chunk output: chunk.Unit carries no file
// identifier of its own (see its doc comment), so a batch result pairs
// each file's units with its path explicitly.
type FileChunks struct {
	Path  string
	Units []chunk.Unit
}

// Stats reports what one ChunkFiles call did across a batch of files,
// mirroring internal/ingest/graph.Stats's shape so a caller (eventually
// loam-c94.12/loam-c94.13) can log and persist per-ingest counts the same
// way for both tracks of the pipeline.
type Stats struct {
	// FilesChunked is how many files were chunked (ok=true from
	// ChunkFile), including a file that ended up with zero units (e.g. an
	// empty file, or a source file containing only imports and no
	// declarations).
	FilesChunked int
	// FilesSkippedBinary is how many files were skipped because they
	// looked binary (isBinary) -- skipped and counted, never silently
	// dropped with no trace, matching docs/ingestion-spec.md "Binary/
	// non-text files are skipped."
	FilesSkippedBinary int
	// FilesFailed is how many files a registered grammar could not
	// actually parse into any tree at all (see ChunkFile's doc comment for
	// how rare this is in practice).
	FilesFailed int
	// UnitsProduced is the total unit count across every chunked file,
	// after EnforceBudget.
	UnitsProduced int
	// UnitsSplit and PiecesFromSplit sum every chunked file's
	// chunk.Result, the same visibility EnforceBudget itself provides
	// per-file.
	UnitsSplit      int
	PiecesFromSplit int
}

// ChunkFiles chunks each of files via ChunkFile, tolerating any single
// file's trouble rather than aborting the whole batch -- mirroring
// internal/ingest/graph.IngestFiles's own per-file resilience:
//   - A binary file (ChunkFile's ok=false) is counted in
//     Stats.FilesSkippedBinary.
//   - A hard parse failure (ChunkFile's err!=nil, not a ctx error) is
//     logged and counted in Stats.FilesFailed.
//
// EVERY file in files gets exactly one FileChunks entry in the returned
// slice, in input order, INCLUDING a skipped-binary or failed one -- that
// entry simply carries zero units (loam-8uo). Omitting them, as this
// function originally did, is a silent index-corruption bug: a file that
// WAS text and BECOMES binary is in the plan's reparse set and therefore
// not in its DropFiles, so if it never reaches internal/ingest/vectors
// its previous chunks are never deleted and keep matching RAG search,
// indefinitely, for content the file no longer has. A zero-unit entry is
// exactly what vectors.IngestFileChunks already handles correctly -- it
// still issues the file's ReplaceFileChunks call, with an empty inputs
// slice, precisely so the stale chunks are dropped (pinned by that
// package's TestIngestFileChunks_FileWithNoUnits_StillReplacesToDropStale
// Chunks) -- so the drop path that already exists does the work and the
// orchestrator needs no reparse-set-minus-output diffing of its own.
//
// Stats therefore still discriminates the three outcomes; the OUTPUT
// deliberately does not, because every one of them means the same thing
// downstream: "this file's chunks, whatever they were, must be replaced by
// these (possibly zero) units."
//
// Only ctx's own error stops the batch outright, checked before each file
// and surfacing again from any ChunkFile call that observes it mid-flight,
// returned wrapped immediately.
func (c *Chunker) ChunkFiles(ctx context.Context, files []FileInput, budgeter Budgeter) ([]FileChunks, Stats, error) {
	var stats Stats
	out := make([]FileChunks, 0, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return out, stats, fmt.Errorf("chunking %s: %w", f.Path, err)
		}
		units, result, ok, err := c.ChunkFile(ctx, f.Path, f.Content, budgeter)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return out, stats, fmt.Errorf("chunking %s: %w", f.Path, err)
			}
			stats.FilesFailed++
			c.logger.ErrorContext(ctx, "dropping chunks for file that failed to chunk", "file", f.Path, "error", err)
			out = append(out, FileChunks{Path: f.Path})
			continue
		}
		if !ok {
			stats.FilesSkippedBinary++
			c.logger.InfoContext(ctx, "dropping chunks for file that is now binary", "file", f.Path)
			out = append(out, FileChunks{Path: f.Path})
			continue
		}
		stats.FilesChunked++
		stats.UnitsProduced += len(units)
		stats.UnitsSplit += result.UnitsSplit
		stats.PiecesFromSplit += result.PiecesProduced
		out = append(out, FileChunks{Path: f.Path, Units: units})
	}
	return out, stats, nil
}
