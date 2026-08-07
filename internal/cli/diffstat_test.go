package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComputeDiffStat_CountsHunkLinesNotFileHeaders is the parser's central
// hazard: `---` and `+++` are themselves lines beginning with - and +, so a
// counter that did not wait for an `@@` hunk header would report one extra
// insertion and one extra deletion on EVERY file, and the error would scale
// with the number of files rather than looking obviously wrong.
func TestComputeDiffStat_CountsHunkLinesNotFileHeaders(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/auth.go b/auth.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/auth.go\n" +
		"+++ b/auth.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package auth\n" +
		"-old\n" +
		"+new\n" +
		"+extra\n"
	stat := computeDiffStat(diff)
	assert.Equal(t, 1, stat.FilesChanged)
	assert.Equal(t, 2, stat.Insertions)
	assert.Equal(t, 1, stat.Deletions)
	require.Len(t, stat.Files, 1)
	assert.Equal(t, diffStatFile{Path: "auth.go", Insertions: 2, Deletions: 1}, stat.Files[0])
}

// TestComputeDiffStat_MultipleFiles_AttributesEachSeparately pins that the
// per-file rows are what the summary is made of, not a second count that
// could disagree with it.
func TestComputeDiffStat_MultipleFiles_AttributesEachSeparately(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1,0 +1,2 @@\n+p\n+q\n"
	stat := computeDiffStat(diff)
	require.Len(t, stat.Files, 2)
	assert.Equal(t, diffStatFile{Path: "a.go", Insertions: 1, Deletions: 1}, stat.Files[0])
	assert.Equal(t, diffStatFile{Path: "b.go", Insertions: 2}, stat.Files[1])
	assert.Equal(t, 2, stat.FilesChanged)
	assert.Equal(t, 3, stat.Insertions)
	assert.Equal(t, 1, stat.Deletions)
	total := 0
	for _, file := range stat.Files {
		total += file.Insertions
	}
	assert.Equal(t, stat.Insertions, total, "the summary must be the sum of the rows it is presented alongside")
}

// TestComputeDiffStat_NewAndDeletedFiles pins the two headers that have no
// counterpart on the other side: a new file's `--- /dev/null` and a deleted
// file's `+++ /dev/null`, which must still be named and counted.
func TestComputeDiffStat_NewAndDeletedFiles(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1,2 @@\n+a\n+b\n" +
		"diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n"
	stat := computeDiffStat(diff)
	require.Len(t, stat.Files, 2)
	assert.Equal(t, diffStatFile{Path: "new.go", Insertions: 2}, stat.Files[0])
	assert.Equal(t, diffStatFile{Path: "gone.go", Deletions: 2}, stat.Files[1], "a deleted file must be named after itself, never after /dev/null")
}

// TestComputeDiffStat_BinaryFile_IsFlaggedNotCountedAsZero pins that a
// binary change is distinguishable from a file that changed by nothing.
func TestComputeDiffStat_BinaryFile_IsFlaggedNotCountedAsZero(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/logo.png b/logo.png\nindex 111..222 100644\nBinary files a/logo.png and b/logo.png differ\n"
	stat := computeDiffStat(diff)
	require.Len(t, stat.Files, 1)
	assert.True(t, stat.Files[0].Binary)
	assert.Equal(t, "logo.png", stat.Files[0].Path)
	assert.Equal(t, 1, stat.FilesChanged)
}

// TestComputeDiffStat_RenameWithNoContentChange pins the one shape with
// neither a `+++` header nor any hunk: the destination path must still be
// reported, and it must be the DESTINATION, not the source.
func TestComputeDiffStat_RenameWithNoContentChange(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/old/path.go b/new/path.go\nsimilarity index 100%\nrename from old/path.go\nrename to new/path.go\n"
	stat := computeDiffStat(diff)
	require.Len(t, stat.Files, 1)
	assert.Equal(t, "new/path.go", stat.Files[0].Path)
}

// TestComputeDiffStat_RenameIntoAPathContainingTheHeaderSeparator is why
// the `rename to` branch exists at all. For an ordinary rename the
// `diff --git` fallback already recovers the destination from the last
// " b/", so nothing distinguishes the two routes -- until the destination
// path itself contains " b/", at which point the fallback splits in the
// wrong place and only the explicit `rename to` line is right. Without this
// case, deleting the branch entirely changes no observable behaviour.
func TestComputeDiffStat_RenameIntoAPathContainingTheHeaderSeparator(t *testing.T) {
	t.Parallel()
	const dst = "docs/section b/notes.md"
	assert.NotEqual(t, dst, pathFromDiffGitLine("a/notes.md b/"+dst), "precondition: the header-line fallback cannot recover this path, which is what makes `rename to` load-bearing")
	diff := "diff --git a/notes.md b/" + dst + "\nsimilarity index 100%\nrename from notes.md\nrename to " + dst + "\n"
	stat := computeDiffStat(diff)
	require.Len(t, stat.Files, 1)
	assert.Equal(t, dst, stat.Files[0].Path)
}

// TestPathFromDiffGitLine covers the fallback path namer directly,
// including the case a naive split on the first space gets wrong.
func TestPathFromDiffGitLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rest string
		want string
	}{
		{"plain", "a/auth.go b/auth.go", "auth.go"},
		{"nested", "a/internal/cli/clone.go b/internal/cli/clone.go", "internal/cli/clone.go"},
		{"path with spaces", "a/my docs/notes.md b/my docs/notes.md", "my docs/notes.md"},
		{"rename", "a/old.go b/new.go", "new.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, pathFromDiffGitLine(tt.rest))
		})
	}
}

// TestComputeDiffStat_EmptyDiff_IsZeroFilesNotNil pins that nothing changed
// encodes as an empty list rather than null, matching every other list this
// CLI returns.
func TestComputeDiffStat_EmptyDiff_IsZeroFilesNotNil(t *testing.T) {
	t.Parallel()
	stat := computeDiffStat("")
	assert.NotNil(t, stat.Files)
	assert.Empty(t, stat.Files)
	assert.Equal(t, 0, stat.FilesChanged)
	assert.False(t, stat.Truncated)
}

// TestDiffWasTruncated_DetectsTheServersMarker pins that a capped diff SAYS
// it is capped, so the counts are not read as complete.
func TestDiffWasTruncated_DetectsTheServersMarker(t *testing.T) {
	t.Parallel()
	// The literal shape internal/gitdiff appends (diffTruncatedMarkerFormat).
	marker := fmt.Sprintf("\n... diff truncated at %d bytes; git produced more -- fetch %s locally and diff against %s directly for the full change ...\n", 4<<20, "wb-1", "main")
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y" + marker
	stat := computeDiffStat(diff)
	assert.True(t, stat.Truncated)
	assert.Equal(t, 1, stat.FilesChanged, "the part that DID arrive is still summarized")
}

// TestDiffWasTruncated_OnlyTheTailCounts is the false-positive guard. The
// marker is always the LAST thing in a truncated diff, so a match anywhere
// earlier is content, not truncation -- and content is exactly where it can
// appear, since internal/gitdiff's own source carries that sentence and any
// diff touching that file reproduces it.
//
// The fixture places a genuine, byte-for-byte match at the START and then
// pushes it out of the tail with padding, which is the only construction
// that actually distinguishes a tail search from a whole-text one: a
// fixture whose "marker" differs from the real one at all (an escaped
// newline, say) would pass under either implementation and prove nothing.
func TestDiffWasTruncated_OnlyTheTailCounts(t *testing.T) {
	t.Parallel()
	marker := diffTruncatedNeedle + "4194304 bytes; git produced more ...\n"
	require.True(t, diffWasTruncated("some diff"+marker), "precondition: this IS the marker, so the test below is not passing on a near-miss")
	padded := "some diff" + marker + strings.Repeat("+padding pushing the marker out of the tail\n", 100)
	require.Greater(t, len(padded)-len("some diff"+marker), diffTruncatedTailBytes, "precondition: the padding must exceed the tail window")
	assert.False(t, diffWasTruncated(padded))
}

// TestHumanDiffStat_ReportsEveryFileAndTheSummary pins the human rendering
// -- the mode a caller reads directly -- including the truncation warning,
// which must not be visible only in the structured formats.
func TestHumanDiffStat_ReportsEveryFileAndTheSummary(t *testing.T) {
	t.Parallel()
	stat := diffStat{
		FilesChanged: 2,
		Insertions:   3,
		Deletions:    1,
		Truncated:    true,
		Files: []diffStatFile{
			{Path: "a.go", Insertions: 3, Deletions: 1},
			{Path: "logo.png", Binary: true},
		},
	}
	text := humanDiffStat(stat)
	assert.Contains(t, text, "a.go")
	assert.Contains(t, text, "+3 -1")
	assert.Contains(t, text, "logo.png")
	assert.Contains(t, text, "binary")
	assert.Contains(t, text, "2 file(s) changed, 3 insertion(s)(+), 1 deletion(s)(-)")
	assert.Contains(t, text, "WARNING")
}
