package fakeforge

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireGit skips the test when the git binary is not available, since
// the git smart-HTTP surface and every seeding/control operation shells
// out to it.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// newTestServer builds a Server wrapped in an httptest.Server, wiring
// cleanup for both so callers never share state across tests.
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv, err := New(logger)
	if err != nil {
		t.Fatalf("fakeforge.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.SetBaseURL(ts.URL)
	return srv, ts
}

// withCreds returns rawURL with user/pass embedded as HTTP Basic
// credentials, the form a real `git clone`/`git push` invocation uses to
// authenticate against smart HTTP without a terminal prompt.
func withCreds(t *testing.T, rawURL, user, pass string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// clientGitEnv builds the environment for a client-side git invocation,
// isolated from every credential store and config file the developer's or
// CI machine happens to have.
//
// This isolation is load-bearing, not hygiene. macOS's Command Line Tools
// ship a SYSTEM gitconfig
// (/Library/Developer/CommandLineTools/usr/share/git-core/gitconfig) that
// sets credential.helper=osxkeychain, and osxkeychain keys entries by
// protocol+host while ignoring the port. Every fakeforge test server binds
// 127.0.0.1 on an ephemeral port and most of them register the same token
// string, so once any authenticating test stored a credential for
// 127.0.0.1, the helper would hand that same token to a LATER test that
// deliberately supplies none -- and the server, which genuinely holds that
// token, would accept it. TestGitCloneFailsWithoutCredentials then saw its
// unauthenticated clone SUCCEED, i.e. a credentials guard silently
// inverting, intermittently and only on machines with a keychain helper.
//
// GIT_CONFIG_NOSYSTEM drops the system file; HOME and XDG_CONFIG_HOME are
// redirected at a per-test temp dir so no user-global config is read
// either; credential.helper is then explicitly cleared so an inherited
// GIT_CONFIG_* or a future config source cannot reintroduce one.
// GIT_TERMINAL_PROMPT=0 keeps a rejected credential failing fast rather
// than hanging on a prompt.
func clientGitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
}

// clientGitArgs prefixes args with the -c overrides every client-side git
// invocation needs. credential.helper is cleared with an empty value,
// which git treats as resetting the helper list rather than adding one.
func clientGitArgs(args []string) []string {
	return append([]string{"-c", "credential.helper="}, args...)
}

// runClientGit runs a real git command as an external test actor would
// (not through Server.runGit), failing the test on error. See
// clientGitEnv for why the environment is scrubbed rather than inherited.
func runClientGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", clientGitArgs(args)...)
	cmd.Dir = dir
	cmd.Env = append(clientGitEnv(t),
		"GIT_AUTHOR_NAME=test-actor", "GIT_AUTHOR_EMAIL=test-actor@example.invalid",
		"GIT_COMMITTER_NAME=test-actor", "GIT_COMMITTER_EMAIL=test-actor@example.invalid")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return out
}

// tryClientGit runs a real git command and returns its error without
// failing the test, for cases where the caller expects the command to fail
// (e.g. a rejected push).
func tryClientGit(t *testing.T, dir string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", clientGitArgs(args)...)
	cmd.Dir = dir
	cmd.Env = clientGitEnv(t)
	return cmd.CombinedOutput()
}

// branchSHA returns the commit SHA that branch points to in repo's bare
// storage, failing the test if it cannot be resolved.
func branchSHA(t *testing.T, srv *Server, repo, branch string) string {
	t.Helper()
	out, err := srv.runGit(t.Context(), "", "--git-dir="+srv.repoDir(repo), "rev-parse", "refs/heads/"+branch)
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// branchExists reports whether branch exists in repo's bare storage.
func branchExists(t *testing.T, srv *Server, repo, branch string) bool {
	t.Helper()
	_, err := srv.runGit(t.Context(), "", "--git-dir="+srv.repoDir(repo), "rev-parse", "--verify", "refs/heads/"+branch)
	return err == nil
}
