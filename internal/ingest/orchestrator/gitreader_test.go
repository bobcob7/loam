package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestMirror builds a real git repository containing files and returns
// the path to use as a --git-dir. Real git is used rather than a fake:
// every hazard this reader exists to handle (NUL bytes in a blob, a
// newline in a path, a gitlink that is not a blob, the exact byte framing
// of `cat-file --batch`) is a property of git's own output, so a stub
// would only test this package's assumptions about git rather than git.
func newTestMirror(t *testing.T, files map[string]string) string {
	t.Helper()
	work := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=loam", "GIT_AUTHOR_EMAIL=loam@example.invalid",
			"GIT_COMMITTER_NAME=loam", "GIT_COMMITTER_EMAIL=loam@example.invalid",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+work,
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "--initial-branch=main")
	for name, content := range files {
		path := filepath.Join(work, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	run("add", "-A")
	run("commit", "-m", "seed")
	return filepath.Join(work, ".git")
}

func TestResolveRef_ReturnsTheCommitTheMirrorHasForTheBranch(t *testing.T) {
	t.Parallel()
	mirror := newTestMirror(t, map[string]string{"a.go": "package a\n"})
	ref, err := newGitReader(testLogger()).ResolveRef(t.Context(), mirror, "main")
	require.NoError(t, err)
	assert.Len(t, ref, 40, "a resolved ref must be a full object id, not a branch name")
	assert.NotContains(t, ref, "\n", "the trailing newline git prints must be trimmed")
}

// TestResolveRef_UnknownBranchIsALabeledError proves an ingest job naming a
// branch the mirror does not have fails with a distinguishable error
// rather than an empty ref that would silently be used as the new tip.
func TestResolveRef_UnknownBranchIsALabeledError(t *testing.T) {
	t.Parallel()
	mirror := newTestMirror(t, map[string]string{"a.go": "package a\n"})
	_, err := newGitReader(testLogger()).ResolveRef(t.Context(), mirror, "no-such-branch")
	require.Error(t, err)
	assert.ErrorIs(t, err, errBranchMissing)
}

// TestResolveRef_MissingMirrorIsDistinguishedFromAMissingBranch matters
// operationally: "this repo was never cloned" and "this repo has no such
// branch" call for completely different fixes, and both exit git nonzero.
func TestResolveRef_MissingMirrorIsDistinguishedFromAMissingBranch(t *testing.T) {
	t.Parallel()
	_, err := newGitReader(testLogger()).ResolveRef(t.Context(), filepath.Join(t.TempDir(), "never-cloned.git"), "main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errMirrorMissing)
	assert.NotErrorIs(t, err, errBranchMissing)
}

// TestResolveRef_ResolvesRefsHeadsNotAnAmbiguousShortName proves the
// reader asks for refs/heads/<branch> explicitly. internal/mirrorsync
// fetches upstream branches into refs/heads/*, but a tag of the same name
// can also exist in a mirror -- and `git rev-parse <name>` prefers the
// tag. Indexing a tag's commit while recording it as the branch's
// ingested_ref would make every subsequent incremental diff wrong.
func TestResolveRef_ResolvesRefsHeadsNotAnAmbiguousShortName(t *testing.T) {
	t.Parallel()
	mirror := newTestMirror(t, map[string]string{"a.go": "package a\n"})
	r := newGitReader(testLogger())
	branchRef, err := r.ResolveRef(t.Context(), mirror, "main")
	require.NoError(t, err)
	// Point a TAG named "main" at a different commit than the branch.
	work := filepath.Dir(mirror)
	require.NoError(t, os.WriteFile(filepath.Join(work, "b.go"), []byte("package a\n\nfunc B() {}\n"), 0o644))
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "second"}, {"tag", "main"}, {"reset", "--hard", branchRef}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=loam", "GIT_AUTHOR_EMAIL=loam@example.invalid",
			"GIT_COMMITTER_NAME=loam", "GIT_COMMITTER_EMAIL=loam@example.invalid",
			"GIT_CONFIG_NOSYSTEM=1", "HOME="+work,
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	got, err := r.ResolveRef(t.Context(), mirror, "main")
	require.NoError(t, err)
	assert.Equal(t, branchRef, got, "the BRANCH's commit must win over a same-named tag pointing elsewhere")
}

// TestReadFiles_ReturnsRequestedPathsInOrderWithExactBytes is the reader's
// core contract. The binary fixture is what makes the `cat-file --batch`
// framing load-bearing: its content contains NUL bytes and an embedded
// newline, so any line-oriented parse of the response stream mispairs
// content with paths.
func TestReadFiles_ReturnsRequestedPathsInOrderWithExactBytes(t *testing.T) {
	t.Parallel()
	binary := "\x89PNG\x00\x00\n1234 blob 99\n\x00tail"
	mirror := newTestMirror(t, map[string]string{
		"a.go":            "package a\n\nfunc A() {}\n",
		"docs/README.md":  "# Hi\n",
		"assets/logo.png": binary,
	})
	files, err := newGitReader(testLogger()).ReadFiles(t.Context(), mirror, "main", []string{"assets/logo.png", "a.go", "docs/README.md"})
	require.NoError(t, err)
	require.Len(t, files, 3, "every requested path that is a blob must come back")
	assert.Equal(t, "assets/logo.png", files[0].Path, "results must be in the order requested, since chunks are paired to paths by position")
	assert.Equal(t, binary, string(files[0].Content), "blob bytes must survive exactly, NULs and embedded header-lookalikes included")
	assert.Equal(t, "a.go", files[1].Path)
	assert.Equal(t, "package a\n\nfunc A() {}\n", string(files[1].Content))
	assert.Equal(t, "docs/README.md", files[2].Path)
}

// TestReadFiles_PathContainingANewlineIsReadCorrectly is the -z rationale
// made falsifiable. git C-quotes such a path in non -z output, and a
// newline-delimited cat-file request stream would split it into two bogus
// requests, desynchronizing every response after it from its path.
func TestReadFiles_PathContainingANewlineIsReadCorrectly(t *testing.T) {
	t.Parallel()
	weird := "weird\nname.txt"
	mirror := newTestMirror(t, map[string]string{
		weird:  "content behind a newline\n",
		"z.go": "package z\n",
	})
	files, err := newGitReader(testLogger()).ReadFiles(t.Context(), mirror, "main", []string{weird, "z.go"})
	require.NoError(t, err)
	require.Len(t, files, 2, "a path with a literal newline must still be found and read")
	assert.Equal(t, weird, files[0].Path)
	assert.Equal(t, "content behind a newline\n", string(files[0].Content))
	assert.Equal(t, "package z\n", string(files[1].Content), "the file AFTER the weird path must still get its own content, not a shifted one")
}

// TestReadFiles_PathThatIsNoLongerInTheTreeIsSkippedNotFatal proves the
// reader tolerates the plan being a snapshot while the mirror is live: a
// path that vanished between planning and reading simply has nothing to
// reparse, and must not fail an otherwise good ingest.
func TestReadFiles_PathThatIsNoLongerInTheTreeIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	mirror := newTestMirror(t, map[string]string{"a.go": "package a\n"})
	files, err := newGitReader(testLogger()).ReadFiles(t.Context(), mirror, "main", []string{"a.go", "vanished.go"})
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "a.go", files[0].Path)
}

// TestReadFiles_EmptyPathsMakesNoGitCall proves the common incremental
// ingest with nothing to reparse costs no subprocess. A missing mirror
// directory is used as the trip-wire: any git invocation against it would
// fail, so returning cleanly is itself the proof none happened.
func TestReadFiles_EmptyPathsMakesNoGitCall(t *testing.T) {
	t.Parallel()
	files, err := newGitReader(testLogger()).ReadFiles(t.Context(), filepath.Join(t.TempDir(), "never-cloned.git"), "main", nil)
	require.NoError(t, err)
	assert.Empty(t, files)
}

// TestParseLsTreeZ_SkipsNonBlobEntries proves a submodule gitlink (type
// "commit") never reaches cat-file as if it were a blob.
func TestParseLsTreeZ_SkipsNonBlobEntries(t *testing.T) {
	t.Parallel()
	record := strings.Join([]string{
		"100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\ta.go",
		"160000 commit bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tvendor/sub",
		"040000 tree cccccccccccccccccccccccccccccccccccccccc\tdir",
	}, "\x00") + "\x00"
	blobs, err := parseLsTreeZ([]byte(record))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a.go": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, blobs)
}

// TestParseLsTreeZ_MalformedRecordIsAnErrorNotAPanic and its cat-file
// sibling below exist because of loam-337: internal/ingest.Pool has no
// recover(), so a slice index panic anywhere in this call path takes down
// the whole server process instead of failing one job. Every malformed
// shape must therefore leave through the error return.
func TestParseLsTreeZ_MalformedRecordIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{
		"no tab separator":  "100644 blob aaaa a.go\x00",
		"too few fields":    "100644 blob\ta.go\x00",
		"too many fields":   "100644 blob aaaa extra\ta.go\x00",
		"truncated to junk": "\tno-metadata\x00",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseLsTreeZ([]byte(input))
			require.Error(t, err)
			assert.ErrorIs(t, err, errBatchProtocol)
		})
	}
}

func TestParseCatFileBatch_MalformedStreamIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		stdout string
		want   int
	}{
		"empty stream but a response expected": {stdout: "", want: 1},
		"missing object":                       {stdout: "deadbeef missing\n", want: 1},
		"unparseable size":                     {stdout: "deadbeef blob NaN\nxx\n", want: 1},
		"negative size":                        {stdout: "deadbeef blob -1\n\n", want: 1},
		"content shorter than declared size":   {stdout: "deadbeef blob 100\nshort\n", want: 1},
		"missing record terminator":            {stdout: "deadbeef blob 5\nabcde", want: 1},
		"fewer responses than requested":       {stdout: "deadbeef blob 1\nx\n", want: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseCatFileBatch([]byte(tc.stdout), tc.want)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBatchProtocol)
		})
	}
}

// TestParseCatFileBatch_ContentIsFramedBySizeNotByNewlines is the direct
// unit-level pin of the framing rule: a blob whose own bytes look exactly
// like a cat-file header must be read by its declared byte count, not by
// scanning for the next newline.
func TestParseCatFileBatch_ContentIsFramedBySizeNotByNewlines(t *testing.T) {
	t.Parallel()
	payload := "aaaa blob 4\nnext\n"
	stdout := "deadbeef blob " + itoa(len(payload)) + "\n" + payload + "\n" + "cafebabe blob 2\nhi\n"
	contents, err := parseCatFileBatch([]byte(stdout), 2)
	require.NoError(t, err)
	require.Len(t, contents, 2)
	assert.Equal(t, payload, string(contents[0]), "the first blob must be read by size, header-lookalike bytes and all")
	assert.Equal(t, "hi", string(contents[1]), "the second response must still be found at the right offset")
}

// itoa keeps the fixture above readable without pulling strconv into the
// assertion itself.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
