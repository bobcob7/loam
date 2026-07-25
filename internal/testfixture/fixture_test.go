package testfixture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustRev is Repo.Rev with the error handled via require, so call sites
// that only care about the resolved SHA stay uncluttered.
func mustRev(t *testing.T, ctx context.Context, repo *Repo, ref string) string {
	t.Helper()
	sha, err := repo.Rev(ctx, ref)
	require.NoError(t, err)
	return sha
}

func TestNew_SeedsExpectedSymbolGraphFiles(t *testing.T) {
	t.Parallel()
	repo := NewT(t.Context(), t)
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
	repo := NewT(t.Context(), t)
	goSrc, err := os.ReadFile(filepath.Join(repo.Dir(), "pkg/validate/validate.go"))
	require.NoError(t, err)
	tsSrc, err := os.ReadFile(filepath.Join(repo.Dir(), "src/validate.ts"))
	require.NoError(t, err)
	assert.Contains(t, string(goSrc), "func Validate(")
	assert.Contains(t, string(tsSrc), "function Validate(")
}

func TestNew_DocHasMultipleTopLevelSections(t *testing.T) {
	t.Parallel()
	repo := NewT(t.Context(), t)
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
	repo := NewT(t.Context(), t)
	src, err := os.ReadFile(filepath.Join(repo.Dir(), "scripts/parity.py"))
	require.NoError(t, err)
	assert.Contains(t, string(src), "return is_odd(n - 1)")
	assert.Contains(t, string(src), "return is_even(n - 1)")
}

func TestNew_CommitsOnMainBranch(t *testing.T) {
	t.Parallel()
	repo := NewT(t.Context(), t)
	branch, err := runGit(t.Context(), repo.Dir(), "rev-parse", "--abbrev-ref", "HEAD")
	require.NoError(t, err)
	assert.Equal(t, defaultBranch, branch)
	log, err := runGit(t.Context(), repo.Dir(), "log", "--oneline")
	require.NoError(t, err)
	assert.Contains(t, log, "seed: fixture-polyglot initial commit")
}

func TestNew_IsHermeticAgainstHostileGlobalGitConfig(t *testing.T) {
	// Deliberately no t.Parallel(): t.Setenv panics if the test (or an
	// ancestor) calls t.Parallel, since env vars are process-global.
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	hostile := "[commit]\n\tgpgsign = true\n[user]\n\tname = not-the-fixture\n\temail = not-the-fixture@example.com\n"
	require.NoError(t, os.WriteFile(globalConfig, []byte(hostile), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	repo := NewT(t.Context(), t)
	sha := mustRev(t, t.Context(), repo, defaultBranch)
	assert.NotEmpty(t, sha, "commit must succeed despite the ambient GIT_CONFIG_GLOBAL demanding gpgsign")
	author, err := runGit(t.Context(), repo.Dir(), "log", "-1", "--format=%an <%ae>")
	require.NoError(t, err)
	assert.Equal(t, "loam-fixture <fixture@loam.test>", author, "our own identity env must win over the hostile global config")
}

func TestNewBare_IsBareRepository(t *testing.T) {
	t.Parallel()
	repo := NewBareT(t.Context(), t)
	assert.True(t, repo.IsBare())
	out, err := runGitIn(t.Context(), repo.Dir(), nil, nil, "rev-parse", "--is-bare-repository")
	require.NoError(t, err)
	assert.Equal(t, "true", out)
	assert.NotEmpty(t, mustRev(t, t.Context(), repo, defaultBranch))
}

func TestNewBare_IsIndependentOfOtherBareRepos(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repoA := NewBareT(ctx, t)
	repoB := NewBareT(ctx, t)
	require.NotEqual(t, repoA.Dir(), repoB.Dir())
	shaBeforeB := mustRev(t, ctx, repoB, defaultBranch)
	newTipA, err := repoA.Advance(ctx, defaultBranch)
	require.NoError(t, err)
	assert.Equal(t, shaBeforeB, mustRev(t, ctx, repoB, defaultBranch), "mutating repoA's bare repo must not move repoB's branch")
	other := exec.CommandContext(ctx, "git", "--git-dir="+repoB.Dir(), "cat-file", "-e", newTipA)
	assert.Error(t, other.Run(), "repoB's object store must not contain repoA's new commit")
}

func TestClone_ChecksOutFixtureContent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	src := NewT(ctx, t)
	clone := CloneT(ctx, t, src)
	_, err := os.Stat(filepath.Join(clone.Dir(), "pkg/validate/validate.go"))
	require.NoError(t, err)
	assert.Equal(t, mustRev(t, ctx, src, defaultBranch), mustRev(t, ctx, clone, defaultBranch))
}

func TestClone_IsIndependentOfSourceAbsentAnExplicitPush(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	src := NewT(ctx, t)
	clone := CloneT(ctx, t, src)
	shaBeforeSrc := mustRev(t, ctx, src, defaultBranch)
	// Mutate the clone's local branch directly; nothing pushes it anywhere.
	newTipClone, err := clone.Advance(ctx, defaultBranch)
	require.NoError(t, err)
	assert.Equal(t, shaBeforeSrc, mustRev(t, ctx, src, defaultBranch), "advancing the clone must not move src's branch")
	missingInSrc := exec.CommandContext(ctx, "git", "--git-dir="+src.Dir()+"/.git", "cat-file", "-e", newTipClone)
	assert.Error(t, missingInSrc.Run(), "src's object store must not contain the clone's new commit absent a push")
	// And the reverse: mutating src must not reach back into the clone.
	newTipSrc, err := src.Advance(ctx, defaultBranch)
	require.NoError(t, err)
	assert.NotEqual(t, mustRev(t, ctx, clone, defaultBranch), newTipSrc, "advancing src must not move the clone's checked-out branch")
}

func TestMaterializations_AreIndependent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repoA := NewT(ctx, t)
	repoB := NewT(ctx, t)
	require.NotEqual(t, repoA.Dir(), repoB.Dir())
	shaBeforeA := mustRev(t, ctx, repoA, defaultBranch)
	shaBeforeB := mustRev(t, ctx, repoB, defaultBranch)
	assert.Equal(t, shaBeforeA, shaBeforeB, "two fresh materializations should start from an identical seed commit")
	newTipA, err := repoA.Advance(ctx, defaultBranch)
	require.NoError(t, err)
	assert.Equal(t, newTipA, mustRev(t, ctx, repoA, defaultBranch), "repoA's branch must move to the new commit")
	assert.Equal(t, shaBeforeB, mustRev(t, ctx, repoB, defaultBranch), "mutating repoA must not move repoB's branch")
	other := exec.CommandContext(ctx, "git", "cat-file", "-e", newTipA)
	other.Dir = repoB.Dir()
	assert.Error(t, other.Run(), "repoB's object store must not contain repoA's new commit")
}

func TestAdvance_FastForwardsBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	oldTip := mustRev(t, ctx, repo, defaultBranch)
	newTip, err := repo.Advance(ctx, defaultBranch)
	require.NoError(t, err)
	assert.NotEqual(t, oldTip, newTip)
	assert.Equal(t, newTip, mustRev(t, ctx, repo, defaultBranch))
	isAncestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", oldTip, newTip)
	isAncestor.Dir = repo.Dir()
	assert.NoError(t, isAncestor.Run(), "old tip must be an ancestor of the advanced tip")
}

func TestAdvance_CreatesMissingBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	sha, err := repo.Advance(ctx, "feature/new-branch")
	require.NoError(t, err)
	assert.Equal(t, sha, mustRev(t, ctx, repo, "feature/new-branch"))
}

func TestConflict_ProducesRealMergeConflict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	baseSHA, branchSHA, err := repo.Conflict(ctx, defaultBranch, "feature/conflicting")
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", baseSHA, branchSHA)
	cmd.Dir = repo.Dir()
	out, mergeErr := cmd.CombinedOutput()
	assert.Error(t, mergeErr, "merge-tree should report a conflict, got clean output:\n%s", out)
	assert.Contains(t, string(out), conflictPath)
}

func TestConflict_DoesNotFastForwardCleanly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	common := mustRev(t, ctx, repo, defaultBranch)
	baseSHA, branchSHA, err := repo.Conflict(ctx, defaultBranch, "feature/conflicting")
	require.NoError(t, err)
	assert.NotEqual(t, baseSHA, branchSHA)
	base := exec.CommandContext(ctx, "git", "merge-base", baseSHA, branchSHA)
	base.Dir = repo.Dir()
	mergeBase, err := base.Output()
	require.NoError(t, err)
	assert.Equal(t, common, strings.TrimSpace(string(mergeBase)), "since branch did not previously exist, the merge base is base's pre-Conflict tip")
}

func TestConflict_PreservesBranchsPriorHistory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	// Give the work branch real history before it conflicts, the way an
	// agent pushing commits would (docs/git-spec.md: work-branch refs
	// advance only by agent pushes, never authored by the server).
	workTip, err := repo.Advance(ctx, "feature/has-history")
	require.NoError(t, err)
	_, branchSHA, err := repo.Conflict(ctx, defaultBranch, "feature/has-history")
	require.NoError(t, err)
	isAncestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", workTip, branchSHA)
	isAncestor.Dir = repo.Dir()
	assert.NoError(t, isAncestor.Run(), "branch's prior commit must remain an ancestor of the post-Conflict tip, not be discarded")
}

func TestForcePush_RewritesHistoryMakingOldTipUnreachable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	_, err := repo.Advance(ctx, "feature/rewrite-me")
	require.NoError(t, err)
	oldTip := mustRev(t, ctx, repo, "feature/rewrite-me")
	selfAncestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", oldTip, oldTip)
	selfAncestor.Dir = repo.Dir()
	require.NoError(t, selfAncestor.Run(), "sanity: oldTip must resolve to a real commit before we assert anything about it")
	newTip, err := repo.ForcePush(ctx, "feature/rewrite-me")
	require.NoError(t, err)
	assert.NotEqual(t, oldTip, newTip)
	assert.Equal(t, newTip, mustRev(t, ctx, repo, "feature/rewrite-me"))
	isAncestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", oldTip, newTip)
	isAncestor.Dir = repo.Dir()
	assert.Error(t, isAncestor.Run(), "old tip must not be an ancestor of the rewritten tip")
	selfMergeBase := exec.CommandContext(ctx, "git", "merge-base", oldTip, oldTip)
	selfMergeBase.Dir = repo.Dir()
	require.NoError(t, selfMergeBase.Run(), "sanity: oldTip must be a resolvable object before we assert it has no merge base with newTip")
	mergeBase := exec.CommandContext(ctx, "git", "merge-base", oldTip, newTip)
	mergeBase.Dir = repo.Dir()
	assert.Error(t, mergeBase.Run(), "there should be no valid merge base between old and rewritten history")
	branches, err := runGit(ctx, repo.Dir(), "branch", "--all", "--contains", oldTip)
	require.NoError(t, err)
	assert.Empty(t, branches, "no branch should still contain the rewritten-away tip")
}

func TestDeleteBranch_RemovesTheRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	_, err := repo.Advance(ctx, "feature/to-delete")
	require.NoError(t, err)
	before := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "refs/heads/feature/to-delete")
	before.Dir = repo.Dir()
	require.NoError(t, before.Run(), "sanity: the branch must exist before DeleteBranch is asked to remove it")
	require.NoError(t, repo.DeleteBranch(ctx, "feature/to-delete"))
	after := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "refs/heads/feature/to-delete")
	after.Dir = repo.Dir()
	assert.Error(t, after.Run(), "deleted branch ref must no longer resolve")
}

func TestRename_DetectedAsGitRename(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := NewT(ctx, t)
	parent := mustRev(t, ctx, repo, defaultBranch)
	sha, err := repo.Rename(ctx, defaultBranch, "scripts/parity.py", "scripts/parity_renamed.py")
	require.NoError(t, err)
	assert.Equal(t, sha, mustRev(t, ctx, repo, defaultBranch))
	out, err := runGit(ctx, repo.Dir(), "diff", "--name-status", "-M", parent, sha)
	require.NoError(t, err)
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
	repo := NewT(ctx, t)
	sha, err := repo.Rename(ctx, defaultBranch, "scripts/parity.py", "scripts/parity_renamed.py")
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", sha+":scripts/parity.py")
	cmd.Dir = repo.Dir()
	assert.Error(t, cmd.Run(), "old path must be gone from the renamed-to tree")
	present := exec.CommandContext(ctx, "git", "cat-file", "-e", sha+":scripts/parity_renamed.py")
	present.Dir = repo.Dir()
	assert.NoError(t, present.Run(), "new path must be present in the renamed-to tree")
}
