package fakeforge

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitCloneAndPushRoundTrip(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	srv.AddToken("push-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"README.md": []byte("hello\n")}, SeedOptions{}))
	cloneDir := t.TempDir()
	cloneURL := withCreds(t, srv.GitURL("acme/widgets"), "anyuser", "push-token")
	runClientGit(t, "", "clone", cloneURL, cloneDir)
	got, err := os.ReadFile(filepath.Join(cloneDir, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(got))
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, "NEW.txt"), []byte("new file\n"), 0o644))
	runClientGit(t, cloneDir, "add", "-A")
	runClientGit(t, cloneDir, "commit", "-m", "add new file")
	runClientGit(t, cloneDir, "push", "origin", "HEAD:refs/heads/main")
	tip := branchSHA(t, srv, "acme/widgets", "main")
	subject, err := srv.runGit(ctx, "", "--git-dir="+srv.repoDir("acme/widgets"), "log", "-1", "--format=%s", tip)
	require.NoError(t, err)
	assert.Equal(t, "add new file\n", string(subject))
}

func TestGitCloneFailsWithoutCredentials(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	srv.AddToken("push-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	_, err := tryClientGit(t, "", "clone", srv.GitURL("acme/widgets"), t.TempDir())
	assert.Error(t, err)
}

func TestGitCloneFailsWithBadToken(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	srv.AddToken("push-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	cloneURL := withCreds(t, srv.GitURL("acme/widgets"), "anyuser", "wrong-token")
	_, err := tryClientGit(t, "", "clone", cloneURL, t.TempDir())
	assert.Error(t, err)
}

func TestGitCloneAcceptsAnyUsernameWithValidToken(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	srv.AddToken("push-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	cloneURL := withCreds(t, srv.GitURL("acme/widgets"), "whoever-i-want", "push-token")
	runClientGit(t, "", "clone", cloneURL, t.TempDir())
}

func TestGitPushDeniedForReadOnlyToken(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	srv.AddReadOnlyToken("ro-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	before := branchSHA(t, srv, "acme/widgets", "main")
	cloneDir := t.TempDir()
	cloneURL := withCreds(t, srv.GitURL("acme/widgets"), "anyuser", "ro-token")
	runClientGit(t, "", "clone", cloneURL, cloneDir) // reads must still work
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, "NEW.txt"), []byte("x"), 0o644))
	runClientGit(t, cloneDir, "add", "-A")
	runClientGit(t, cloneDir, "commit", "-m", "should not land")
	_, err := tryClientGit(t, cloneDir, "push", "origin", "HEAD:refs/heads/main")
	assert.Error(t, err)
	after := branchSHA(t, srv, "acme/widgets", "main")
	assert.Equal(t, before, after, "read-only token must not be able to change the branch tip")
}

func TestGitInfoRefsReceivePackDeniedForReadOnlyToken(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	srv.AddReadOnlyToken("ro-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/git/acme/widgets.git/info/refs?service=git-receive-pack", nil)
	require.NoError(t, err)
	req.SetBasicAuth("anyuser", "ro-token")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestGitAuthRejectsMissingBasicAuth(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	srv.AddToken("push-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/git/acme/widgets.git/info/refs?service=git-upload-pack", nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestGitAuthRejectsBadToken(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	srv.AddToken("push-token")
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/git/acme/widgets.git/info/refs?service=git-upload-pack", nil)
	require.NoError(t, err)
	req.SetBasicAuth("anyuser", "wrong-token")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestGitUnavailableReturns503(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv, err := New(logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	srv.gitPath = ""
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/git/acme/widgets.git/info/refs?service=git-upload-pack", nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
