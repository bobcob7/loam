package gitanchor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runGit runs a real git subcommand in dir (empty means the process's own
// cwd), failing t immediately on a nonzero exit -- the same convention
// internal/gitdiff/testhelpers_test.go's runGit uses, so a broken fixture
// never surfaces as a confusing failure several lines later.
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
// config` -- matches internal/gitdiff/testhelpers_test.go's helper of the
// same name.
func newWorkingRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "gitanchor-test@example.invalid")
	runGit(t, dir, "config", "user.name", "gitanchor-test")
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
// cloning src -- what a real enrolled repo's bare mirror looks like on
// disk, never a hand-rolled fixture.
func bareCloneInto(t *testing.T, src, mirrorDir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGit(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
}

// seedWorkBranchRef relocates an already-cloned mirror's refs/heads/<name>
// to refnames.WorkBranch(name) and deletes the original -- standing in for
// what `work start` does server-side, matching
// internal/gitdiff/testhelpers_test.go's helper of the same name.
func seedWorkBranchRef(t *testing.T, mirrorDir, name string) {
	t.Helper()
	runGit(t, "", "--git-dir="+mirrorDir, "update-ref", "refs/heads/loam-reserved/"+name, "refs/heads/"+name)
	runGit(t, "", "--git-dir="+mirrorDir, "update-ref", "-d", "refs/heads/"+name)
}

// linesOf builds a fixture file's content from n numbered, newline-
// terminated lines -- so a test asserting "an N-line file" is unambiguous
// about what N counts, rather than depending on an incidental trailing
// newline it never mentions.
func linesOf(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}
