package chunk

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// totalContentLen sums every piece's content length, so tests can assert no
// bytes were lost across a split — the property that distinguishes
// splitting (loam-zoa's chosen behaviour) from silent truncation.
func totalContentLen(units []Unit) int {
	total := 0
	for _, u := range units {
		total += len(u.Content)
	}
	return total
}

func TestTokenBudgetChars_AppliesSafetyMarginAndByteRatio(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 3072, TokenBudgetChars(2048), "2048 tokens * 0.75 margin * 2 bytes/token")
	assert.Equal(t, 768, TokenBudgetChars(512), "512 tokens * 0.75 margin * 2 bytes/token")
}

func TestTokenBudgetChars_NonPositiveContextWindow_ReturnsZero(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, TokenBudgetChars(0))
	assert.Equal(t, 0, TokenBudgetChars(-1))
}

// TestEnforceBudget_UnderBudgetChunk_PassesThroughUnchanged is the small-
// chunk control case: a chunk well within budget must survive untouched,
// including its line range and identity, so EnforceBudget cannot be
// satisfied by simply splitting everything indiscriminately.
func TestEnforceBudget_UnderBudgetChunk_PassesThroughUnchanged(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 2048 }}
	units := []Unit{{StartLine: 1, EndLine: 1, Content: "func small() {}"}}
	out, result := EnforceBudget(t.Context(), testLogger(), "small.go", units, budgeter)
	require.Equal(t, units, out)
	assert.Equal(t, Result{}, result)
}

// TestEnforceBudget_OversizedChunk_IsSplitNotFailed is the core proof this
// bead exists for: a single chunk that genuinely exceeds the configured
// model's token budget must come back as multiple pieces, each within
// budget, with the file's content fully preserved (not truncated, not
// dropped) and the split made visible via the returned Result.
func TestEnforceBudget_OversizedChunk_IsSplitNotFailed(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 2048 }}
	budget := TokenBudgetChars(2048)
	line := strings.Repeat("a", 100)
	lines := make([]string, 200) // 200 * 101 bytes >> budget (3072)
	for i := range lines {
		lines[i] = line
	}
	content := strings.Join(lines, "\n")
	units := []Unit{{StartLine: 10, EndLine: 209, Content: content}}
	out, result := EnforceBudget(t.Context(), testLogger(), "huge.go", units, budgeter)
	require.Greater(t, len(out), 1, "an oversized chunk must become more than one piece")
	assert.Equal(t, Result{UnitsSplit: 1, PiecesProduced: len(out)}, result)
	for _, piece := range out {
		assert.LessOrEqualf(t, len(piece.Content), budget, "piece for lines %d-%d exceeds the budget", piece.StartLine, piece.EndLine)
	}
	rejoined := make([]string, len(out))
	for i, piece := range out {
		rejoined[i] = piece.Content
	}
	assert.Equal(t, content, strings.Join(rejoined, "\n"), "splitting must preserve every byte of the original content")
}

// TestEnforceBudget_LineRangesAreContiguousAndCoverOriginal pins that a
// split's pieces reconstruct the original unit's line range exactly: the
// first piece starts where the input started, the last piece ends where
// the input ended, and each subsequent piece picks up immediately after
// the previous one left off (no gap, no overlap, no reordering).
func TestEnforceBudget_LineRangesAreContiguousAndCoverOriginal(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 2048 }}
	line := strings.Repeat("b", 100)
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = line
	}
	units := []Unit{{StartLine: 5, EndLine: 204, Content: strings.Join(lines, "\n")}}
	out, _ := EnforceBudget(t.Context(), testLogger(), "ranges.go", units, budgeter)
	require.NotEmpty(t, out)
	assert.Equal(t, 5, out[0].StartLine)
	assert.Equal(t, 204, out[len(out)-1].EndLine)
	for i := 1; i < len(out); i++ {
		assert.Equalf(t, out[i-1].EndLine+1, out[i].StartLine, "piece %d must start immediately after piece %d ends", i, i-1)
	}
}

// TestEnforceBudget_SingleLineExceedsBudget_HardSplitsOnRuneBoundaries
// covers the worst case named in loam-zoa's DESIGN: a chunk with no
// newlines at all -- a minified-JS-shaped line, and one containing
// multi-byte UTF-8 runes (standing in for base64/CJK-dense content) -- so
// the line-oriented split alone cannot help and the rune-safe hard split
// must take over, without ever cutting a multi-byte rune in half or
// losing any content.
func TestEnforceBudget_SingleLineExceedsBudget_HardSplitsOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 2048 }}
	budget := TokenBudgetChars(2048)
	// A single unbroken "line" of multi-byte runes long enough to force
	// several hard-split pieces.
	content := strings.Repeat("é", budget*3) // "é", 2 bytes in UTF-8
	units := []Unit{{StartLine: 1, EndLine: 1, Content: content}}
	out, result := EnforceBudget(t.Context(), testLogger(), "minified.js", units, budgeter)
	require.Greater(t, len(out), 1)
	assert.Equal(t, 1, result.UnitsSplit)
	for _, piece := range out {
		assert.LessOrEqual(t, len(piece.Content), budget)
		assert.Truef(t, utf8.ValidString(piece.Content), "piece must not cut a multi-byte rune: %q", piece.Content)
		assert.Equal(t, 1, piece.StartLine)
		assert.Equal(t, 1, piece.EndLine)
	}
	var rejoined strings.Builder
	for _, piece := range out {
		rejoined.WriteString(piece.Content)
	}
	assert.Equal(t, content, rejoined.String(), "hard-splitting must preserve every rune")
}

// TestEnforceBudget_MultipleUnits_OnlyOversizedOnesAreSplit exercises a
// realistic mixed batch -- most chunks from a real file are well within
// budget, and only the pathological one should be touched.
func TestEnforceBudget_MultipleUnits_OnlyOversizedOnesAreSplit(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 2048 }}
	budget := TokenBudgetChars(2048)
	small := Unit{StartLine: 1, EndLine: 2, Content: "func a() {}\nfunc b() {}"}
	line := strings.Repeat("c", 100)
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = line
	}
	huge := Unit{StartLine: 3, EndLine: 202, Content: strings.Join(lines, "\n")}
	out, result := EnforceBudget(t.Context(), testLogger(), "mixed.go", []Unit{small, huge}, budgeter)
	assert.Equal(t, Result{UnitsSplit: 1, PiecesProduced: result.PiecesProduced}, result)
	assert.Equal(t, small, out[0], "the under-budget unit must be untouched and stay first")
	for _, piece := range out[1:] {
		assert.LessOrEqual(t, len(piece.Content), budget)
	}
}

// TestEnforceBudget_ExactlyAtBudget_IsNotSplit and
// TestEnforceBudget_OneByteOverBudget_IsSplit together pin the boundary
// condition exactly: a chunk of precisely budget bytes must pass through
// untouched, and one byte more must split. This is the specific case an
// off-by-one in the budget comparison (e.g. using < instead of <=, or
// computing the budget one token too generous) would get wrong in exactly
// one direction, so the pair catches it regardless of which direction the
// bug goes.
func TestEnforceBudget_ExactlyAtBudget_IsNotSplit(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 2048 }}
	budget := TokenBudgetChars(2048)
	units := []Unit{{StartLine: 1, EndLine: 1, Content: strings.Repeat("x", budget)}}
	out, result := EnforceBudget(t.Context(), testLogger(), "exact.go", units, budgeter)
	require.Len(t, out, 1)
	assert.Equal(t, Result{}, result)
	assert.Equal(t, budget, len(out[0].Content))
}

func TestEnforceBudget_OneByteOverBudget_IsSplit(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 2048 }}
	budget := TokenBudgetChars(2048)
	units := []Unit{{StartLine: 1, EndLine: 1, Content: strings.Repeat("x", budget+1)}}
	out, result := EnforceBudget(t.Context(), testLogger(), "over.go", units, budgeter)
	require.Greater(t, len(out), 1, "one byte over budget on a single line must still be split")
	assert.Equal(t, 1, result.UnitsSplit)
	assert.Equal(t, totalContentLen(units), totalContentLen(out), "no content may be lost splitting a single oversized line")
}

// TestEnforceBudget_ZeroContextWindow_SplitsEverythingVisibly pins
// TokenBudgetChars's deliberate fail-safe: a misconfigured Budgeter
// reporting 0 must not be silently treated as "no limit" -- every
// non-empty chunk is split down to nothing, which is loud (many pieces,
// many split log lines) rather than a silent bypass of the whole feature.
func TestEnforceBudget_ZeroContextWindow_SplitsEverythingVisibly(t *testing.T) {
	t.Parallel()
	budgeter := &BudgeterMock{ContextWindowFunc: func() int { return 0 }}
	units := []Unit{{StartLine: 1, EndLine: 1, Content: "func a() {}"}}
	out, result := EnforceBudget(t.Context(), testLogger(), "misconfigured.go", units, budgeter)
	assert.Equal(t, 1, result.UnitsSplit)
	assert.Greater(t, result.PiecesProduced, 1, "a zero budget must fragment even a tiny chunk, not silently pass it through")
	assert.Equal(t, "func a() {}", strings.Join(contentsOf(out), ""), "content must still be fully preserved, just as many tiny pieces")
}

func contentsOf(units []Unit) []string {
	out := make([]string, len(units))
	for i, u := range units {
		out[i] = u.Content
	}
	return out
}
