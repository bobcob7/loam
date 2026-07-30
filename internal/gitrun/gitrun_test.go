package gitrun

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initBareRepo creates a bare git repository at dir using the test
// process's own, unhardened environment -- this is fixture setup, not the
// isolation under test, so it deliberately does not go through Env or
// NewCommand.
func initBareRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "init", "--quiet", "--bare", dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)
}

// runIsolated runs `git <args...>` against mirrorDir through this
// package's own seam (GitDirArgs + Env + NewCommand), exactly the way
// every absorbed call site now does, and returns stdout and the exit
// code.
func runIsolated(t *testing.T, mirrorDir string, args ...string) (stdout string, exitCode int) {
	t.Helper()
	home, cleanup, err := NewIsolatedHome()
	require.NoError(t, err)
	defer cleanup()
	var outBuf, errBuf bytes.Buffer
	cmd := NewCommand(t.Context(), Env(home), nil, &outBuf, &errBuf, GitDirArgs(mirrorDir, args...)...)
	runErr := cmd.Run()
	if runErr == nil {
		return outBuf.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return outBuf.String(), exitErr.ExitCode()
	}
	t.Fatalf("running git %v: %v (stderr: %s)", args, runErr, errBuf.String())
	return "", -1
}

// TestEnv_IsolatesFromAmbientHomeGitconfig is loam-ldx's own required
// durability test: it reproduces the shape of the historical bug this
// bead's own instructions call out (an ambient gitconfig setting
// something that silently changes a git invocation's answer -- there,
// macOS's SYSTEM gitconfig's credential.helper=osxkeychain; here, a
// poisoned USER-level ~/.gitconfig, which is specifically the gap
// internal/handler/git had before this bead: GIT_CONFIG_NOSYSTEM alone
// does not block that layer, only a redirected HOME does -- see
// internal/gittransport/isolation_test.go's own
// TestTransport_IsolatesFromHostileAmbientNetrc for the same pattern
// applied to that package's netrc hazard).
//
// It sets HOME -- the actual ambient environment variable for THIS test
// process -- to a directory containing a .gitconfig that sets user.name
// to a recognisable poison value, then runs `git config --get user.name`
// against a scratch bare repo through runIsolated. Env's own HOME
// override, not the ambient one this test just set, is what the
// subprocess must see: a clean, unconfigured mirror has no local
// user.name of its own, so the only way this could resolve to anything is
// by leaking the poisoned ambient config. Exit 1 with empty stdout is the
// correct, isolated answer; exit 0 with the poison value on stdout is
// exactly what Env forgetting to override HOME (or falling back to
// os.Getenv("HOME")) would produce -- confirmed by mutating Env to do
// exactly that and re-running this test, which then fails on the
// assert.Equal below (exitCode 0, stdout "HOSTILE-AMBIENT-POISON"), not a
// panic.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestEnv_IsolatesFromAmbientHomeGitconfig(t *testing.T) {
	poisonedHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(poisonedHome, ".gitconfig"), []byte("[user]\n\tname = HOSTILE-AMBIENT-POISON\n"), 0o644))
	t.Setenv("HOME", poisonedHome)
	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	initBareRepo(t, mirrorDir)
	stdout, exitCode := runIsolated(t, mirrorDir, "config", "--get", "user.name")
	assert.Equal(t, 1, exitCode, "an isolated invocation must not resolve user.name from the ambient (poisoned) HOME's .gitconfig")
	assert.Empty(t, strings.TrimSpace(stdout), "stdout must not contain the ambient .gitconfig's poisoned value")
	assert.NotContains(t, stdout, "HOSTILE-AMBIENT-POISON")
}
