package gittransport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/stretchr/testify/require"
)

// TestTransport_IsolatesFromHostileAmbientNetrc reproduces, at this
// package's own boundary, the exact documented defect from today's
// session: an ambient credential source silently authenticating a
// request that was supposed to fail (there: macOS's SYSTEM gitconfig
// wiring up osxkeychain, itself keyed by protocol+host while IGNORING
// the port; here: a poisoned ~/.netrc, matched by libcurl -- which git's
// http transport always consults -- the same traditional format that
// likewise carries no port field, so it is exactly as port-blind as
// osxkeychain). host is deliberately "" here -- Transport injects no
// Authorization header at all for an anonymous call, and this package's
// own `-c credential.helper=` (see run) already neutralizes any
// ambient credential HELPER regardless of gitEnv's isolation, so a
// hostile credential.helper alone would not distinguish this test from
// a no-op; netrc is the one ambient credential source libcurl consults
// unconditionally, outside git's own credential-helper machinery, that
// gitEnv's HOME redirection is what actually has to defeat. If
// isolation holds, the poisoned ~/.netrc under the ambient HOME is
// never read and the fetch fails cleanly; if HOME's redirection is
// dropped, libcurl finds it, answers with the correct token, and the
// fetch silently succeeds -- precisely the failure class this bead
// exists to rule out.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a
// parallel ancestor.
func TestTransport_IsolatesFromHostileAmbientNetrc(t *testing.T) {
	requireGit(t)
	const correctToken = "correct-token-the-ambient-netrc-knows"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(correctToken)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	hostname, _, ok := strings.Cut(host, ":")
	require.True(t, ok, "fakeforge's httptest server URL must carry an explicit port")
	ambientHome := t.TempDir()
	netrc := "machine " + hostname + "\nlogin any\npassword " + correctToken + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(ambientHome, ".netrc"), []byte(netrc), 0o600))
	t.Setenv("HOME", ambientHome)
	credStore := &staticCredentialSource{}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), "", mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.Error(t, err, "an anonymous fetch against a token-gated repo must fail -- it must NEVER be rescued by an ambient .netrc entry the host machine happens to have")
	require.Equal(t, 0, credStore.calls, "no credential was resolved for this deliberately anonymous call, so the store must never have been consulted")
}

// TestTransport_NeutralizesHostileAmbientGitConfigCountOnAnonymousCall
// pins loam-ys1 item (2): gitEnv's anonymous branch (authHeaderValue == "")
// used to return early without emitting GIT_CONFIG_COUNT at all, so the
// subprocess simply inherited whatever GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/
// GIT_CONFIG_VALUE_n the parent process happened to have set -- including a
// hostile http.extraHeader. This test sets exactly that ambient config,
// carrying a *valid* Authorization header for the repo's real token, in the
// parent process's own environment (t.Setenv), then makes a deliberately
// anonymous (host == "") call against that token-gated repo. If the
// ambient config is honoured, the "anonymous" fetch is secretly
// authenticated and succeeds; if isolation holds, GIT_CONFIG_COUNT=0
// overrides the inherited value and the fetch fails cleanly, exactly like
// TestTransport_IsolatesFromHostileAmbientNetrc's netrc case.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestTransport_NeutralizesHostileAmbientGitConfigCountOnAnonymousCall(t *testing.T) {
	requireGit(t)
	const correctToken = "correct-token-the-ambient-config-knows"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(correctToken)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	encoded := basicAuthValue(t, correctToken)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Basic "+encoded)
	credStore := &staticCredentialSource{}
	transport := New(credStore, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), "", mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.Error(t, err, "an anonymous fetch against a token-gated repo must fail -- it must NEVER be rescued by a hostile ambient GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 the parent process happens to have set")
	require.Equal(t, 0, credStore.calls, "no credential was resolved for this deliberately anonymous call, so the store must never have been consulted")
}
