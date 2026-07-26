package mirrorreconcile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBareMirror creates a real bare git repository under t.TempDir(),
// exactly what a bare mirror looks like on disk before reconciliation.
func newBareMirror(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	cmd := exec.CommandContext(t.Context(), "git", "init", "--quiet", "--bare", dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)
	return dir
}

// gitConfigGet reads back a single config key from repoPath's OWN
// --local scope via a separate `git config --get` invocation -- never
// trusting ReconcileMirror itself for the value under test, per this
// bead's own testing instructions. --local is required, not cosmetic: a
// bare `git config --get` reads git's fully merged scope (system, global,
// local), so a dev machine or CI image with these same keys already set in
// ~/.gitconfig would make a test pass even if ReconcileMirror wrote
// nothing at all to repoPath itself.
func gitConfigGet(t *testing.T, repoPath, key string) (value string, ok bool) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+repoPath, "config", "--local", "--get", key)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// TestReconcileMirror_WritesRequiredConfigAndHook proves the substance of
// this bead: after one call, both required config keys read back exactly
// "true" via a separate git-config invocation, and the hook file exists,
// executable, at the exact path loam-ofg.19's own DESIGN note pins.
func TestReconcileMirror_WritesRequiredConfigAndHook(t *testing.T) {
	t.Parallel()
	repoPath := newBareMirror(t)
	err := ReconcileMirror(t.Context(), repoPath)
	require.NoError(t, err)
	denyNonFF, ok := gitConfigGet(t, repoPath, "receive.denyNonFastForwards")
	require.True(t, ok, "receive.denyNonFastForwards must be set")
	assert.Equal(t, "true", denyNonFF)
	denyDeletes, ok := gitConfigGet(t, repoPath, "receive.denyDeletes")
	require.True(t, ok, "receive.denyDeletes must be set")
	assert.Equal(t, "true", denyDeletes)
	hookPath := filepath.Join(repoPath, "hooks", "pre-receive")
	info, statErr := os.Stat(hookPath)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	content, readErr := os.ReadFile(hookPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "loam-ofg.18")
}

// TestReconcileMirror_NonexistentPathIsNilNotError proves
// docs/server-spec.md's Startup step 3 contract: a mirror missing from
// disk (not yet cloned, or lost) must not abort reconciliation -- it is
// derived state the next Mirror Sync cycle re-clones. This also proves
// ReconcileMirror creates nothing at a path that does not already exist as
// a directory.
func TestReconcileMirror_NonexistentPathIsNilNotError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "never-cloned.git")
	err := ReconcileMirror(t.Context(), missing)
	require.NoError(t, err)
	_, statErr := os.Stat(missing)
	assert.True(t, os.IsNotExist(statErr), "ReconcileMirror must not create a mirror path that did not already exist")
}

// TestReconcileMirror_SecondCallIsNoopAndConfigStaysCorrect is the
// explicit idempotency test the bead demands: reconcile twice, and assert
// the second run is a no-op (identical hook bytes/mode and identical
// config values) AND that the config is still correct afterward.
func TestReconcileMirror_SecondCallIsNoopAndConfigStaysCorrect(t *testing.T) {
	t.Parallel()
	repoPath := newBareMirror(t)
	require.NoError(t, ReconcileMirror(t.Context(), repoPath))
	hookPath := filepath.Join(repoPath, "hooks", "pre-receive")
	firstContent, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	firstInfo, err := os.Stat(hookPath)
	require.NoError(t, err)
	firstDenyNonFF, _ := gitConfigGet(t, repoPath, "receive.denyNonFastForwards")
	firstDenyDeletes, _ := gitConfigGet(t, repoPath, "receive.denyDeletes")
	require.NoError(t, ReconcileMirror(t.Context(), repoPath))
	secondContent, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	secondInfo, err := os.Stat(hookPath)
	require.NoError(t, err)
	secondDenyNonFF, ok := gitConfigGet(t, repoPath, "receive.denyNonFastForwards")
	require.True(t, ok)
	secondDenyDeletes, ok := gitConfigGet(t, repoPath, "receive.denyDeletes")
	require.True(t, ok)
	assert.Equal(t, firstContent, secondContent, "hook bytes must be identical after a repeated call")
	assert.Equal(t, firstInfo.Mode().Perm(), secondInfo.Mode().Perm(), "hook mode must be identical after a repeated call")
	assert.Equal(t, "true", secondDenyNonFF)
	assert.Equal(t, "true", secondDenyDeletes)
	assert.Equal(t, firstDenyNonFF, secondDenyNonFF)
	assert.Equal(t, firstDenyDeletes, secondDenyDeletes)
}

// TestReconcileMirror_RepairsAStaleHookMode proves writeHook's atomic
// write-temp-then-rename genuinely REPLACES a tampered hook, not merely
// re-chmods whatever is already there: a pre-existing hook file, left both
// non-executable AND with the wrong content by some prior state (a hand
// edit, an older version of this package, direct tampering), must come
// back as both 0o755 AND byte-identical to hookScript. Asserting content
// is load-bearing, not incidental: a mutant that skip-if-present guards
// the write (only chmods an existing file, never rewrites its bytes) would
// still pass a mode-only assertion here, silently leaving an edited or
// truncated hook in place forever while every future reconciliation
// reports success.
func TestReconcileMirror_RepairsAStaleHookMode(t *testing.T) {
	t.Parallel()
	repoPath := newBareMirror(t)
	hookPath := filepath.Join(repoPath, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o644))
	require.NoError(t, ReconcileMirror(t.Context(), repoPath))
	info, err := os.Stat(hookPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	content, err := os.ReadFile(hookPath)
	require.NoError(t, err)
	assert.Equal(t, hookScript, string(content), "a tampered hook's CONTENT must be replaced, not just its mode repaired")
}

// TestReconcileMirror_NonRepoDirectoryInsideAGitWorktreeErrorsAndDoesNotTouchEnclosingRepo
// proves the discovery-safety fix in verifyBareRepo/setConfig (MUST-FIX 3):
// a directory that exists but is not itself a git repository, nested
// inside a real git worktree, must error out of ReconcileMirror rather
// than have git's own -C/-cd upward repository discovery silently read
// and write the ENCLOSING repository's config instead. Verified live during
// review: `git -C <nonRepoSubdir> config receive.denyDeletes true` returns
// rc=0 and mutates the enclosing repo's config -- exactly what --git-dir
// here must prevent, since a half-finished clone or a restored volume that
// exists as a directory but never became a real git repo would otherwise
// make ReconcileMirror report success while hardening someone else's repo
// instead of the intended mirror.
func TestReconcileMirror_NonRepoDirectoryInsideAGitWorktreeErrorsAndDoesNotTouchEnclosingRepo(t *testing.T) {
	t.Parallel()
	enclosingRepo := t.TempDir()
	initCmd := exec.CommandContext(t.Context(), "git", "init", "--quiet", enclosingRepo)
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	nonRepoSubdir := filepath.Join(enclosingRepo, "not-a-repo", "nested", "mirror.git")
	require.NoError(t, os.MkdirAll(nonRepoSubdir, 0o755))
	reconcileErr := ReconcileMirror(t.Context(), nonRepoSubdir)
	require.Error(t, reconcileErr, "a non-repo directory nested in a real git worktree must error, not silently harden the enclosing repo")
	enclosingGitDir := filepath.Join(enclosingRepo, ".git")
	_, ok := gitConfigGet(t, enclosingGitDir, "receive.denyDeletes")
	assert.False(t, ok, "the enclosing repository's config must be untouched")
	_, ok = gitConfigGet(t, enclosingGitDir, "receive.denyNonFastForwards")
	assert.False(t, ok, "the enclosing repository's config must be untouched")
	hookPath := filepath.Join(enclosingGitDir, "hooks", "pre-receive")
	_, statErr := os.Stat(hookPath)
	assert.True(t, os.IsNotExist(statErr), "the enclosing repository's own hooks must be untouched")
}

// TestReconcileMirror_NonBareRepoErrorsWithoutWritingAPhantomHook proves
// the other half of MUST-FIX 3: a repoPath that IS a valid git repository,
// but a non-bare one (a working tree, not a mirror), must also be
// rejected -- writing this package's hook at "<repoPath>/hooks/pre-receive"
// would land outside that repo's real hook directory (".git/hooks") and
// never run, while the config calls would land in that real repository's
// config, silently doing nothing useful for push safety.
func TestReconcileMirror_NonBareRepoErrorsWithoutWritingAPhantomHook(t *testing.T) {
	t.Parallel()
	workTree := t.TempDir()
	initCmd := exec.CommandContext(t.Context(), "git", "init", "--quiet", workTree)
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	reconcileErr := ReconcileMirror(t.Context(), workTree)
	require.Error(t, reconcileErr, "a non-bare repository must be rejected, not treated as a mirror")
	phantomHook := filepath.Join(workTree, "hooks", "pre-receive")
	_, statErr := os.Stat(phantomHook)
	assert.True(t, os.IsNotExist(statErr), "must not write a phantom hook outside the repo's real hooks directory")
}

// TestReconcileMirror_DoesNotDisturbUnrelatedConfig proves ReconcileMirror
// never "clobbers local changes": an unrelated config key, standing in for
// state a real mirror carries that this package has no business touching,
// must survive reconciliation untouched.
func TestReconcileMirror_DoesNotDisturbUnrelatedConfig(t *testing.T) {
	t.Parallel()
	repoPath := newBareMirror(t)
	setCmd := exec.CommandContext(t.Context(), "git", "--git-dir="+repoPath, "config", "loam.testmarker", "keep-me")
	require.NoError(t, setCmd.Run())
	require.NoError(t, ReconcileMirror(t.Context(), repoPath))
	marker, ok := gitConfigGet(t, repoPath, "loam.testmarker")
	require.True(t, ok)
	assert.Equal(t, "keep-me", marker)
}
