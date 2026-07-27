package diffplan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runGit runs a real git subcommand in dir (empty means the process's own
// cwd), failing t immediately on a nonzero exit -- matches
// internal/gitdiff/testhelpers_test.go's runGit.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// newWorkingRepo creates a real, non-bare git repository at dir on branch
// "main", with an explicit committer AND author identity set via `git
// config` -- never left to git's own username@hostname guess. CI has no
// global gitconfig, so every commit this package's tests make must set this
// explicitly (the exact gap internal/gittransport/helpers_test.go's
// runVerificationGit doc comment warns made CI red before).
func newWorkingRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "diffplan-test@example.invalid")
	runGit(t, dir, "config", "user.name", "diffplan-test")
}

// writeAndCommit writes content to path relative to dir and commits it on
// dir's current branch with message.
func writeAndCommit(t *testing.T, dir, path, content, message string) {
	t.Helper()
	full := filepath.Join(dir, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "--quiet", "-m", message)
}

// removeAndCommit removes path (relative to dir) and commits the deletion.
func removeAndCommit(t *testing.T, dir, path, message string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(dir, path)))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "--quiet", "-m", message)
}

// renameAndCommit renames oldPath to newPath (both relative to dir) via
// `git mv` and commits it.
func renameAndCommit(t *testing.T, dir, oldPath, newPath, message string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, newPath)), 0o755))
	runGit(t, dir, "mv", oldPath, newPath)
	runGit(t, dir, "commit", "--quiet", "-m", message)
}

// bareCloneInto creates a bare mirror at mirrorDir (parents included) by
// cloning src -- matches internal/gitdiff/testhelpers_test.go's
// bareCloneInto.
func bareCloneInto(t *testing.T, src, mirrorDir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGit(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
}
