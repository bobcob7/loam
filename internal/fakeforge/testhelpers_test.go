package fakeforge

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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

// runClientGit runs a real git command as an external test actor would
// (not through Server.runGit), failing the test on error. GIT_TERMINAL_PROMPT
// is disabled so a rejected credential fails fast instead of hanging.
func runClientGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
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
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
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
