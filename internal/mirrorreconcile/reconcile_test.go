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

// gitConfigGet reads back a single config key from repoPath via a
// separate `git config --get` invocation -- never trusting ReconcileMirror
// itself for the value under test, per this bead's own testing
// instructions.
func gitConfigGet(t *testing.T, repoPath, key string) (value string, ok bool) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", repoPath, "config", "--get", key)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// TestReconcileMirror_WritesRequiredConfigAndHook proves the substance of
// this bead: after one call, both required config keys read back exactly
// "true" via a separate git-config invocation, and the hook file exists,
// executable, at the exact path docs/git-spec.md pins.
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

// TestReconcileMirror_RepairsAStaleHookMode proves the chmod-after-write
// fix in writeHook is not vacuous: a pre-existing hook file left
// non-executable by some prior state must be forced back to 0o755, since
// os.WriteFile alone would silently leave an existing file's mode
// untouched.
func TestReconcileMirror_RepairsAStaleHookMode(t *testing.T) {
	t.Parallel()
	repoPath := newBareMirror(t)
	hookPath := filepath.Join(repoPath, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o644))
	require.NoError(t, ReconcileMirror(t.Context(), repoPath))
	info, err := os.Stat(hookPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

// TestReconcileMirror_DoesNotDisturbUnrelatedConfig proves ReconcileMirror
// never "clobbers local changes": an unrelated config key, standing in for
// state a real mirror carries that this package has no business touching,
// must survive reconciliation untouched.
func TestReconcileMirror_DoesNotDisturbUnrelatedConfig(t *testing.T) {
	t.Parallel()
	repoPath := newBareMirror(t)
	setCmd := exec.CommandContext(t.Context(), "git", "-C", repoPath, "config", "loam.testmarker", "keep-me")
	require.NoError(t, setCmd.Run())
	require.NoError(t, ReconcileMirror(t.Context(), repoPath))
	marker, ok := gitConfigGet(t, repoPath, "loam.testmarker")
	require.True(t, ok)
	assert.Equal(t, "keep-me", marker)
}
