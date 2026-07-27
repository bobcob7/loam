package chunk

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// tokenBudgetSafetyMargin discounts the embedding model's advertised
// context window before it is ever converted into a byte budget, so
// bytesPerTokenEstimate's headroom gets headroom of its own: even if a
// chunk's real token density lands worse than assumed, there is still
// slack before the embedder's truncate:false rejection
// (internal/ingest/embed/ollama) would fire. 0.75 means EnforceBudget only
// ever plans against three quarters of the model's nominal window.
const tokenBudgetSafetyMargin = 0.75

// bytesPerTokenEstimate is this package's approximation of how many bytes
// of chunk content cost one embedding-model token. It exists because
// neither this package nor the chunker that will call it (loam-c94.10) has
// access to the embedding model's actual tokenizer or an exact token
// count: Ollama's /api/embed has no token-count response field, and while
// recent Ollama releases add a separate /api/tokenize endpoint, calling it
// would mean a network round trip per candidate chunk before the chunk can
// even be produced — incompatible with the chunker being a pure function
// over file bytes (loam-c94.10's NOTES: "no DB, no network").
//
// Byte-level BPE tokenizers (what Ollama's embedding models use) average
// roughly 4 bytes/token for English prose and source code; using 2 here
// already halves that, generous headroom for the common case this
// pipeline mostly sees (repo source and docs).
//
// This ratio is NOT safe for every input, and that is a deliberate,
// documented residual rather than an oversight:
//   - CJK text: a byte-level BPE tokenizer typically spends close to one
//     token per CJK character, and each such character is 3 UTF-8 bytes —
//     roughly 3 bytes/token, worse than the 2 assumed here for a chunk
//     that is mostly CJK.
//   - base64 blobs and minified JS: high-entropy or unusually dense text
//     defeats BPE's merge tables (no common substrings to merge), pushing
//     real token density down toward 1 byte/token in the worst case.
//
// A chunk dense enough in one of those ways can still exceed the real
// model context window despite fitting this estimate's budget. That
// residual is intentionally not fully absorbed by shrinking this constant
// further: an estimate tight enough to be safe for pure base64 would
// fragment ordinary code chunks far below a useful size, working against
// the RAG chunk quality this whole pipeline exists for. Instead, the
// embedder's defensive truncate:false rejection is the backstop for
// exactly this residual — an approximation miss fails that one embed call
// loudly (ollama.IsContextLengthExceeded), per loam-eg9, rather than
// silently producing a corrupt vector.
const bytesPerTokenEstimate = 2

// TokenBudgetChars converts contextWindow (an embedding model's token
// budget, e.g. embed.Embedder.ContextWindow) into the conservative byte
// budget EnforceBudget compares chunk content length against, applying
// both tokenBudgetSafetyMargin and bytesPerTokenEstimate's headroom.
// contextWindow <= 0 yields 0, so a misconfigured zero budget forces every
// non-empty chunk to be split rather than silently disabling the check —
// visibly wrong (many split log lines) rather than invisibly wrong.
func TokenBudgetChars(contextWindow int) int {
	if contextWindow <= 0 {
		return 0
	}
	return int(float64(contextWindow) * tokenBudgetSafetyMargin * bytesPerTokenEstimate)
}

// Unit is one chunk-shaped span of file content: a 1-indexed, inclusive
// start/end line range plus its text. It mirrors internal/chunkstore.
// ChunkInput's file/line/content shape but is declared independently here
// because this package runs before embedding, while ChunkInput already
// carries a post-embed vector this package has no use for.
type Unit struct {
	StartLine int
	EndLine   int
	Content   string
}

// Result reports what EnforceBudget did, for a caller (the eventual
// orchestrator, loam-c94.12) to log or fold into ingest_jobs.stats — the
// visibility loam-zoa requires for an over-budget chunk's handling.
type Result struct {
	// UnitsSplit is how many input units exceeded the budget.
	UnitsSplit int
	// PiecesProduced is the total number of output units those split
	// units became (always > UnitsSplit when UnitsSplit > 0).
	PiecesProduced int
}

// EnforceBudget is the chunk-time safety net loam-zoa adds: given units
// already produced by symbol/section/sliding-window chunking
// (loam-c94.10), it splits any unit whose content exceeds budgeter's
// token budget into sequential pieces that each fit, so no unit this
// function returns can trigger the embedder's truncate:false rejection
// (internal/ingest/embed/ollama) — the failure loam-eg9 chose specifically
// because letting it happen either silently corrupts a vector (truncate
// defaulting true) or, with truncate:false, aborts the whole ingest
// transaction for one oversized file.
//
// Splitting, never truncating or dropping: every byte of an oversized
// unit's content is preserved across its output pieces, on whole-line
// boundaries wherever possible. This can still cut across a symbol
// boundary (mid-function, mid-class) when a single symbol is itself larger
// than the budget — unavoidable once the alternative is failing the
// file's whole ingest, and exactly what loam-zoa's DESIGN calls for: "a
// file large enough to have triggered truncation is chunked into
// embeddable pieces instead of failing ingest." If even a single line
// exceeds the budget (a minified-JS line, a base64 blob with no
// newlines), that line is hard-split on rune boundaries — a last resort,
// but still lossless.
//
// file and logger exist purely for visibility: every split unit emits one
// WarnContext line naming the file, its original line range, and how many
// pieces it became, so an operator can see in logs — not only inferred
// from search quality — which files are pushing the configured model's
// budget.
func EnforceBudget(ctx context.Context, logger *slog.Logger, file string, units []Unit, budgeter Budgeter) ([]Unit, Result) {
	budget := TokenBudgetChars(budgeter.ContextWindow())
	out := make([]Unit, 0, len(units))
	var result Result
	for _, u := range units {
		if len(u.Content) <= budget {
			out = append(out, u)
			continue
		}
		pieces := splitUnit(u, budget)
		result.UnitsSplit++
		result.PiecesProduced += len(pieces)
		logger.WarnContext(ctx, "chunk exceeded embedding model budget and was split",
			"file", file, "start_line", u.StartLine, "end_line", u.EndLine,
			"content_bytes", len(u.Content), "budget_bytes", budget, "pieces", len(pieces))
		out = append(out, pieces...)
	}
	return out, result
}

// splitUnit splits u's content into sequential pieces of at most budget
// bytes each, greedily accumulating whole lines per piece and hard-splitting
// any single line that alone exceeds budget. It assumes u.Content's line
// count equals u.EndLine-u.StartLine+1 (true for every chunk unit this
// package's callers produce, since StartLine/EndLine are derived from the
// same content); a mismatch only skews the line numbers this function
// assigns to the tail of a hard-split line's pieces, never the content
// preserved.
func splitUnit(u Unit, budget int) []Unit {
	lines := strings.Split(u.Content, "\n")
	pieces := make([]Unit, 0, 4)
	pieceLines := make([]string, 0, len(lines))
	pieceLen := 0
	pieceStart := u.StartLine
	for i, line := range lines {
		absLine := u.StartLine + i
		if len(line) > budget {
			if len(pieceLines) > 0 {
				pieces = append(pieces, Unit{StartLine: pieceStart, EndLine: absLine - 1, Content: strings.Join(pieceLines, "\n")})
				pieceLines = pieceLines[:0]
				pieceLen = 0
			}
			for _, sub := range hardSplitLine(line, budget) {
				pieces = append(pieces, Unit{StartLine: absLine, EndLine: absLine, Content: sub})
			}
			pieceStart = absLine + 1
			continue
		}
		addedLen := len(line)
		if len(pieceLines) > 0 {
			addedLen++
		}
		if pieceLen+addedLen > budget && len(pieceLines) > 0 {
			pieces = append(pieces, Unit{StartLine: pieceStart, EndLine: absLine - 1, Content: strings.Join(pieceLines, "\n")})
			pieceLines = pieceLines[:0]
			pieceLen = 0
			pieceStart = absLine
			addedLen = len(line)
		}
		pieceLines = append(pieceLines, line)
		pieceLen += addedLen
	}
	if len(pieceLines) > 0 {
		pieces = append(pieces, Unit{StartLine: pieceStart, EndLine: u.EndLine, Content: strings.Join(pieceLines, "\n")})
	}
	return pieces
}

// hardSplitLine splits line (assumed to contain no newline) into pieces of
// at most budget bytes each, cutting only on rune boundaries so no piece
// contains a truncated multi-byte UTF-8 sequence. It is the last resort
// splitUnit falls back to when a single line alone exceeds budget (a
// minified-JS line, a base64 blob). budget <= 0 is clamped to 1 rune per
// piece rather than looping forever or panicking.
func hardSplitLine(line string, budget int) []string {
	if budget <= 0 {
		budget = 1
	}
	runes := []rune(line)
	pieces := make([]string, 0, len(runes)/budget+1)
	start := 0
	length := 0
	for i, r := range runes {
		runeLen := utf8.RuneLen(r)
		if length+runeLen > budget && i > start {
			pieces = append(pieces, string(runes[start:i]))
			start = i
			length = 0
		}
		length += runeLen
	}
	pieces = append(pieces, string(runes[start:]))
	return pieces
}
