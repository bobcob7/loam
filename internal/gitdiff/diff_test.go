package gitdiff

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
// varies per test.
var sampleRepoID = uuid.New()

// computerOver builds a Computer rooted at dataDir whose RepoStore always
// resolves sampleRepoID to repoName, regardless of the id passed in --
// every test here constructs exactly one work branch's worth of fixture,
// so a fixed mapping is enough and keeps each test's setup to the point.
func computerOver(dataDir, repoName string) *Computer {
	repos := &RepoStoreMock{
		GetRepoByIDFunc: func(_ context.Context, id uuid.UUID) (reposstore.Repo, error) {
			return reposstore.Repo{ID: id, Name: repoName}, nil
		},
	}
	return New(dataDir, repos)
}

// workBranch builds the workbranchstore.WorkBranch Diff needs: RepoID
// (resolved by computerOver's RepoStore), Target, and Name -- the three
// fields Diff actually reads.
func workBranch(target, name string) workbranchstore.WorkBranch {
	return workbranchstore.WorkBranch{RepoID: sampleRepoID, Target: target, Name: name}
}

// TestDiff_HappyPath_ReturnsWorkBranchsOwnChange proves the basic case
// against a real bare mirror: a work branch with one commit on top of its
// target returns that commit's unified diff.
func TestDiff_HappyPath_ReturnsWorkBranchsOwnChange(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.txt", "one\n", "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	writeAndCommit(t, src, "f.txt", "one\ntwo\n", "wb-1 adds a line")
	runGit(t, src, "checkout", "--quiet", "main")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := computerOver(dataDir, "acme/widgets")
	diff, err := c.Diff(t.Context(), workBranch("main", "wb-1"))
	require.NoError(t, err)
	assert.Contains(t, diff, "+two")
	assert.Contains(t, diff, "--- a/f.txt")
	assert.Contains(t, diff, "+++ b/f.txt")
}

// TestDiff_NoDivergence_ReturnsEmptyStringNoError proves an empty diff (the
// work branch's ref is identical to its target's) is a valid, non-error
// result -- the empty string -- not mistaken for a failure.
func TestDiff_NoDivergence_ReturnsEmptyStringNoError(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.txt", "one\n", "init")
	runGit(t, src, "branch", "wb-1")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := computerOver(dataDir, "acme/widgets")
	diff, err := c.Diff(t.Context(), workBranch("main", "wb-1"))
	require.NoError(t, err)
	assert.Empty(t, diff)
}

// TestDiff_ThreeDotExcludesTargetOnlyChanges_TwoDotWouldNotHave is THE test
// that would notice a three-dot-for-two-dot swap. It builds a fixture
// where the two forms genuinely differ, not just differently-worded output
// for the same content:
//
//   - main and wb-1 fork from a common commit.
//   - wb-1 then adds its OWN change (marked wbOwnChangeMarker below).
//   - main independently advances with a DIFFERENT, unrelated change
//     (marked targetOnlyChangeMarker) that wb-1 never merged.
//
// Three-dot (`main...wb-1`, the merge base) diffs only from the fork
// point, so it contains wb-1's own change and NOT main's independent
// advance. Two-dot (`main..wb-1`) diffs the two tips directly, so it would
// ALSO show targetOnlyChangeMarker's file reverting -- content that
// genuinely differs between the two forms, verified against real git, not
// asserted from documentation alone. A mutant that swaps "..." for ".."
// in runDiff's args would make this test's "must not contain" assertion
// fail, not panic.
func TestDiff_ThreeDotExcludesTargetOnlyChanges_TwoDotWouldNotHave(t *testing.T) {
	t.Parallel()
	const wbOwnChangeMarker = "WB_OWN_CHANGE"
	const targetOnlyChangeMarker = "TARGET_ONLY_CHANGE"
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "shared.txt", "base\n", "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	writeAndCommit(t, src, "wb.txt", wbOwnChangeMarker+"\n", "wb-1's own change")
	runGit(t, src, "checkout", "--quiet", "main")
	writeAndCommit(t, src, "target.txt", targetOnlyChangeMarker+"\n", "main's own, independent advance")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := computerOver(dataDir, "acme/widgets")
	diff, err := c.Diff(t.Context(), workBranch("main", "wb-1"))
	require.NoError(t, err)
	assert.Contains(t, diff, wbOwnChangeMarker, "three-dot must show the work branch's own change")
	assert.NotContains(t, diff, targetOnlyChangeMarker, "three-dot must NOT show target's independent advance -- a two-dot diff would")

	// Compute the two-dot form directly with real git (not this package's
	// own code) to prove the two forms genuinely differ for this fixture,
	// rather than trusting that assumption undemonstrated.
	twoDot := runGit(t, "", "--git-dir="+mirrorpath.Dir(dataDir, "acme/widgets"), "diff", "main..refs/heads/loam-reserved/wb-1")
	assert.Contains(t, twoDot, targetOnlyChangeMarker, "two-dot diff over the same fixture DOES show target's independent advance, proving the forms differ")
}

// TestDiff_UnrelatedHistories_NoMergeBase proves the "no merge base"
// condition (target and name share no common ancestor) is surfaced as
// gitdiff.ErrNoMergeBase, not a generic or panicking failure -- verified
// empirically: `git diff a...b` on unrelated histories exits 128 with
// "fatal: a...b: no merge base" on real git.
func TestDiff_UnrelatedHistories_NoMergeBase(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.txt", "one\n", "init")
	runGit(t, src, "checkout", "--quiet", "--orphan", "wb-1")
	runGit(t, src, "rm", "-rf", "--quiet", ".")
	writeAndCommit(t, src, "g.txt", "unrelated\n", "unrelated history")
	runGit(t, src, "checkout", "--quiet", "main")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := computerOver(dataDir, "acme/widgets")
	_, err := c.Diff(t.Context(), workBranch("main", "wb-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoMergeBase)
}

// TestDiff_MirrorMissingOnDisk_ReturnsErrMirrorMissing proves a repo
// enrolled in the store but whose bare mirror is absent on disk (never
// cloned, or a stale path) fails as ErrMirrorMissing, not a generic error
// or an empty, misleadingly-successful diff.
func TestDiff_MirrorMissingOnDisk_ReturnsErrMirrorMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir() // no mirrors/ directory ever created under it
	c := computerOver(dataDir, "acme/widgets")
	_, err := c.Diff(t.Context(), workBranch("main", "wb-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMirrorMissing)
}

// TestDiff_TargetRefMissing_ReturnsErrRefMissing proves a work branch
// whose recorded target no longer names a ref in the mirror (e.g. deleted
// upstream, or the mirror has fallen behind the registry) fails as
// ErrRefMissing rather than a bare git error or a silently empty diff.
func TestDiff_TargetRefMissing_ReturnsErrRefMissing(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.txt", "one\n", "init")
	runGit(t, src, "branch", "wb-1")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := computerOver(dataDir, "acme/widgets")
	_, err := c.Diff(t.Context(), workBranch("no-such-target", "wb-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMissing)
}

// TestDiff_WorkBranchRefMissing_ReturnsErrRefMissing is
// TestDiff_TargetRefMissing_ReturnsErrRefMissing's sibling for the work
// branch's own ref. Since loam-5iu `work start` creates that ref
// server-side, so this is no longer the commonly-reachable shape of
// ErrRefMissing it once was -- it now means the mirror has fallen out of
// step with the work-branch registry, which is exactly what ErrRefMissing
// is documented to signal.
func TestDiff_WorkBranchRefMissing_ReturnsErrRefMissing(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.txt", "one\n", "init")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	c := computerOver(dataDir, "acme/widgets")
	_, err := c.Diff(t.Context(), workBranch("main", "wb-does-not-exist"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMissing)
}

// TestDiff_UsesGitDirNotDashC_UpwardDiscoveryHazard reproduces, against
// real git, the exact hazard loam-ofg.19's review established for `git -C`
// (internal/mirrorreconcile/reconcile.go's own doc comments carry the same
// citation): given a directory that EXISTS but is not itself a valid git
// repository, `-C` chdirs into it and then walks UP looking for an
// enclosing repository, silently operating on whatever it finds there
// instead of failing. `--git-dir` never does this.
//
// outer is a real, unrelated git repo one level above the "mirror" path,
// with branches of the SAME names (main, wb-1) as the ones this test
// diffs, but different, marked content -- so a mutant that swaps
// --git-dir for -C in run's argv would not just behave differently, it
// would SUCCEED, silently returning outer's wrong diff instead of this
// package's correct ErrMirrorMissing. That is why this must be caught by
// an assertion on the returned error, not a panic: the mutant's failure
// mode is a plausible-looking wrong answer, not a crash.
func TestDiff_UsesGitDirNotDashC_UpwardDiscoveryHazard(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	newWorkingRepo(t, outer)
	writeAndCommit(t, outer, "f.txt", "outer main\n", "outer main")
	runGit(t, outer, "checkout", "--quiet", "-b", "wb-1")
	writeAndCommit(t, outer, "f.txt", "outer main\nWRONG_REPO_MARKER\n", "outer wb-1")
	runGit(t, outer, "checkout", "--quiet", "main")
	// outer carries the work branch at the RESERVED ref path too, so a -C
	// mutant escaping into it would resolve every ref this diff needs and
	// return outer's wrong answer. Without this the mutant would instead
	// die on ErrRefMissing, and the test would still pass -- for the wrong
	// reason, killing nothing.
	seedWorkBranchRef(t, filepath.Join(outer, ".git"), "wb-1")
	// mirrorDir exists as a plain directory nested inside outer, but is
	// not itself a git repository -- exactly the shape mirrorpath.Dir
	// produces for a repo enrolled in the store whose mirror was never
	// actually cloned (or a stale/interrupted enrollment), and exactly
	// the shape -C's upward walk would escape into outer from.
	mirrorDir := filepath.Join(outer, "mirrors", "acme", "widgets.git")
	require.NoError(t, os.MkdirAll(mirrorDir, 0o755))
	c := computerOver(outer, "acme/widgets")
	diff, err := c.Diff(t.Context(), workBranch("main", "wb-1"))
	require.Error(t, err, "a correct --git-dir addressing must fail on a non-repository path instead of silently escaping to the enclosing repo")
	assert.ErrorIs(t, err, ErrMirrorMissing)
	assert.Empty(t, diff)
	assert.NotContains(t, diff, "WRONG_REPO_MARKER")
}

// TestDiff_TruncatesOversizedDiffWithVisibleMarker proves the truncation
// cap: GetWorkBranchDiffResponse (proto/loam/v1/workbranch.proto) carries
// only a bare `string diff` field, with no sibling `truncated` bool the
// way ListWorkBranchesResponse and the graph RPCs have, so a capped diff
// must signal truncation IN the text itself. maxDiffBytes is overridden
// here (a package-level var, not a const, exactly so a whitebox test can
// shrink it) rather than actually generating a multi-megabyte fixture.
func TestDiff_TruncatesOversizedDiffWithVisibleMarker(t *testing.T) {
	old := maxDiffBytes
	maxDiffBytes = 100
	t.Cleanup(func() { maxDiffBytes = old })
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.txt", "base\n", "init")
	runGit(t, src, "checkout", "--quiet", "-b", "wb-1")
	// A single long line comfortably exceeds the 100-byte cap above, with
	// a unique marker positioned at the very END of the diff's content --
	// present only if the full, untruncated diff reached the assertion.
	big := strings.Repeat("x", 500) + "\nEND_OF_DIFF_MARKER\n"
	writeAndCommit(t, src, "f.txt", "base\n"+big, "wb-1 adds a large change")
	runGit(t, src, "checkout", "--quiet", "main")
	dataDir := t.TempDir()
	bareCloneInto(t, src, mirrorpath.Dir(dataDir, "acme/widgets"))
	seedWorkBranchRef(t, mirrorpath.Dir(dataDir, "acme/widgets"), "wb-1")
	c := computerOver(dataDir, "acme/widgets")
	diff, err := c.Diff(t.Context(), workBranch("main", "wb-1"))
	require.NoError(t, err)
	assert.NotContains(t, diff, "END_OF_DIFF_MARKER", "an ignored cap would let the full diff, including its tail marker, through")
	assert.Contains(t, diff, "truncated at 100 bytes", "a truncated response must say so in the text itself -- the proto carries no separate truncated field")
}

// TestVerifyRef_MirrorMissing_ClassifiesGitsOwnStderr proves verifyRef's
// classification directly: a bad --git-dir produces git's own "not a git
// repository" stderr, which must map to ErrMirrorMissing, not bubble up
// as an opaque wrapped git failure.
func TestVerifyRef_MirrorMissing_ClassifiesGitsOwnStderr(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), nil)
	err := c.verifyRef(t.Context(), filepath.Join(t.TempDir(), "does-not-exist.git"), "main", "refs/heads/main")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMirrorMissing))
}

// TestRunDiff_MirrorMissing_ClassifiesGitsDifferentWordingForDiff proves
// runDiff's own ErrMirrorMissing classification, exercised directly
// (bypassing verifyRef, which would otherwise always catch a missing
// mirror first in the real Diff() call path -- this covers the narrower
// TOCTOU window where the mirror disappears between verifyRef and
// runDiff). This is the case isMirrorMissingStderr's case-insensitive
// match exists for: verified empirically, `git diff` against a bad
// --git-dir misparses the whole invocation as `diff --no-index` (see
// package doc comment) and says "warning: Not a git repository" --
// capital N, unlike rev-parse's lowercase "fatal: not a git repository".
// A case-sensitive Contains check here would silently miss this and fall
// through to a generic wrapped error instead.
func TestRunDiff_MirrorMissing_ClassifiesGitsDifferentWordingForDiff(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), nil)
	_, err := c.runDiff(t.Context(), filepath.Join(t.TempDir(), "does-not-exist.git"), "refs/heads/main", "refs/heads/loam-reserved/wb-1", "wb-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMirrorMissing)
}
