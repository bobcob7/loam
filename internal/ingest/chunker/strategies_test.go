package chunker

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsBinary_NulByteInFirstBytes_ReportsTrue(t *testing.T) {
	t.Parallel()
	assert.True(t, isBinary([]byte("abc\x00def")))
}

func TestIsBinary_PlainTextWithNoNulByte_ReportsFalse(t *testing.T) {
	t.Parallel()
	assert.False(t, isBinary([]byte("package main\n\nfunc main() {}\n")))
}

func TestIsBinary_NulByteBeyondSniffWindow_IsNotDetected(t *testing.T) {
	t.Parallel()
	// Documents the sniff-window trade-off explicitly, rather than leaving
	// it as an unstated limitation: a NUL far past binarySniffLen is
	// invisible to this heuristic by design (bounded scan cost), so this
	// is a pinned characteristic, not a bug report.
	content := append([]byte(strings.Repeat("a", binarySniffLen+10)), 0)
	assert.False(t, isBinary(content))
}

func TestSanitizeUTF8_ValidContent_ReturnedUnchangedByteIdentical(t *testing.T) {
	t.Parallel()
	// loam-c94.20 acceptance criterion 6: valid UTF-8, including multibyte
	// characters, must not be mangled by the fix -- exact bytes, not just
	// "looks the same".
	valid := []byte("café: 日本語, emoji 🎉, and a stray Ñ\n")
	out, sanitized := sanitizeUTF8(valid)
	assert.False(t, sanitized)
	assert.True(t, bytes.Equal(valid, out), "valid UTF-8 content must be byte-identical after sanitizeUTF8")
}

func TestSanitizeUTF8_InvalidByte_ReplacedWithReplacementCharacter(t *testing.T) {
	t.Parallel()
	// The exact byte the production incident reported: 0xa5, a lone
	// continuation byte -- valid Mac Roman/Latin-1, invalid UTF-8.
	invalid := []byte("SELECT 1; -- old comment \xa5 bullet\n")
	out, sanitized := sanitizeUTF8(invalid)
	require.True(t, sanitized)
	assert.True(t, utf8.Valid(out), "sanitized output must always be valid UTF-8")
	assert.NotContains(t, string(out), "\xa5")
	assert.Contains(t, string(out), invalidUTF8Replacement)
	assert.Contains(t, string(out), "SELECT 1; -- old comment", "bytes surrounding the bad one must be preserved")
	assert.Contains(t, string(out), "bullet\n", "bytes surrounding the bad one must be preserved")
}

func TestSanitizeUTF8_EmptyContent_ReturnedUnchanged(t *testing.T) {
	t.Parallel()
	out, sanitized := sanitizeUTF8(nil)
	assert.False(t, sanitized)
	assert.Empty(t, out)
}

func TestIsMarkdownPath_RecognizesMdAndMarkdownExtensionsCaseInsensitively(t *testing.T) {
	t.Parallel()
	assert.True(t, isMarkdownPath("README.md"))
	assert.True(t, isMarkdownPath("docs/OVERVIEW.MD"))
	assert.True(t, isMarkdownPath("notes.markdown"))
	assert.False(t, isMarkdownPath("notes.txt"))
	assert.False(t, isMarkdownPath("main.go"))
}

func TestFileLines_TrimsExactlyOneTrailingNewline(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"a", "b"}, fileLines([]byte("a\nb\n")))
	assert.Equal(t, []string{"a", "b"}, fileLines([]byte("a\nb")), "no trailing newline still yields the same two lines")
	assert.Equal(t, []string{"a", "b", ""}, fileLines([]byte("a\nb\n\n")), "a second trailing newline is a genuine blank final line, not trimmed away")
	assert.Nil(t, fileLines([]byte{}))
	assert.Nil(t, fileLines([]byte("\n")))
}

// TestUnitForLines_BlankContent_ReturnsFalse is MUTATION 2's kill switch:
// if a caller of unitForLines (or unitForLines itself) stopped checking
// for blank content, a chunk with nothing in it -- zero search value, but
// still a real embed call and a persisted chunks row -- would reach
// EnforceBudget's untouched fast path and come out the other side
// unfiltered.
func TestUnitForLines_BlankContent_ReturnsFalse(t *testing.T) {
	t.Parallel()
	_, ok := unitForLines([]string{"", "  ", "\t"}, 1, 3)
	assert.False(t, ok)
	_, ok = unitForLines([]string{"real content"}, 1, 1)
	assert.True(t, ok)
}

func TestUnitForLines_InvalidRange_ReturnsFalse(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c"}
	_, ok := unitForLines(lines, 0, 1)
	assert.False(t, ok, "startLine below 1")
	_, ok = unitForLines(lines, 2, 1)
	assert.False(t, ok, "endLine before startLine")
	_, ok = unitForLines(lines, 1, 4)
	assert.False(t, ok, "endLine past the last line")
}

func TestUnitForLines_ValidRange_JoinsExactSlice(t *testing.T) {
	t.Parallel()
	lines := []string{"one", "two", "three", "four"}
	u, ok := unitForLines(lines, 2, 3)
	require.True(t, ok)
	assert.Equal(t, 2, u.StartLine)
	assert.Equal(t, 3, u.EndLine)
	assert.Equal(t, "two\nthree", u.Content)
}

// TestChunkMarkdownSections_PreHeadingContent_BecomesLeadingChunk proves
// non-blank content before the first heading is not dropped: it becomes
// its own chunk starting at line 1.
func TestChunkMarkdownSections_PreHeadingContent_BecomesLeadingChunk(t *testing.T) {
	t.Parallel()
	lines := fileLines([]byte("Intro paragraph before any heading.\n\n# Title\nBody text.\n"))
	units := chunkMarkdownSections(lines)
	require.Len(t, units, 2)
	assert.Equal(t, 1, units[0].StartLine)
	assert.Equal(t, 2, units[0].EndLine)
	assert.Contains(t, units[0].Content, "Intro paragraph")
	assert.Equal(t, 3, units[1].StartLine)
	assert.Contains(t, units[1].Content, "# Title")
}

// TestChunkMarkdownSections_BlankPreHeadingContent_ProducesNoLeadingChunk
// is MUTATION 2's markdown-specific instance: blank lines before the first
// heading must not become an empty leading chunk.
func TestChunkMarkdownSections_BlankPreHeadingContent_ProducesNoLeadingChunk(t *testing.T) {
	t.Parallel()
	lines := fileLines([]byte("\n\n\n# Title\nBody\n"))
	units := chunkMarkdownSections(lines)
	require.Len(t, units, 1, "the blank lines before the heading must not become their own chunk")
	assert.Equal(t, 4, units[0].StartLine)
	assertNoBlankUnits(t, units)
}

func TestChunkMarkdownSections_NoHeadings_WholeFileIsOneChunk(t *testing.T) {
	t.Parallel()
	lines := fileLines([]byte("Just prose.\nNo headings anywhere in this file.\n"))
	units := chunkMarkdownSections(lines)
	require.Len(t, units, 1)
	assert.Equal(t, 1, units[0].StartLine)
	assert.Equal(t, 2, units[0].EndLine)
}

// TestChunkMarkdownSections_OversizedSection_IsNotSplitHere pins that
// section-level splitting is exclusively chunk.EnforceBudget's job: this
// strategy function on its own, given no budget concept at all, must
// return an oversized section as a single raw unit, not attempt to shrink
// it -- proving "every path ends in EnforceBudget" is true because the
// strategies themselves do no budget-aware work of their own.
func TestChunkMarkdownSections_OversizedSection_IsNotSplitHere(t *testing.T) {
	t.Parallel()
	bigParagraph := strings.Repeat("word ", 5000)
	lines := fileLines([]byte("# Huge Section\n" + bigParagraph + "\n"))
	units := chunkMarkdownSections(lines)
	require.Len(t, units, 1, "chunkMarkdownSections never splits on its own, regardless of size")
	assert.Greater(t, len(units[0].Content), 4096, "the section really is oversized relative to a realistic embedding budget")
}

// TestChunkSlidingWindow_OverlapIsExactlyTheConfiguredAmount is the
// low-level counterpart of the ChunkFile-level overlap test: it calls
// chunkSlidingWindow directly (no EnforceBudget in between) so a failure
// here isolates the bug to the windowing arithmetic itself.
func TestChunkSlidingWindow_OverlapIsExactlyTheConfiguredAmount(t *testing.T) {
	t.Parallel()
	lines := make([]string, 250)
	for i := range lines {
		lines[i] = "x"
	}
	units := chunkSlidingWindow(lines)
	require.Len(t, units, 3)
	assert.Equal(t, 1, units[0].StartLine)
	assert.Equal(t, 100, units[0].EndLine)
	assert.Equal(t, 81, units[1].StartLine)
	assert.Equal(t, 180, units[1].EndLine)
	assert.Equal(t, 161, units[2].StartLine)
	assert.Equal(t, 250, units[2].EndLine)
}

func TestChunkSlidingWindow_ShorterThanOneWindow_IsSingleChunk(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c"}
	units := chunkSlidingWindow(lines)
	require.Len(t, units, 1)
	assert.Equal(t, 1, units[0].StartLine)
	assert.Equal(t, 3, units[0].EndLine)
}

func TestChunkSlidingWindow_EmptyFile_ProducesNoUnits(t *testing.T) {
	t.Parallel()
	assert.Empty(t, chunkSlidingWindow(nil))
}

// TestChunkSlidingWindow_TrailingBlankWindow_IsNotEmitted proves the
// sliding-window strategy also goes through unitForLines' blank filter: a
// 200-line file whose second half is entirely blank lines produces a final
// window (lines 161-200) that is itself all-blank, and that window must
// not be emitted at all.
func TestChunkSlidingWindow_TrailingBlankWindow_IsNotEmitted(t *testing.T) {
	t.Parallel()
	lines := make([]string, 200)
	for i := 0; i < 100; i++ {
		lines[i] = "content"
	}
	units := chunkSlidingWindow(lines)
	require.Len(t, units, 2, "the all-blank final window (lines 161-200) must be filtered out, not emitted as a third window")
	for _, u := range units {
		assert.NotEmpty(t, strings.TrimSpace(u.Content))
	}
}
