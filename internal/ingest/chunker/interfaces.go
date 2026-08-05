// Package chunker implements loam-c94.10: for each changed file, produce the
// RAG chunk units (docs/ingestion-spec.md "Chunk -> Embed -> Vectors") that
// loam-c94.11 will embed and persist. The strategy is chosen per file:
//
//   - A file whose extension has a registered Tree-sitter grammar
//     (internal/parser.LanguageForPath) is chunked by top-level symbol --
//     one chunk per function/method/type/class declaration, reusing the
//     same parse loam-c94.5's graph extractor runs, so the two tracks stay
//     independent (each opens its own Tree) but share the same underlying
//     Tree-sitter binding.
//   - A markdown file (.md/.markdown) with no registered grammar is chunked
//     by section: one chunk per top-level ("#" through "######") heading,
//     plus one leading chunk for any non-blank content before the first
//     heading, if any.
//   - Every other text file falls back to a fixed-size sliding window with
//     overlap (see chunkSlidingWindow's doc comment for the window/overlap
//     size and why).
//   - A binary file (sniffed for a NUL byte) is skipped: no chunks, no
//     error.
//
// A file that passes the binary sniff but still contains invalid UTF-8
// (loam-c94.20 -- e.g. a large, otherwise-valid .sql file carrying one
// stray byte from an old Mac-Roman save) is sanitized, not skipped: every
// invalid byte sequence is replaced with the Unicode replacement character
// before any strategy runs, so the invariant "text leaving this package is
// valid UTF-8" holds for every unit ever returned, and Chunker.ChunkFile's
// caller can tell it happened via chunk.Result.SanitizedInvalidUTF8 (folded
// into Stats.FilesSanitizedInvalidUTF8 by ChunkFiles) -- counted and
// logged, never silent. See chunker.go's sanitizeUTF8 for the full
// sanitize-vs-skip argument.
//
// Every strategy's raw units are then run through
// internal/ingest/chunk.EnforceBudget before being returned, so no unit
// this package hands back can exceed the configured embedding model's
// token budget -- EnforceBudget's own doc comment describes itself as
// exactly this: "the final step any chunk-producing source -- code-by-
// symbol, docs-by-section, or the sliding-window fallback -- runs its
// output through before chunks are handed to the Embedder."
//
// This package never calls an Embedder and never writes to the database --
// it is a pure function over file bytes plus an injected parser, mirroring
// internal/ingest/graph's own scope split (NOTES on loam-c94.10: "otherwise
// a pure function over file bytes (no DB, no network)").
package chunker

import (
	"context"

	"github.com/bobcob7/loam/internal/parser"
)

//go:generate go tool moq -out moq_test.go . fileParser Budgeter

// fileParser is the subset of *parser.Parser / *parser.ParserPool this
// package needs: parsing pre-read file content, as a given Language, into a
// syntax tree. Defined here at the consumer (go-standards: "define
// interfaces where consumed"), mirroring internal/ingest/graph's identical
// fileParser seam so both trees of the pipeline (parse->graph,
// chunk->embed) can share one *parser.ParserPool in production while
// mocking it independently in tests.
type fileParser interface {
	Parse(ctx context.Context, lang parser.Language, src []byte) (*parser.Tree, error)
}

// Budgeter is the seam Chunker.ChunkFile forwards to
// internal/ingest/chunk.EnforceBudget to learn the embedding model's token
// budget. It is declared locally rather than importing chunk.Budgeter
// directly, for the same reason that type's own doc comment gives: a
// package should mock exactly (and only) the one fact it depends on, not
// reach into another package's test-only mock. Because Go interface
// satisfaction is structural, any Budgeter value here is also a valid
// chunk.Budgeter, so it can be passed straight through to EnforceBudget
// with no adapter.
type Budgeter interface {
	// ContextWindow reports the embedding model's token budget
	// (internal/ingest/embed.Embedder.ContextWindow).
	ContextWindow() int
}
