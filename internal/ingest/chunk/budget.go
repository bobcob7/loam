package chunk

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// bytesPerTokenBudget is the effective bytes-per-token this package plans
// against when converting an embedding model's token budget
// (embed.Embedder.ContextWindow) into the byte budget EnforceBudget
// compares chunk content length to.
//
// It exists because neither this package nor the chunker that will call it
// (loam-c94.10) has access to the embedding model's actual tokenizer or a
// way to count tokens before an embed call: Ollama's /api/embed response
// does carry a token count (prompt_eval_count), but only after the request
// completes — too late to size a chunk before it is sent. A separate
// /api/tokenize endpoint has been proposed for Ollama but is not part of
// any released version this package can depend on, and calling it would in
// any case mean a network round trip per candidate chunk, incompatible
// with the chunker being a pure function over file bytes (loam-c94.10's
// NOTES: "no DB, no network").
//
// 2.0 is deliberately below the ~3.0-3.5 bytes/token byte-level BPE
// tokenizers measure for source code — denser than the ~4 bytes/token
// typical of English prose, which is why source code, not prose, is the
// baseline this constant is calibrated against — roughly 40% headroom on
// top of that code-calibrated density. That headroom is not free, but it
// is cheap: measured against this repo's own Go source (2451 top-level
// funcs; mean 411 B, p99 2001 B, 38.8 B/line), a 2048-token budget
// (TokenBudgetChars(2048) = 4096 bytes) is exceeded by well under 1% of
// funcs. The headroom buys protection against an expensive failure, not
// merely a wasteful one: a miss here does not cost "one failed job" — per
// docs/ingestion-spec.md "Consistency & Failure" the failure aborts the
// whole ingest transaction, and because the offending chunk's content is
// unchanged, a retry fails identically, which this bead's own DESCRIPTION
// calls "retry forever" against a repo whose index can then never advance.
// Cheap-and-frequent (some extra splitting) beats rare-and-fatal (a
// permanently stuck repo).
//
// This ratio is still not safe for every input, and that residual is
// deliberate rather than an oversight:
//   - CJK text: a byte-level BPE tokenizer typically spends close to one
//     token per CJK character, and each such character is 3 UTF-8 bytes —
//     roughly 3 bytes/token, worse than the 2.0 assumed here for a chunk
//     that is mostly CJK.
//   - base64 blobs and minified JS: high-entropy or unusually dense text
//     defeats BPE's merge tables (no common substrings to merge), pushing
//     real token density down toward 1 byte/token in the worst case.
//
// A chunk dense enough in one of those ways can still exceed the real
// model context window despite fitting this estimate's budget. Shrinking
// this constant further to absorb that case is not the fix: an estimate
// tight enough to be safe for pure base64 would fragment ordinary code
// chunks far below a useful size, working against the RAG chunk quality
// this whole pipeline exists for. Instead, the embedder's defensive
// truncate:false rejection is the backstop for exactly this residual — an
// approximation miss fails that one embed call loudly
// (ollama.IsContextLengthExceeded), per loam-eg9, rather than silently
// producing a corrupt vector.
const bytesPerTokenBudget = 2.0

// TokenBudgetChars converts contextWindow (an embedding model's token
// budget, e.g. embed.Embedder.ContextWindow) into the byte budget
// EnforceBudget compares chunk content length against, via
// bytesPerTokenBudget. contextWindow <= 0 yields 0, so a misconfigured
// zero budget forces every non-empty chunk to be split rather than
// silently disabling the check — visibly wrong (many split log lines)
// rather than invisibly wrong.
func TokenBudgetChars(contextWindow int) int {
	if contextWindow <= 0 {
		return 0
	}
	return int(float64(contextWindow) * bytesPerTokenBudget)
}

// Unit is one chunk-shaped span of file content: a 1-indexed, inclusive
// start/end line range plus its text. It is declared independently of
// internal/chunkstore.ChunkInput (which also carries StartLine, EndLine
// and Content, plus a post-embed Embedding this package has no use for)
// because this package runs before embedding and has no file identifier
// to carry either — a caller pairs a Unit with the file it came from
// itself (EnforceBudget takes file as a separate argument for exactly
// this reason).
type Unit struct {
	StartLine int
	EndLine   int
	Content   string
}

// Result reports what happened while turning one file into chunk units --
// primarily what EnforceBudget did (UnitsSplit/PiecesProduced, for loam-
// zoa's over-budget-chunk visibility), plus SanitizedInvalidUTF8, which
// chunker.Chunker.ChunkFile sets earlier in the same pipeline (loam-c94.20)
// -- for a caller (the orchestrator, loam-c94.12) to log or fold into
// ingest_jobs.stats.
type Result struct {
	// UnitsSplit is how many input units exceeded the budget.
	UnitsSplit int
	// PiecesProduced is the total number of output units those split
	// units became (always > UnitsSplit when UnitsSplit > 0).
	PiecesProduced int
	// SanitizedInvalidUTF8 reports whether the file's content contained at
	// least one invalid UTF-8 byte sequence that Chunker.ChunkFile replaced
	// with the Unicode replacement character before chunking (loam-c94.20),
	// so text leaving the chunker is always valid UTF-8. This field is set
	// by ChunkFile itself, not by EnforceBudget (sanitisation happens
	// earlier in the pipeline, at the same point that decides chunk-or-skip)
	// -- Result is simply the one per-file report both steps fold their
	// findings into, so batch.go's Stats aggregation has a single place to
	// read from, mirroring how UnitsSplit/PiecesProduced already work.
	SanitizedInvalidUTF8 bool
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
// Splitting, never truncating: concatenating every returned piece's
// Content in order, with no separator, reproduces the input unit's
// Content exactly — each non-final piece keeps the newline that followed
// it in the original (see splitUnit), so this holds even across a
// hard-split line. The one deliberate exception is a piece that would
// otherwise contain only whitespace (e.g. a run of blank lines, or the
// empty final segment a trailing newline produces): EnforceBudget folds
// its line range into the piece before it rather than emitting a chunk
// with nothing for the embedder to usefully embed — a vector for pure
// whitespace has no search value, and would still cost an embed call and
// a persisted chunks row for nothing. Splitting can still cut across a
// symbol boundary (mid-function, mid-class) when a single symbol is
// itself larger than the budget — unavoidable once the alternative is
// failing the file's whole ingest, and exactly what loam-zoa's ACCEPTANCE
// CRITERIA calls for: "a file large enough to have triggered truncation
// is chunked into embeddable pieces instead of failing ingest." If even a
// single line exceeds the budget (e.g. a minified-JS line or a base64
// blob with no newlines to split on), that line is hard-split on rune
// boundaries — a last resort, but still lossless.
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

// SplitUnit is splitUnit exported for internal/ingest/vectors (loam-c94.16):
// when an embed call rejects a chunk that this package's own byte-budget
// estimate missed -- a genuine token-density outlier (dense JSON, base64),
// not a bug in the estimate, see bytesPerTokenBudget's doc comment -- the
// caller needs the exact same lossless, line-granularity, rune-boundary-safe
// splitting EnforceBudget already uses, reacting to the embedder's own
// rejection instead of re-guessing a byte/token ratio. Reusing this function
// rather than duplicating its logic is the point: there is exactly one
// definition of "how to cut a chunk without corrupting it" in this codebase.
func SplitUnit(u Unit, budget int) []Unit {
	return splitUnit(u, budget)
}

// splitUnit splits u's content into sequential pieces that each fit
// within budget, greedily accumulating whole lines per piece and
// hard-splitting any single line that alone exceeds budget.
//
// Every piece that is not the very last one produced keeps the trailing
// "\n" that followed its content in the original — attached only to the
// last piece derived from a given line, never to an earlier hard-split
// fragment of one — so concatenating every returned piece's Content in
// order, with no separator, reproduces u.Content exactly (see
// EnforceBudget's doc comment for the one exception: a would-be
// whitespace-only piece is folded into its predecessor instead of
// emitted). To leave room for that trailing byte, accumulation is capped
// at budget-1 rather than budget; a piece that never receives a trailing
// newline (the very last one) is consequently at most one byte more
// conservative than strictly necessary, which is immaterial at the sizes
// this package deals in.
//
// It assumes u.Content's line count equals u.EndLine-u.StartLine+1 (true
// for every chunk unit this package's callers produce, since
// StartLine/EndLine are derived from the same content); a mismatch only
// skews the line numbers assigned to a hard-split line's pieces, never
// the content preserved.
func splitUnit(u Unit, budget int) []Unit {
	splitCap := budget - 1
	if splitCap < 0 {
		splitCap = 0
	}
	lines := strings.Split(u.Content, "\n")
	lastIdx := len(lines) - 1
	pieces := make([]Unit, 0, 4)
	pieceLines := make([]string, 0, len(lines))
	pieceLen := 0
	pieceStart := u.StartLine
	for i, line := range lines {
		absLine := u.StartLine + i
		if len(line) > splitCap {
			if len(pieceLines) > 0 {
				pieces = appendGroupedPiece(pieces, pieceStart, absLine-1, strings.Join(pieceLines, "\n")+"\n")
				pieceLines = pieceLines[:0]
				pieceLen = 0
			}
			subs := hardSplitLine(line, splitCap)
			for j, sub := range subs {
				if j == len(subs)-1 && i != lastIdx {
					sub += "\n"
				}
				pieces = append(pieces, Unit{StartLine: absLine, EndLine: absLine, Content: sub})
			}
			pieceStart = absLine + 1
			continue
		}
		addedLen := len(line)
		if len(pieceLines) > 0 {
			addedLen++
		}
		if pieceLen+addedLen > splitCap && len(pieceLines) > 0 {
			pieces = appendGroupedPiece(pieces, pieceStart, absLine-1, strings.Join(pieceLines, "\n")+"\n")
			pieceLines = pieceLines[:0]
			pieceLen = 0
			pieceStart = absLine
			addedLen = len(line)
		}
		pieceLines = append(pieceLines, line)
		pieceLen += addedLen
	}
	if len(pieceLines) > 0 {
		pieces = appendGroupedPiece(pieces, pieceStart, u.EndLine, strings.Join(pieceLines, "\n"))
	}
	return pieces
}

// appendGroupedPiece appends a whole-line-granularity piece to pieces,
// unless content is empty or whitespace-only, in which case its line
// range is folded into the preceding piece (extending that piece's
// EndLine) instead of being emitted as its own chunk: a piece built
// entirely from blank lines has no search value and would otherwise
// become a persisted, embedded chunks row for nothing. Folding never adds
// bytes to any piece's Content, so it can never push a piece over budget.
//
// This is deliberately only ever called for grouped-lines pieces, never
// for an individual hard-split fragment of one line (splitUnit's other
// piece-emission site): a hard-split fragment may be whitespace-only by
// pure accident of where a real, non-blank line happened to be cut (e.g.
// a fragment landing entirely within a run of indentation), and folding
// that away would corrupt reconstruction of genuine content rather than
// discard genuinely empty filler.
//
// If pieces is empty — only reachable when the very first flush of a
// wholly blank/whitespace unit is itself whitespace-only — the piece is
// appended anyway, so a unit is never reduced to zero pieces.
func appendGroupedPiece(pieces []Unit, startLine, endLine int, content string) []Unit {
	if strings.TrimSpace(content) == "" && len(pieces) > 0 {
		pieces[len(pieces)-1].EndLine = endLine
		return pieces
	}
	return append(pieces, Unit{StartLine: startLine, EndLine: endLine, Content: content})
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
