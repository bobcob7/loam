package testfixture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// hermeticEnv returns the environment overrides that make every git
// invocation in this package ignore the calling machine's global and
// system git config (commit.gpgsign, core.hooksPath, init.templateDir,
// core.autocrlf, user.name/email, ...), so fixture materialization and
// mutation behave identically regardless of the developer or CI box it
// runs on. Returned fresh each call so no package-level slice is shared
// (and potentially mutated) across callers.
func hermeticEnv() []string {
	return []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
}

// fixtureIdentityEnv returns the author/committer environment overrides
// used for every commit this package makes, so mutations are attributable
// to the fixture rather than whatever hermeticEnv leaves unset. Returned
// fresh each call for the same reason as hermeticEnv.
func fixtureIdentityEnv() []string {
	return []string{"GIT_AUTHOR_NAME=loam-fixture", "GIT_AUTHOR_EMAIL=fixture@loam.test", "GIT_COMMITTER_NAME=loam-fixture", "GIT_COMMITTER_EMAIL=fixture@loam.test"}
}

// runGit runs git with args in dir (the process's own working directory if
// dir is ""), returning trimmed stdout, or an error wrapping the combined
// output on failure.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitFull(ctx, dir, nil, nil, args...)
}

// runGitEnv is runGit with additional environment variables layered on top
// of the current process environment and hermeticEnv (e.g. GIT_AUTHOR_*
// overrides).
func runGitEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	return runGitFull(ctx, dir, nil, extraEnv, args...)
}

// runGitIn runs a git plumbing command against gitDir (bare-repo-safe: sets
// GIT_DIR rather than requiring a working tree), optionally feeding stdin
// and extra environment variables such as GIT_INDEX_FILE.
func runGitIn(ctx context.Context, gitDir string, stdin []byte, extraEnv []string, args ...string) (string, error) {
	env := append([]string{"GIT_DIR=" + gitDir}, extraEnv...)
	return runGitFull(ctx, "", stdin, env, args...)
}

// runGitFull is the shared implementation behind runGit, runGitEnv, and
// runGitIn. It always applies hermeticEnv, regardless of whether the caller
// passes any extraEnv, so no code path can accidentally run against the
// ambient machine's git config.
func runGitFull(ctx context.Context, dir string, stdin []byte, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	env := append(os.Environ(), hermeticEnv()...)
	cmd.Env = append(env, extraEnv...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %v (dir=%q): %w\n%s", args, dir, err, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// tryRevIn resolves ref against gitDir without treating a missing ref as an
// error, reporting ok=false if ref does not exist. Used to let mutation
// helpers fall back to a sensible default when a caller names a branch that
// has not been created yet.
func tryRevIn(ctx context.Context, gitDir, ref string) (sha string, ok bool) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", ref)
	env := append(os.Environ(), hermeticEnv()...)
	cmd.Env = append(env, "GIT_DIR="+gitDir)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
