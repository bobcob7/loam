package chunker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/parser"
)

// binarySniffLen is how many leading bytes isBinary inspects for a NUL
// byte -- the same "sniff the front of the file" heuristic git's own
// buffer_is_binary check and most other tools use, since real NUL bytes are
// essentially never present in genuine UTF-8/ASCII source or docs but are
// common in the first few KB of any binary format (executables, images,
// archives). Capping the scan avoids reading a whole multi-megabyte binary
// blob just to prove it is one.
const binarySniffLen = 8000

// isBinary reports whether content looks like non-text data. A file with no
// NUL byte in its first binarySniffLen bytes is treated as text even if it
// later contains bytes that are not valid UTF-8 -- isBinary's own job is
// only to decide "chunk or skip". Encoding IS validated, just not here: see
// sanitizeUTF8, which ChunkFile runs over every file isBinary lets through,
// scanning the WHOLE file rather than only this function's prefix (loam-
// c94.20 -- a file with no NUL in its first binarySniffLen bytes but an
// invalid byte later, e.g. a large .sql file with one stray Mac-Roman byte,
// is exactly the case a prefix-only encoding check would keep missing).
func isBinary(content []byte) bool {
	n := len(content)
	if n > binarySniffLen {
		n = binarySniffLen
	}
	return bytes.IndexByte(content[:n], 0) != -1
}

// invalidUTF8Replacement is the Unicode replacement character (U+FFFD)
// sanitizeUTF8 substitutes for each run of invalid UTF-8 bytes -- the same
// character strings.ToValidUTF8 and Go's own []rune/range-over-string
// conversions use for a decode error, so a sanitized chunk reads the same
// way any other place in the toolchain that met invalid UTF-8 would render
// it.
const invalidUTF8Replacement = "�"

// sanitizeUTF8 reports content unchanged (sanitized=false, no copy) when it
// is already valid UTF-8, or a copy with each run of invalid bytes replaced
// by invalidUTF8Replacement (sanitized=true) when it is not.
//
// THE DECISION (loam-c94.20): sanitise, not skip, despite isBinary already
// skipping for the not-dissimilar "this doesn't look like text" case.
// Skipping would be simpler and more consistent with isBinary's existing
// policy, but the reported production case is a large, otherwise entirely
// valid SQL file carrying exactly one stray byte from an old Mac-Roman
// save -- skipping it throws away genuinely useful, genuinely searchable
// content over one byte the user did not choose and likely does not know
// about. A replacement character inside one embedded chunk is harmless for
// RAG (the surrounding context still embeds and searches normally), and the
// alternative -- dropping the whole file from the index -- is a worse
// answer to "the SQL file I need to search didn't make it in" than "one
// chunk of it has a stray � where a bullet used to be". Whichever a future
// change picks, it must stay counted (chunk.Result.SanitizedInvalidUTF8,
// folded into Stats.FilesSanitizedInvalidUTF8 by batch.go) and logged, per
// this bead's own requirement -- never silent either way.
//
// content is scanned in FULL here, not merely binarySniffLen's leading
// prefix isBinary inspects: unlike a NUL byte, which genuine text
// essentially never contains anywhere, an invalid encoding can appear
// arbitrarily far into an otherwise-genuine text file -- exactly the
// reported case, a large SQL file whose one bad byte was nowhere near the
// front. A prefix-only check here would carry isBinary's identical hole and
// simply miss it.
func sanitizeUTF8(content []byte) (out []byte, sanitized bool) {
	if utf8.Valid(content) {
		return content, false
	}
	return []byte(strings.ToValidUTF8(string(content), invalidUTF8Replacement)), true
}

// isMarkdownPath reports whether path's extension marks it as a markdown
// document for the docs-by-section strategy.
func isMarkdownPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

// fileLines splits content into its 1-indexed lines, treating a single
// trailing "\n" as end-of-file rather than an extra blank final line -- so
// a file that ends the ordinary way (final newline present) reports the
// same line count a text editor would show, and every chunker in this
// package can address content purely by 1-indexed, inclusive line numbers.
func fileLines(content []byte) []string {
	s := strings.TrimSuffix(string(content), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// unitForLines builds a chunk.Unit spanning lines[startLine-1:endLine]
// (1-indexed, inclusive), or reports ok=false without allocating one when
// the range is invalid or the resulting content is empty or
// whitespace-only. Every strategy in this package (symbol, section,
// sliding-window) goes through this single function, so "never emit a
// blank chunk" is enforced in one place rather than three: a blank chunk
// has no search value and would still cost an embed call and a persisted
// chunks row for nothing, the same reasoning
// internal/ingest/chunk.EnforceBudget's appendGroupedPiece already applies
// to a unit that becomes blank only after splitting -- this function
// applies it before a unit is ever created, since EnforceBudget's own fast
// path (content already under budget) does not re-check blankness on units
// it did not itself split.
func unitForLines(lines []string, startLine, endLine int) (chunk.Unit, bool) {
	if startLine < 1 || endLine < startLine || endLine > len(lines) {
		return chunk.Unit{}, false
	}
	content := strings.Join(lines[startLine-1:endLine], "\n")
	if strings.TrimSpace(content) == "" {
		return chunk.Unit{}, false
	}
	return chunk.Unit{StartLine: startLine, EndLine: endLine, Content: content}, true
}

// Chunker turns one file's content into RAG chunk units. p supplies parsed
// syntax trees for the code-by-symbol strategy (typically a
// *parser.Parser or *parser.ParserPool, shared with internal/ingest/graph's
// own Extractor); logger is used for the same operator-visibility warnings
// EnforceBudget itself emits. Construct with NewChunker.
type Chunker struct {
	parser fileParser
	logger *slog.Logger
}

// NewChunker builds a Chunker over p. logger must be non-nil.
func NewChunker(p fileParser, logger *slog.Logger) *Chunker {
	return &Chunker{parser: p, logger: logger}
}

// ChunkFile chunks one file's content, choosing a strategy from path (see
// the package doc comment), and runs the result through
// internal/ingest/chunk.EnforceBudget so no returned unit exceeds
// budgeter's token budget.
//
// ok is false, with nil units, a zero chunk.Result, and a nil error, when
// content looks binary (isBinary) -- mirroring
// internal/ingest/graph.ExtractFile's ok=false-for-unsupported-language
// shape, so a caller counting a batch's outcomes (ChunkFiles) can tell
// "deliberately skipped" apart from "chunked to zero units" (e.g. an empty
// file, or a source file containing only imports/package clause and no
// declarations) without inspecting units itself.
//
// A file isBinary lets through is then run through sanitizeUTF8 before any
// strategy sees it, so the invariant "text leaving ChunkFile is valid
// UTF-8" holds for every unit this function ever returns, and every
// downstream consumer -- ultimately a Postgres text column, which
// SQLSTATE 22021-rejects anything else (loam-c94.20) -- can rely on it
// without re-validating. ChunkFile itself logs when this happens;
// result.SanitizedInvalidUTF8 additionally reports it so the caller
// (ChunkFiles) can fold it into Stats.FilesSanitizedInvalidUTF8. See
// sanitizeUTF8's doc comment for why sanitising rather than skipping.
//
// err is non-nil only for a genuine, rare failure: ctx's own
// cancellation/deadline, or the underlying parser failing to produce any
// tree at all for a file whose extension does have a registered grammar
// (see internal/ingest/graph.ExtractFile's doc comment for how rare that
// is in practice, and why it is distinct from a tree that merely contains
// syntax errors -- Tree-sitter is error-tolerant and returns a partial
// tree for ordinary broken source). A syntax error inside an otherwise
// usable tree is not an error here: chunkSymbols simply finds whatever
// well-formed top-level declarations the tree does contain.
func (c *Chunker) ChunkFile(ctx context.Context, path string, content []byte, budgeter Budgeter) (units []chunk.Unit, result chunk.Result, ok bool, err error) {
	if isBinary(content) {
		return nil, chunk.Result{}, false, nil
	}
	content, sanitized := sanitizeUTF8(content)
	if sanitized {
		c.logger.WarnContext(ctx, "file contained invalid UTF-8 and was sanitized before chunking", "file", path)
	}
	lines := fileLines(content)
	raw, err := c.chunkRaw(ctx, path, content, lines)
	if err != nil {
		return nil, chunk.Result{}, false, err
	}
	units, result = chunk.EnforceBudget(ctx, c.logger, path, raw, budgeter)
	result.SanitizedInvalidUTF8 = sanitized
	return units, result, true, nil
}

// chunkRaw dispatches to the strategy path's extension/grammar selects, and
// returns its unsplit units -- the raw material ChunkFile then runs
// through EnforceBudget.
func (c *Chunker) chunkRaw(ctx context.Context, path string, content []byte, lines []string) ([]chunk.Unit, error) {
	lang, err := parser.LanguageForPath(path)
	if err == nil {
		return c.chunkCode(ctx, path, lang, content, lines)
	}
	if !errors.Is(err, parser.ErrNoGrammar) {
		return nil, fmt.Errorf("resolving language for %s: %w", path, err)
	}
	if isMarkdownPath(path) {
		return chunkMarkdownSections(lines), nil
	}
	return chunkSlidingWindow(lines), nil
}

// chunkCode runs path's registered grammar over content and extracts one
// unit per top-level declaration (see chunkSymbols). A hard parse failure
// (no tree at all) is only ever ctx cancellation or the parser's own
// internal precondition failure -- see ChunkFile's doc comment -- so it is
// returned wrapped rather than silently falling back to another strategy,
// exactly mirroring how internal/ingest/graph.ExtractFile treats the same
// failure as fatal to the one file, not papered over.
func (c *Chunker) chunkCode(ctx context.Context, path string, lang parser.Language, content []byte, lines []string) ([]chunk.Unit, error) {
	tree, err := c.parser.Parse(ctx, lang, content)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	defer tree.Close()
	return chunkSymbols(lang, tree, lines), nil
}
