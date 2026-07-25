package forge

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireGit skips the test when the git binary is not on PATH, per the
// bead's instruction to skip gracefully rather than fail.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH; skipping git-probe test")
	}
}

// runGit runs git with the given args in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// newBareRepoWithCommit creates a bare repo at <tmp>/repo.git seeded with
// one commit on refs/heads/main, via a scratch working clone.
func newBareRepoWithCommit(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "repo.git")
	runGit(t, tmp, "init", "--bare", "-q", "-b", "main", bare)
	work := filepath.Join(tmp, "work")
	require.NoError(t, os.Mkdir(work, 0o755))
	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "config", "user.email", "a@example.com")
	runGit(t, work, "config", "user.name", "a")
	require.NoError(t, os.WriteFile(filepath.Join(work, "f.txt"), []byte("hi"), 0o644))
	runGit(t, work, "add", "f.txt")
	runGit(t, work, "commit", "-q", "-m", "init")
	runGit(t, work, "push", "-q", bare, "main")
	return bare
}

func TestForgejo_CheckRepo_RepoNotFound(t *testing.T) {
	t.Parallel()
	requireGit(t)
	tmp := t.TempDir()
	missing := "file://" + filepath.Join(tmp, "does-not-exist.git")
	f := NewForgejo("unused-host", "", http.DefaultClient, testLogger())
	err := f.CheckRepo(t.Context(), missing)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoNotFound)
}

// gitSmartHTTPHandler is a minimal git smart-HTTP server sufficient for
// `git ls-remote` (the real git binary) against bare, plus a
// configurable status for the receive-pack advertisement so tests can
// simulate "can write" vs "write denied" without needing a full
// receive-pack implementation — CheckRepo's write probe only inspects
// the status code of that one request.
type gitSmartHTTPHandler struct {
	bareDir            string
	receivePackAllowed bool
}

func (h *gitSmartHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Query().Get("service") == "git-receive-pack":
		if !h.receivePackAllowed {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(pktLine("# service=git-receive-pack\n") + flushPkt))
	case r.URL.Query().Get("service") == "git-upload-pack":
		h.serveUploadPackAdvertisement(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveUploadPackAdvertisement shells out to `git upload-pack
// --advertise-refs` against the bare repo to produce a byte-correct
// pkt-line advertisement, so the real `git ls-remote` binary (used by
// the read probe) can parse it.
func (h *gitSmartHTTPHandler) serveUploadPackAdvertisement(w http.ResponseWriter, r *http.Request) {
	cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", "--advertise-refs", h.bareDir)
	out, err := cmd.Output()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(pktLine("# service=git-upload-pack\n")))
	_, _ = w.Write([]byte(flushPkt))
	_, _ = w.Write(out)
}

const flushPkt = "0000"

func pktLine(s string) string {
	return hex4(len(s)+4) + s
}

func hex4(n int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 4)
	for i := 3; i >= 0; i-- {
		b[i] = hexdigits[n&0xf]
		n >>= 4
	}
	return string(b)
}

func TestForgejo_CheckRepo_ReadOkWriteOk(t *testing.T) {
	t.Parallel()
	requireGit(t)
	bare := newBareRepoWithCommit(t)
	server := httptest.NewServer(&gitSmartHTTPHandler{bareDir: bare, receivePackAllowed: true})
	defer server.Close()
	f := NewForgejo("unused-host", "some-token", server.Client(), testLogger())
	err := f.CheckRepo(t.Context(), server.URL)
	assert.NoError(t, err)
}

// TestForgejo_CheckRepo_ReadOkWriteDenied covers the credentials.feature
// scenario "A token without git access fails enrollment": the token can
// read the repo (ls-remote succeeds) but the receive-pack probe is
// denied, so CheckRepo must report ErrNoWriteAccess distinctly from
// ErrRepoNotFound.
func TestForgejo_CheckRepo_ReadOkWriteDenied(t *testing.T) {
	t.Parallel()
	requireGit(t)
	bare := newBareRepoWithCommit(t)
	server := httptest.NewServer(&gitSmartHTTPHandler{bareDir: bare, receivePackAllowed: false})
	defer server.Close()
	f := NewForgejo("unused-host", "read-only-token", server.Client(), testLogger())
	err := f.CheckRepo(t.Context(), server.URL)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoWriteAccess)
	assert.False(t, errors.Is(err, ErrRepoNotFound), "write-denied must not also report not-found")
}

// TestForgejo_OneTokenCoversRESTAndGit covers the credentials.feature
// scenario "One token covers REST and git": a single token value drives
// ValidateToken (REST), GitCredentials (the git-over-HTTPS convention),
// and CheckRepo's git read+write probes, all successfully.
func TestForgejo_OneTokenCoversRESTAndGit(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "shared-token"
	restServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer restServer.Close()
	bare := newBareRepoWithCommit(t)
	gitServer := httptest.NewServer(&authenticatingGitHandler{
		token:   token,
		handler: &gitSmartHTTPHandler{bareDir: bare, receivePackAllowed: true},
	})
	defer gitServer.Close()
	f := NewForgejo(restServer.URL, token, gitServer.Client(), testLogger())
	require.NoError(t, f.ValidateToken(t.Context(), restServer.URL, token))
	username, password, err := f.GitCredentials(t.Context(), token)
	require.NoError(t, err)
	assert.NotEmpty(t, username)
	assert.Equal(t, token, password)
	require.NoError(t, f.CheckRepo(t.Context(), gitServer.URL))
}

// authenticatingGitHandler wraps gitSmartHTTPHandler, rejecting requests
// that don't carry the expected token as the Basic-auth password (any
// username), matching GitCredentials' convention and CheckRepo's
// gitAuthArgs/receivePackProbe auth injection.
type authenticatingGitHandler struct {
	token   string
	handler http.Handler
}

func (h *authenticatingGitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, password, ok := r.BasicAuth()
	if !ok || password != h.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.handler.ServeHTTP(w, r)
}
