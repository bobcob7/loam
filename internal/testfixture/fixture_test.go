package testfixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_SeedsExpectedSymbolGraphFiles(t *testing.T) {
	t.Parallel()
	repo := New(t.Context(), t)
	for _, rel := range []string{
		"pkg/validate/validate.go",
		"pkg/report/report.go",
		"src/validate.ts",
		"src/index.ts",
		"scripts/parity.py",
		"docs/OVERVIEW.md",
	} {
		_, err := os.Stat(filepath.Join(repo.Dir(), rel))
		require.NoErrorf(t, err, "expected seeded file %s", rel)
	}
}

func TestNew_AmbiguousSymbolNameSharedAcrossLanguages(t *testing.T) {
	t.Parallel()
	repo := New(t.Context(), t)
	goSrc, err := os.ReadFile(filepath.Join(repo.Dir(), "pkg/validate/validate.go"))
	require.NoError(t, err)
	tsSrc, err := os.ReadFile(filepath.Join(repo.Dir(), "src/validate.ts"))
	require.NoError(t, err)
	assert.Contains(t, string(goSrc), "func Validate(")
	assert.Contains(t, string(tsSrc), "function Validate(")
}

func TestNew_DocHasMultipleTopLevelSections(t *testing.T) {
	t.Parallel()
	repo := New(t.Context(), t)
	doc, err := os.ReadFile(filepath.Join(repo.Dir(), "docs/OVERVIEW.md"))
	require.NoError(t, err)
	sections := 0
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, "## ") {
			sections++
		}
	}
	assert.GreaterOrEqual(t, sections, 2)
}

func TestNew_MutualRecursionCycle(t *testing.T) {
	t.Parallel()
	repo := New(t.Context(), t)
	src, err := os.ReadFile(filepath.Join(repo.Dir(), "scripts/parity.py"))
	require.NoError(t, err)
	assert.Contains(t, string(src), "return is_odd(n - 1)")
	assert.Contains(t, string(src), "return is_even(n - 1)")
}

func TestNew_CommitsOnMainBranch(t *testing.T) {
	t.Parallel()
	repo := New(t.Context(), t)
	branch := runGit(t.Context(), t, repo.Dir(), "rev-parse", "--abbrev-ref", "HEAD")
	assert.Equal(t, defaultBranch, branch)
	log := runGit(t.Context(), t, repo.Dir(), "log", "--oneline")
	assert.Contains(t, log, "seed: fixture-polyglot initial commit")
}

func TestNewBare_IsBareRepository(t *testing.T) {
	t.Parallel()
	repo := NewBare(t.Context(), t)
	assert.True(t, repo.IsBare())
	out := runGitIn(t.Context(), t, repo.Dir(), nil, nil, "rev-parse", "--is-bare-repository")
	assert.Equal(t, "true", out)
	assert.NotEmpty(t, repo.Rev(t.Context(), t, defaultBranch))
}

func TestClone_ChecksOutFixtureContent(t *testing.T) {
	t.Parallel()
	src := New(t.Context(), t)
	clone := Clone(t.Context(), t, src)
	_, err := os.Stat(filepath.Join(clone.Dir(), "pkg/validate/validate.go"))
	require.NoError(t, err)
	assert.Equal(t, src.Rev(t.Context(), t, defaultBranch), clone.Rev(t.Context(), t, defaultBranch))
}

func TestMaterializations_AreIndependent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repoA := New(ctx, t)
	repoB := New(ctx, t)
	require.NotEqual(t, repoA.Dir(), repoB.Dir())
	shaBeforeA := repoA.Rev(ctx, t, defaultBranch)
	shaBeforeB := repoB.Rev(ctx, t, defaultBranch)
	assert.Equal(t, shaBeforeA, shaBeforeB, "two fresh materializations should start from an identical seed commit")
	newTipA := repoA.Advance(ctx, t, defaultBranch)
	assert.Equal(t, newTipA, repoA.Rev(ctx, t, defaultBranch), "repoA's branch must move to the new commit")
	assert.Equal(t, shaBeforeB, repoB.Rev(ctx, t, defaultBranch), "mutating repoA must not move repoB's branch")
	other := exec.CommandContext(ctx, "git", "cat-file", "-e", newTipA)
	other.Dir = repoB.Dir()
	assert.Error(t, other.Run(), "repoB's object store must not contain repoA's new commit")
}

func TestAdvance_FastForwardsBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	oldTip := repo.Rev(ctx, t, defaultBranch)
	newTip := repo.Advance(ctx, t, defaultBranch)
	assert.NotEqual(t, oldTip, newTip)
	assert.Equal(t, newTip, repo.Rev(ctx, t, defaultBranch))
	isAncestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", oldTip, newTip)
	isAncestor.Dir = repo.Dir()
	assert.NoError(t, isAncestor.Run(), "old tip must be an ancestor of the advanced tip")
}

func TestAdvance_CreatesMissingBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	sha := repo.Advance(ctx, t, "feature/new-branch")
	assert.Equal(t, sha, repo.Rev(ctx, t, "feature/new-branch"))
}

func TestConflict_ProducesRealMergeConflict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	baseSHA, branchSHA := repo.Conflict(ctx, t, defaultBranch, "feature/conflicting")
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", baseSHA, branchSHA)
	cmd.Dir = repo.Dir()
	out, err := cmd.CombinedOutput()
	assert.Error(t, err, "merge-tree should report a conflict, got clean output:\n%s", out)
	assert.Contains(t, string(out), conflictPath)
}

func TestConflict_DoesNotFastForwardCleanly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	common := repo.Rev(ctx, t, defaultBranch)
	baseSHA, branchSHA := repo.Conflict(ctx, t, defaultBranch, "feature/conflicting")
	assert.NotEqual(t, baseSHA, branchSHA)
	base := exec.CommandContext(ctx, "git", "merge-base", baseSHA, branchSHA)
	base.Dir = repo.Dir()
	mergeBase, err := base.Output()
	require.NoError(t, err)
	assert.Equal(t, common, strings.TrimSpace(string(mergeBase)))
}

func TestForcePush_RewritesHistoryMakingOldTipUnreachable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	repo.Advance(ctx, t, "feature/rewrite-me")
	oldTip := repo.Rev(ctx, t, "feature/rewrite-me")
	newTip := repo.ForcePush(ctx, t, "feature/rewrite-me")
	assert.NotEqual(t, oldTip, newTip)
	assert.Equal(t, newTip, repo.Rev(ctx, t, "feature/rewrite-me"))
	isAncestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", oldTip, newTip)
	isAncestor.Dir = repo.Dir()
	assert.Error(t, isAncestor.Run(), "old tip must not be an ancestor of the rewritten tip")
	mergeBase := exec.CommandContext(ctx, "git", "merge-base", oldTip, newTip)
	mergeBase.Dir = repo.Dir()
	assert.Error(t, mergeBase.Run(), "there should be no valid merge base between old and rewritten history")
	branches := runGit(ctx, t, repo.Dir(), "branch", "--all", "--contains", oldTip)
	assert.Empty(t, branches, "no branch should still contain the rewritten-away tip")
}

func TestDeleteBranch_RemovesTheRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	repo.Advance(ctx, t, "feature/to-delete")
	repo.DeleteBranch(ctx, t, "feature/to-delete")
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "refs/heads/feature/to-delete")
	cmd.Dir = repo.Dir()
	assert.Error(t, cmd.Run(), "deleted branch ref must no longer resolve")
}

func TestRename_DetectedAsGitRename(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	parent := repo.Rev(ctx, t, defaultBranch)
	sha := repo.Rename(ctx, t, defaultBranch, "scripts/parity.py", "scripts/parity_renamed.py")
	assert.Equal(t, sha, repo.Rev(ctx, t, defaultBranch))
	out := runGit(ctx, t, repo.Dir(), "diff", "--name-status", "-M", parent, sha)
	lines := strings.Split(out, "\n")
	found := false
	for _, line := range lines {
		if strings.HasPrefix(line, "R") && strings.Contains(line, "scripts/parity.py") && strings.Contains(line, "scripts/parity_renamed.py") {
			found = true
		}
	}
	assert.True(t, found, "expected a rename status line, got:\n%s", out)
}

func TestRename_OldPathGoneFromNewTree(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := New(ctx, t)
	sha := repo.Rename(ctx, t, defaultBranch, "scripts/parity.py", "scripts/parity_renamed.py")
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", sha+":scripts/parity.py")
	cmd.Dir = repo.Dir()
	assert.Error(t, cmd.Run(), "old path must be gone from the renamed-to tree")
	present := exec.CommandContext(ctx, "git", "cat-file", "-e", sha+":scripts/parity_renamed.py")
	present.Dir = repo.Dir()
	assert.NoError(t, present.Run(), "new path must be present in the renamed-to tree")
}
