package gitdiff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runGit runs a real git subcommand in dir (empty means the process's own
// cwd), failing t immediately on a nonzero exit -- the same convention
// internal/handler/git/testhelpers_test.go's runGit uses, so a broken
// fixture never surfaces as a confusing failure several lines later.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// newWorkingRepo creates a real, non-bare git repository at dir, on branch
// "main", with an explicit committer AND author identity set via `git
// config` (never left to git's own username@hostname guess) -- CI has no
// global gitconfig, and this is exactly the gap that made CI red on this
// project earlier: --author alone sets only the author, never the
// committer, so every commit here goes through plain `git commit`, relying
// on the user.name/user.email config set below for both.
func newWorkingRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "gitdiff-test@example.invalid")
	runGit(t, dir, "config", "user.name", "gitdiff-test")
}

// writeAndCommit writes content to path relative to dir and commits it on
// dir's current branch with message.
func writeAndCommit(t *testing.T, dir, path, content, message string) {
	t.Helper()
	full := filepath.Join(dir, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	runGit(t, dir, "add", path)
	runGit(t, dir, "commit", "--quiet", "-m", message)
}

// bareCloneInto creates a bare mirror at mirrorDir (parents included) by
// cloning src -- exactly what a real enrolled repo's bare mirror looks
// like on disk (all of src's branches, not just the checked-out one),
// never a hand-rolled fixture. Matches
// internal/handler/git/testhelpers_test.go's seedBareMirror pattern.
func bareCloneInto(t *testing.T, src, mirrorDir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGit(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
}
