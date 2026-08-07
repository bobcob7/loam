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
	"github.com/bobcob7/loam/internal/urlredact"
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
	_, fetchErr := transport.Fetch(t.Context(), host, mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.Error(t, fetchErr, "wrong token must be rejected by the upstream")
	assert.NotContains(t, fetchErr.Error(), wrongToken, "the wrong token must never appear in the returned error")
	assert.NotContains(t, logBuf.String(), wrongToken, "the wrong token must never appear in a log line")
}

// TestRun_ScrubsEveryFormOfTheSecret exercises the scrubbing contract
// directly, against text that actually contains each form. It stayed here
// rather than moving to internal/urlredact with the scrubber itself
// (loam-051m): what it pins is THIS package's three-form secret list --
// plaintext token, password, base64 auth header -- which is a fact about
// what git can echo, not about how Scrub replaces a substring. Asserting the
// base64 is absent from a real failed fetch would pass vacuously: git does
// not echo the Authorization header on a 401, so the encoded value never
// reaches that output whether the scrubber handles it or not. The header
// carries base64(user:token), which is trivially reversible, so a scrubber
// that knew only the plaintext token would leak the credential intact.
func TestRun_ScrubsEveryFormOfTheSecret(t *testing.T) {
	t.Parallel()
	const token = "scrub-me-secret-token"
	encoded := basicAuthValue(t, token)
	header := "Authorization: Basic " + encoded
	captured := "fatal: could not read Username\ntoken=" + token +
		"\nGIT_CONFIG_VALUE_0=" + header +
		"\nraw-b64=" + encoded + "\n"

	got := urlredact.Scrub(captured, token, token, encoded)

	assert.NotContains(t, got, token, "the plaintext token must be redacted")
	assert.NotContains(t, got, encoded, "the base64 payload must be redacted -- it decodes straight back to the token")
	assert.NotContains(t, got, header, "the header line must be redacted via its base64 payload")
	assert.Contains(t, got, urlredact.Marker, "positive control: something was actually replaced, so the three NotContains above cannot pass on an emptied string")
	assert.Contains(t, got, "fatal: could not read Username", "non-secret text must survive scrubbing")
}

// TestTransport_CloneCreatesPopulatedBareMirror is the acceptance-critical
// proof for loam-ofg.12: cloning must actually produce a bare mirror on
// disk carrying upstream's refs, not merely return without error. Every
// hit for "git clone --mirror" or "git init --bare" in production before
// this method existed was test-only (internal/fakeforge/seed.go,
// internal/testfixture/fixture.go) -- MirrorFetcher.Fetch always assumed
// the mirror already existed.
func TestTransport_CloneCreatesPopulatedBareMirror(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "clone-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := filepath.Join(t.TempDir(), "nested", "acme", "widgets.git")
	_, err := transport.Clone(t.Context(), host, mirrorDir, upstreamURL)
	require.NoError(t, err)
	out, err := runVerificationGit(t, "--git-dir="+mirrorDir, "rev-parse", "--is-bare-repository")
	require.NoErrorf(t, err, "clone must produce a valid git directory: %s", out)
	assert.Equal(t, "true", strings.TrimSpace(string(out)), "clone --mirror must produce a BARE repository")
	shaOut, err := runVerificationGit(t, "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
	require.NoErrorf(t, err, "the mirror must carry upstream's main branch: %s", shaOut)
	assert.NotEmpty(t, strings.TrimSpace(string(shaOut)))
}

// TestTransport_CloneRemovesStaleDirectoryFirst proves a leftover
// directory at mirrorDir (debris from a crashed prior enrollment attempt)
// does not make every retry fail forever -- `git clone` itself refuses a
// non-empty destination, so Clone must clear the path first.
func TestTransport_CloneRemovesStaleDirectoryFirst(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "clone-stale-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := filepath.Join(t.TempDir(), "widgets.git")
	require.NoError(t, os.MkdirAll(mirrorDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mirrorDir, "debris.txt"), []byte("stale"), 0o644))
	_, err := transport.Clone(t.Context(), host, mirrorDir, upstreamURL)
	require.NoError(t, err, "a stale non-empty directory at mirrorDir must not permanently block re-cloning")
	_, statErr := os.Stat(filepath.Join(mirrorDir, "debris.txt"))
	assert.True(t, os.IsNotExist(statErr), "the stale debris file must be gone after Clone")
}

// TestTransport_CloneNeverLeaksTokenOnFailure mirrors
// TestTransport_ErrorAndLogNeverContainToken for Clone: a rejected clone
// (wrong token) must never surface the token in its returned error or a
// log line.
func TestTransport_CloneNeverLeaksTokenOnFailure(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const correctToken = "clone-correct-secret-token"
	const wrongToken = "clone-wrong-secret-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(correctToken)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: wrongToken}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	transport := New(credStore, newGitCredsConverter(), logger)
	mirrorDir := filepath.Join(t.TempDir(), "widgets.git")
	_, err := transport.Clone(t.Context(), host, mirrorDir, upstreamURL)
	require.Error(t, err, "wrong token must be rejected by the upstream")
	assert.NotContains(t, err.Error(), wrongToken)
	assert.NotContains(t, logBuf.String(), wrongToken)
}

// TestTransport_LsRemoteListsBranchesAndSymref proves LsRemote lists
// every upstream branch and its --symref HEAD pointer, needing no local
// mirror at all -- RepoAdminService.ProbeRepo's read (loam-ofg.12).
func TestTransport_LsRemoteListsBranchesAndSymref(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const token = "ls-remote-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	require.NoError(t, srv.CreateCollidingBranch(t.Context(), "acme/widgets", "wb-release", ""))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: token}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	out, err := transport.LsRemote(t.Context(), host, upstreamURL)
	require.NoError(t, err)
	assert.Contains(t, string(out), "refs/heads/main")
	assert.Contains(t, string(out), "refs/heads/wb-release")
	assert.Contains(t, string(out), "ref: refs/heads/main\tHEAD", "the symref line must name upstream's default branch")
}

// TestTransport_LsRemoteNeverLeaksTokenOnFailure mirrors the Fetch/Clone
// token-secrecy tests for LsRemote.
func TestTransport_LsRemoteNeverLeaksTokenOnFailure(t *testing.T) {
	t.Parallel()
	requireGit(t)
	const correctToken = "ls-remote-correct-secret-token"
	const wrongToken = "ls-remote-wrong-secret-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(correctToken)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	credStore := &staticCredentialSource{token: wrongToken}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	transport := New(credStore, newGitCredsConverter(), logger)
	_, err := transport.LsRemote(t.Context(), host, upstreamURL)
	require.Error(t, err, "wrong token must be rejected by the upstream")
	assert.NotContains(t, err.Error(), wrongToken)
	assert.NotContains(t, logBuf.String(), wrongToken)
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

// TestTransport_RejectsUpstreamURLWithUserinfo pins loam-ys1 item (1):
// upstreamURL is passed straight into exec.Command args by every exported
// method here, so a URL enrolled as https://user:token@forge/... would
// leak that credential into `ps` output on every scheduler tick --
// scrubbing would NOT redact it, since it is not the credential this
// package itself resolves and injects. Transport must reject such a URL
// before it ever reaches the credential store or the git subprocess, for
// every method that accepts an upstreamURL.
// TestValidateUpstreamURL_NeverEchoesTheCredential covers the shapes
// url.URL.Redacted() does NOT protect, which is where the first version of
// this guard leaked. Redacted() masks only the PASSWORD, and only when a
// ":" is present -- so the two forms below passed through it verbatim:
//
//	https://<token>@host/path      the standard PAT-in-URL form for
//	                               GitHub/GitLab/Forgejo, and the likeliest
//	                               way a repo is enrolled with a credential
//	https://user%3A<token>@host/   a percent-encoded colon, which defeats
//	                               the "is there a password" test entirely
//
// The unparseable case is the same hazard by another route: *url.Error
// renders as `parse "<raw url>": <reason>`, so a token containing a byte
// net/url rejects would land in the message whole.
//
// This matters beyond the returned error: it is %w-wrapped to the RPC
// boundary and, on the enroll path, written into repos.sync_error.
func TestValidateUpstreamURL_NeverEchoesTheCredential(t *testing.T) {
	t.Parallel()
	const secret = "leaked-token"
	for _, tt := range []struct {
		name string
		url  string
	}{
		{"userinfo with no password (the PAT form)", "https://" + secret + "@forge.example.invalid/acme/widgets.git"},
		{"percent-encoded colon defeats Redacted", "https://user%3A" + secret + "@forge.example.invalid/acme/widgets.git"},
		{"userinfo with a password", "https://user:" + secret + "@forge.example.invalid/acme/widgets.git"},
		{"unparseable, credential in the raw string", "https://user:" + secret + " with a space@forge.example.invalid/x.git"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateUpstreamURL(tt.url)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUpstreamURLHasUserinfo)
			assert.NotContains(t, err.Error(), secret, "the rejected URL's embedded credential must never appear in the error")
		})
	}
}

func TestTransport_RejectsUpstreamURLWithUserinfo(t *testing.T) {
	t.Parallel()
	const poisoned = "https://user:leaked-token@forge.example.invalid/acme/widgets.git"
	tests := []struct {
		name string
		call func(tr *Transport) ([]byte, error)
	}{
		{"Fetch", func(tr *Transport) ([]byte, error) {
			return tr.Fetch(t.Context(), "forge.example.invalid", "/nonexistent-mirror", poisoned, []string{"+refs/heads/*:refs/heads/*"})
		}},
		{"Push", func(tr *Transport) ([]byte, error) {
			return tr.Push(t.Context(), "forge.example.invalid", "/nonexistent-mirror", poisoned, "abc:refs/heads/loam/wb-1")
		}},
		{"DeleteRemoteRef", func(tr *Transport) ([]byte, error) {
			return tr.DeleteRemoteRef(t.Context(), "forge.example.invalid", "/nonexistent-mirror", poisoned, "refs/heads/loam/wb-1")
		}},
		{"Clone", func(tr *Transport) ([]byte, error) {
			return tr.Clone(t.Context(), "forge.example.invalid", filepath.Join(t.TempDir(), "widgets.git"), poisoned)
		}},
		{"LsRemote", func(tr *Transport) ([]byte, error) {
			return tr.LsRemote(t.Context(), "forge.example.invalid", poisoned)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			credStore := &credentialSourceMock{
				GetByHostFunc: func(_ context.Context, _ string) (credentialstore.Credential, error) {
					t.Fatal("credential lookup must not happen once upstream URL validation rejects it")
					return credentialstore.Credential{}, nil
				},
			}
			gitCreds := &gitCredentialConverterMock{
				GitCredentialsFunc: func(_ context.Context, _ string) (string, string, error) {
					t.Fatal("git credential conversion must not happen once upstream URL validation rejects it")
					return "", "", nil
				},
			}
			transport := New(credStore, gitCreds, testLogger())
			_, err := tt.call(transport)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUpstreamURLHasUserinfo)
			assert.NotContains(t, err.Error(), "leaked-token", "the rejected URL's embedded credential must never appear in the returned error")
		})
	}
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
