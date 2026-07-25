package testfixture

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runGit runs git with args in dir (the process's own working directory if
// dir is ""), failing tb with the combined output on error, and returns
// trimmed stdout.
func runGit(ctx context.Context, tb testing.TB, dir string, args ...string) string {
	tb.Helper()
	return runGitEnv(ctx, tb, dir, nil, args...)
}

// runGitEnv is runGit with additional environment variables appended to the
// current process environment (e.g. GIT_AUTHOR_* or GIT_DIR overrides).
func runGitEnv(ctx context.Context, tb testing.TB, dir string, extraEnv []string, args ...string) string {
	tb.Helper()
	return runGitFull(ctx, tb, dir, nil, extraEnv, args...)
}

// runGitIn runs a git plumbing command against gitDir (bare-repo-safe: sets
// GIT_DIR rather than requiring a working tree), optionally feeding stdin
// and extra environment variables such as GIT_INDEX_FILE.
func runGitIn(ctx context.Context, tb testing.TB, gitDir string, stdin []byte, extraEnv []string, args ...string) string {
	tb.Helper()
	env := append([]string{"GIT_DIR=" + gitDir}, extraEnv...)
	return runGitFull(ctx, tb, "", stdin, env, args...)
}

// runGitFull is the shared implementation behind runGit, runGitEnv, and
// runGitIn.
func runGitFull(ctx context.Context, tb testing.TB, dir string, stdin []byte, extraEnv []string, args ...string) string {
	tb.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		tb.Fatalf("git %v (dir=%q): %v\n%s", args, dir, err, out.String())
	}
	return strings.TrimSpace(out.String())
}

// tryRevIn resolves ref against gitDir without failing the test, reporting
// ok=false if ref does not exist. Used to let mutation helpers fall back to
// a sensible default when a caller names a branch that hasn't been created
// yet.
func tryRevIn(ctx context.Context, gitDir, ref string) (sha string, ok bool) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", ref)
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
