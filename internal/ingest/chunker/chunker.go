package chunker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

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
// later contains bytes that are not valid UTF-8 -- this package's job is to
// decide "chunk or skip", not to validate encoding.
func isBinary(content []byte) bool {
	n := len(content)
	if n > binarySniffLen {
		n = binarySniffLen
	}
	return bytes.IndexByte(content[:n], 0) != -1
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
	lines := fileLines(content)
	raw, err := c.chunkRaw(ctx, path, content, lines)
	if err != nil {
		return nil, chunk.Result{}, false, err
	}
	units, result = chunk.EnforceBudget(ctx, c.logger, path, raw, budgeter)
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
