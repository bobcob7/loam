package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// TestPathFromDiffGitLine_EqualHalvesScanBeatsTheLastSeparator is the
// review's finding: pathFromDiffGitLine's equal-halves scan and
// computeDiffStat's `+++ b/` override MASK EACH OTHER. For every ordinary
// diff both routes give the same answer, so deleting either one changes
// nothing observable -- the same `" b/"`-in-path class the `rename to`
// branch turned out to have, one level down.
//
// This is the one shape that separates them: a plain (non-rename) change to
// a path that itself contains `" b/"`. The equal-halves scan finds the
// correct split because both halves match; the last-separator fallback
// underneath it does not. Asserting on pathFromDiffGitLine DIRECTLY is what
// makes this a test of the scan rather than of the `+++` override, which
// would otherwise supply the right answer through computeDiffStat no matter
// what this function returned.
func TestPathFromDiffGitLine_EqualHalvesScanBeatsTheLastSeparator(t *testing.T) {
	t.Parallel()
	const path = "docs/section b/notes.md"
	rest := "a/" + path + " b/" + path
	require.NotEqual(t, path, rest[strings.LastIndex(rest, " b/")+3:], "precondition: the last-separator fallback gets this wrong, so the equal-halves scan is what is under test")
	assert.Equal(t, path, pathFromDiffGitLine(rest))
}

// TestComputeDiffStat_PlusPlusPlusHeaderNamesACopiedFile is the other half
// of the masking pair, and it runs on BYTE-FOR-BYTE REAL GIT OUTPUT rather
// than a typed fixture. That is not fastidiousness: a hand-typed `+++ b/`
// line differs from git's in a way that hid a real defect for two rounds
// (see TestComputeDiffStat_RealGitOutput_SpacePathsCarryNoTrailingTab), and
// this file has now been bitten twice by fixtures that could not fail.
//
// An earlier version of this test justified the `+++ b/` override by
// claiming git emits no `rename to` for a rename that also changes content.
// THAT CLAIM IS FALSE -- verified: a 50%-similarity rename emits `rename
// from`/`rename to` exactly as a pure rename does, so the override was
// being defended on a premise that does not hold. The real, checkable
// ground is COPY detection:
//
//	diff --git a/src.md b/docs/section b/notes.md
//	copy from src.md
//	copy to docs/section b/notes.md
//	+++ b/docs/section b/notes.md
//
// pathFromDiffGitLine cannot split that header (the halves differ, so the
// equal-halves scan declines, and the last " b/" lands inside the
// destination itself), computeDiffStat handles `rename to` but NOT `copy
// to`, and so `+++ b/` is the only route to the right name. Delete it and
// this file is reported as "notes.md".
//
// Known residual, stated rather than papered over: a 100%-similarity COPY
// has no hunk and therefore no `+++` line at all, so a copy of that kind
// into a path containing " b/" would still be misnamed. Handling `copy to`
// would close it -- and would also make this override redundant again. It
// is left alone because plain `git diff` (what internal/gitdiff runs) does
// not detect copies at all without -C, so nothing loam produces today can
// reach either case.
func TestComputeDiffStat_PlusPlusPlusHeaderNamesACopiedFile(t *testing.T) {
	t.Parallel()
	const dst = "docs/section b/notes.md"
	repo := newProbeRepo(t)
	writeProbeFile(t, repo, "src.md", "one\ntwo\nthree\nfour\nfive\n")
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "--quiet", "-m", "base")
	writeProbeFile(t, repo, dst, "one\ntwo\nthree\nfour\nFIVE\n")
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "--quiet", "-m", "copied")
	diff := probeDiff(t, repo, "-C", "--find-copies-harder", "HEAD~1", "HEAD")

	require.Contains(t, diff, "copy to "+dst, "precondition: git must actually have detected a copy, or this proves nothing")
	require.NotContains(t, diff, "rename to ", "precondition: a copy, not a rename -- `rename to` is handled and would answer instead")
	header := strings.TrimPrefix(strings.SplitN(diff, "\n", 2)[0], "diff --git ")
	require.NotEqual(t, dst, pathFromDiffGitLine(header), "precondition: the diff --git line alone cannot name this file, so the +++ header is what is under test")

	stat := computeDiffStat(diff)
	require.Len(t, stat.Files, 1)
	assert.Equal(t, dst, stat.Files[0].Path)
}

// TestComputeDiffStat_RealGitOutput_SpacePathsCarryNoTrailingTab pins the
// defect a reviewer found in the shipped code: GIT APPENDS A TAB to the
// `---`/`+++` header lines when the path contains a space (it is git's own
// disambiguation, since those lines are otherwise space-delimited).
//
// So for every space-containing path the `+++ b/` override did not merely
// duplicate the other routes -- it OVERWROTE A CORRECT ANSWER WITH A WRONG
// ONE, reporting "docs/my notes.md\t" where the diff --git fallback had
// already produced "docs/my notes.md". `work diff --stat`'s per-file rows
// were wrong for a whole class of real paths.
//
// It survived two rounds and a mutation battery for one reason: every
// fixture for that branch was TYPED, and a typed `+++ b/` line has no tab.
// A test that cannot reproduce the input cannot find the bug in it, which
// is why this one shells out to git and feeds the bytes straight through.
func TestComputeDiffStat_RealGitOutput_SpacePathsCarryNoTrailingTab(t *testing.T) {
	t.Parallel()
	const path = "docs/my notes.md"
	repo := newProbeRepo(t)
	writeProbeFile(t, repo, path, "alpha\nbeta\n")
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "--quiet", "-m", "base")
	writeProbeFile(t, repo, path, "alpha\nGAMMA\n")
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "--quiet", "-m", "modified")
	diff := probeDiff(t, repo, "HEAD~1", "HEAD")

	require.Contains(t, diff, "+++ b/"+path+"\t", "precondition: real git must be emitting the trailing tab this test exists for")

	stat := computeDiffStat(diff)
	require.Len(t, stat.Files, 1)
	assert.Equal(t, path, stat.Files[0].Path, "the reported path must not carry git's disambiguating tab")
	assert.Equal(t, diffStatFile{Path: path, Insertions: 1, Deletions: 1}, stat.Files[0])
}

// newProbeRepo initializes a real, isolated git working repo with an author
// identity configured, so mustRunGit can commit in it.
func newProbeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustRunGit(t, dir, "init", "--quiet", "-b", "main")
	mustRunGit(t, dir, "config", "user.name", "fixture")
	mustRunGit(t, dir, "config", "user.email", "fixture@example.com")
	return dir
}

// writeProbeFile writes content at rel inside repo, creating parent
// directories. rel may contain spaces -- that is the point.
func writeProbeFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

// probeDiff returns `git diff <args...>` output VERBATIM. It deliberately
// does not go through mustRunGit, which trims: the trailing bytes of a diff
// are exactly what these tests are about.
func probeDiff(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--no-pager", "diff", "--no-ext-diff"}, args...)...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	require.NoError(t, err)
	return string(out)
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
