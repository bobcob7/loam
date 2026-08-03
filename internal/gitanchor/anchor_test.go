package gitanchor

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// sampleRepoID is shared by every test below; only RepoStoreMock's return
// value (the repo name a dataDir/mirrorpath.Dir join resolves against)
// varies per test -- matches internal/gitdiff/diff_test.go's own
// sampleRepoID/computerOver/workBranch trio.
var sampleRepoID = uuid.New()

func checkerOver(dataDir, repoName string) *Checker {
	repos := &RepoStoreMock{
		GetRepoByIDFunc: func(_ context.Context, id uuid.UUID) (reposstore.Repo, error) {
			return reposstore.Repo{ID: id, Name: repoName}, nil
		},
	}
	return New(dataDir, repos)
}

func workBranch(name string) workbranchstore.WorkBranch {
	return workbranchstore.WorkBranch{RepoID: sampleRepoID, Name: name}
}

// TestFileLineCount_ReturnsActualLineCount is the basic case: a file with a
// deliberately unremarkable, non-round line count (not 100, not any number
// a boundary test elsewhere in this tree happens to use) so a mutant that
// returns a constant or an off-by-a-round-number wrong value cannot pass by
// coincidence.
func TestFileLineCount_ReturnsActualLineCount(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.go", linesOf(37), "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := checkerOver(dataDir, "acme/widgets")
	lines, err := c.FileLineCount(t.Context(), workBranch("wb-1"), "f.go")
	require.NoError(t, err)
	assert.Equal(t, 37, lines)
}

// TestFileLineCount_NoTrailingNewline_CountsPartialLastLine proves the
// trailing-partial-line case: a file whose last line has no '\n' still
// counts as a full line, matching what an editor's gutter or `git diff`'s
// own line numbers would show. Without this case, a lineCounter that
// simply counted '\n' bytes (off by one for exactly this shape) would pass
// every other test in this file.
func TestFileLineCount_NoTrailingNewline_CountsPartialLastLine(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.go", "one\ntwo\nthree", "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := checkerOver(dataDir, "acme/widgets")
	lines, err := c.FileLineCount(t.Context(), workBranch("wb-1"), "f.go")
	require.NoError(t, err)
	assert.Equal(t, 3, lines, "the trailing line with no newline still counts")
}

// TestFileLineCount_EmptyFile_ReturnsZero proves a genuinely empty file
// reports zero lines, not one -- an empty blob has zero bytes, so
// lineCounter must not treat "wrote nothing" the same as "wrote one empty
// line".
func TestFileLineCount_EmptyFile_ReturnsZero(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "empty.txt", "", "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := checkerOver(dataDir, "acme/widgets")
	lines, err := c.FileLineCount(t.Context(), workBranch("wb-1"), "empty.txt")
	require.NoError(t, err)
	assert.Equal(t, 0, lines)
}

// TestFileLineCount_FileNotPresentAtTip_ReturnsErrFileNotFound proves a
// path the diff never touched -- never committed on the work branch at
// all -- is reported as ErrFileNotFound, distinguishable from a mirror or
// ref problem.
func TestFileLineCount_FileNotPresentAtTip_ReturnsErrFileNotFound(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.go", linesOf(10), "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := checkerOver(dataDir, "acme/widgets")
	_, err := c.FileLineCount(t.Context(), workBranch("wb-1"), "never-committed.go")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFileNotFound)
}

// TestFileLineCount_PathIsADirectory_ReturnsErrFileNotFound proves a path
// naming a real tree entry that is a DIRECTORY, not a blob, is rejected the
// same way a missing path is -- this is the isBlob type-check's whole
// reason to exist: `git cat-file -p <ref>:<dir>` does not fail on a tree,
// it pretty-prints its entries, so without this check a comment anchored to
// a directory would silently report that listing's own line count instead
// of failing.
func TestFileLineCount_PathIsADirectory_ReturnsErrFileNotFound(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "pkg/f.go", linesOf(5), "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := checkerOver(dataDir, "acme/widgets")
	_, err := c.FileLineCount(t.Context(), workBranch("wb-1"), "pkg")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFileNotFound)
}

// TestFileLineCount_MirrorMissingOnDisk_ReturnsErrMirrorMissing proves a
// repo whose bare mirror was never cloned (or whose path is stale) fails as
// ErrMirrorMissing, not a generic error nor a misleading zero line count.
func TestFileLineCount_MirrorMissingOnDisk_ReturnsErrMirrorMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir() // no mirrors/ directory ever created under it
	c := checkerOver(dataDir, "acme/widgets")
	_, err := c.FileLineCount(t.Context(), workBranch("wb-1"), "f.go")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMirrorMissing)
}

// TestFileLineCount_WorkBranchRefMissing_ReturnsErrRefMissing proves a
// mirror that exists but has no ref for the named work branch (fallen out
// of sync with the work-branch registry) is reported as ErrRefMissing, an
// operational fault distinguishable from ErrFileNotFound -- neither of
// which loam-hi5o.15's out-of-range-line rejection must ever be confused
// with, since the three are mapped to different Connect codes at the
// handler.
func TestFileLineCount_WorkBranchRefMissing_ReturnsErrRefMissing(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.go", linesOf(10), "init")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	// Deliberately no seedWorkBranchRef: the mirror has main, but no
	// refs/heads/loam-reserved/wb-1 at all.
	c := checkerOver(dataDir, "acme/widgets")
	_, err := c.FileLineCount(t.Context(), workBranch("wb-1"), "f.go")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMissing)
}
