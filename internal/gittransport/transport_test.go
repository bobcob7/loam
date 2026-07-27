package gittransport

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransport_FetchPullsUpstreamRefsIntoMirror(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "fetch-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.NoError(t, err)
	out, err := runVerificationGit(t, "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
	require.NoErrorf(t, err, "mirror should have fetched refs/heads/main: %s", out)
	assert.NotEmpty(t, strings.TrimSpace(string(out)))
	assert.Equal(t, 1, credStore.calls, "GetByHost must be called for this fetch")
}

func TestTransport_PushCreatesBranchUpstream(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "push-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.NoError(t, err)
	mainSHA, err := runVerificationGit(t, "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
	require.NoError(t, err)
	sha := strings.TrimSpace(string(mainSHA))
	_, err = transport.Push(t.Context(), host, mirrorDir, upstreamURL, sha+":refs/heads/loam/wb-1")
	require.NoError(t, err)
	authedURL := withCreds(t, upstreamURL, "any", token)
	out, err := runVerificationGit(t, "ls-remote", authedURL, "refs/heads/loam/wb-1")
	require.NoErrorf(t, err, "ls-remote: %s", out)
	assert.Contains(t, string(out), sha, "pushed branch must point at the mirror's tip")
}

func TestTransport_PushRejectsNonFastForward(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "ff-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.NoError(t, err)
	// Create the branch upstream first (an ordinary create, empty old
	// side of the mirror's history).
	rootSHA, err := runVerificationGit(t, "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
	require.NoError(t, err)
	root := strings.TrimSpace(string(rootSHA))
	_, err = transport.Push(t.Context(), host, mirrorDir, upstreamURL, root+":refs/heads/loam/wb-2")
	require.NoError(t, err)
	// Advance the branch upstream out from under the mirror via a second,
	// independent clone -- simulating a diverging history -- so the
	// mirror's push refspec below is a genuine non-fast-forward.
	otherClone := t.TempDir()
	authedURL := withCreds(t, upstreamURL, "any", token)
	_, err = runVerificationGit(t, "clone", "-q", "--branch", "loam/wb-2", authedURL, otherClone)
	require.NoError(t, err)
	out, err := runVerificationGit(t, "-C", otherClone, "commit", "--allow-empty", "-q", "-m", "diverge", "--author=t <t@example.invalid>")
	require.NoErrorf(t, err, "commit: %s", out)
	_, err = runVerificationGit(t, "-C", otherClone, "push", "-q", authedURL, "loam/wb-2")
	require.NoError(t, err)
	// The mirror's own idea of loam/wb-2 (still at root) would now be a
	// non-fast-forward relative to what's upstream; pushing root again
	// as a plain (non-forced) update must be rejected by the upstream.
	_, err = transport.Push(t.Context(), host, mirrorDir, upstreamURL, root+":refs/heads/loam/wb-2")
	require.Error(t, err, "a non-fast-forward push must be rejected, never forced")
}

func TestTransport_DeleteRemoteRefRemovesUpstreamBranch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "delete-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.NoError(t, err)
	rootSHA, err := runVerificationGit(t, "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
	require.NoError(t, err)
	root := strings.TrimSpace(string(rootSHA))
	_, err = transport.Push(t.Context(), host, mirrorDir, upstreamURL, root+":refs/heads/loam/wb-3")
	require.NoError(t, err)
	authedURL := withCreds(t, upstreamURL, "any", token)
	out, err := runVerificationGit(t, "ls-remote", authedURL, "refs/heads/loam/wb-3")
	require.NoError(t, err)
	require.Contains(t, string(out), root, "precondition: branch must exist upstream before deletion")
	_, err = transport.DeleteRemoteRef(t.Context(), host, mirrorDir, upstreamURL, "refs/heads/loam/wb-3")
	require.NoError(t, err)
	out, err = runVerificationGit(t, "ls-remote", authedURL, "refs/heads/loam/wb-3")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(out)), "branch must be gone upstream after DeleteRemoteRef")
}

func TestTransport_MirrorConfigNeverContainsToken(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "config-secrecy-test-token-zzz"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.NoError(t, err)
	rootSHA, err := runVerificationGit(t, "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
	require.NoError(t, err)
	root := strings.TrimSpace(string(rootSHA))
	_, err = transport.Push(t.Context(), host, mirrorDir, upstreamURL, root+":refs/heads/loam/wb-4")
	require.NoError(t, err)
	configBytes, err := os.ReadFile(filepath.Join(mirrorDir, "config"))
	require.NoError(t, err)
	assert.NotContains(t, string(configBytes), token, "the mirror's on-disk git config must carry no trace of the token")
	assert.NotContains(t, string(configBytes), "extraHeader", "no per-invocation config should ever be persisted into the mirror")
}

func TestTransport_ErrorAndLogNeverContainToken(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const correctToken = "correct-secret-token-abc123"
	const wrongToken = "wrong-secret-token-xyz789"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(correctToken)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: wrongToken}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	transport := New(credStore, newGitCredsConverter(), logger)
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.Error(t, err, "wrong token must be rejected by the upstream")
	assert.NotContains(t, err.Error(), wrongToken, "the wrong token must never appear in the returned error")
	assert.NotContains(t, logBuf.String(), wrongToken, "the wrong token must never appear in a log line")
}

func TestTransport_ResolvesCredentialFreshEveryInvocation(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "fresh-every-call-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.NoError(t, err)
	_, err = transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.NoError(t, err)
	assert.Equal(t, 2, credStore.calls, "each Fetch must resolve the credential itself, never reuse one cached from a previous call")
}

func TestTransport_CredentialLookupFailurePreventsGitInvocation(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: credential store unreachable")
	credStore := &credentialSourceMock{
		GetByHostFunc: func(_ context.Context, _ string) (credentialstore.Credential, error) {
			return credentialstore.Credential{}, wantErr
		},
	}
	gitCreds := &gitCredentialConverterMock{
		GitCredentialsFunc: func(_ context.Context, _ string) (string, string, error) {
			t.Fatal("GitCredentials must not be called when credential lookup already failed")
			return "", "", nil
		},
	}
	transport := New(credStore, gitCreds, testLogger())
	_, err := transport.Fetch(t.Context(), "forge.example.com", "/nonexistent-mirror", "https://forge.example.com/x/y.git", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Len(t, credStore.GetByHostCalls(), 1)
}

func TestTransport_GitCredentialConversionFailurePreventsGitInvocation(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: forge rejects this token shape")
	credStore := &credentialSourceMock{
		GetByHostFunc: func(_ context.Context, _ string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Token: "some-token"}, nil
		},
	}
	gitCreds := &gitCredentialConverterMock{
		GitCredentialsFunc: func(_ context.Context, _ string) (string, string, error) {
			return "", "", wantErr
		},
	}
	transport := New(credStore, gitCreds, testLogger())
	_, err := transport.Push(t.Context(), "forge.example.com", "/nonexistent-mirror", "https://forge.example.com/x/y.git", "abc:refs/heads/loam/wb-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}
