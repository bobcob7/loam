package gittransport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/stretchr/testify/assert"
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
	// Deliberately NOT asserting credStore.calls == 0. runRaw consults
	// the store only when host != "", and this call passes host == "",
	// so that assertion can never be non-zero whether or not the
	// neutralisation works -- it reads as evidence while proving
	// nothing. The require.Error above is the whole test.
}

// TestTransport_NeutralizesAmbientGitConfigParametersOnAnonymousCall is the
// same guard for the OTHER ambient config channel. GIT_CONFIG_PARAMETERS is
// how git itself propagates `-c` to subprocesses, so an inherited value is
// ordinary rather than exotic, and git honours it ALONGSIDE
// GIT_CONFIG_COUNT -- so clearing only the latter left this route wide
// open. Before gitEnv cleared it, this fetch SUCCEEDED: the hostile ambient
// header authenticated a call that deliberately resolved no credential of
// its own, defeating the sibling test above by a different door.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestTransport_NeutralizesAmbientGitConfigParametersOnAnonymousCall(t *testing.T) {
	requireGit(t)
	const correctToken = "correct-token-the-ambient-parameters-know"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(correctToken)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	encoded := basicAuthValue(t, correctToken)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.extraHeader'='Authorization: Basic "+encoded+"'")
	transport := New(&staticCredentialSource{}, newGitCredsConverter(), testLogger())
	mirrorDir := newBareMirror(t)
	_, err := transport.Fetch(t.Context(), "", mirrorDir, upstreamURL, []string{"+refs/heads/*:refs/heads/*"})
	require.Error(t, err, "an anonymous fetch against a token-gated repo must fail -- it must NEVER be rescued by a hostile ambient GIT_CONFIG_PARAMETERS the parent process happens to have set")
}

// TestDropGitCurlVerbose_RemovesTheKeyRegardlessOfValue pins loam-bot5:
// unlike every other GIT_TRACE* variable, git only presence-checks
// GIT_CURL_VERBOSE (verified empirically against git 2.50.1: an
// otherwise-identical `git ls-remote` over http emits "http.c:889 ==
// Info:" trace lines with GIT_CURL_VERBOSE=0 set that are absent when the
// variable is unset entirely), so "0" and "" both still count as "set"
// and both turn curl tracing ON. gitEnv used to append "GIT_CURL_VERBOSE=0"
// after os.Environ(), which had exactly that inverted effect. This test
// exercises dropGitCurlVerbose directly against a slice carrying the
// variable with several different values, none of which git would treat
// as "off": if the old "=0" override line were ever reintroduced instead
// of the removal, this would still see a GIT_CURL_VERBOSE key survive
// (with whatever value dropGitCurlVerbose received or was appended after
// it) and fail.
func TestDropGitCurlVerbose_RemovesTheKeyRegardlessOfValue(t *testing.T) {
	t.Parallel()
	for _, hostileValue := range []string{"0", "", "1", "true"} {
		environ := []string{"PATH=/usr/bin", "GIT_CURL_VERBOSE=" + hostileValue, "HOME=/home/whoever"}
		filtered := dropGitCurlVerbose(environ)
		for _, kv := range filtered {
			name, _, _ := strings.Cut(kv, "=")
			assert.NotEqual(t, "GIT_CURL_VERBOSE", name, "GIT_CURL_VERBOSE must be absent, not merely reset to a falsy value -- git presence-checks it rather than parsing a boolean")
		}
		assert.Len(t, filtered, 2, "only the two unrelated entries should survive")
	}
}

// TestTransport_GitEnvNeverCarriesGitCurlVerbose pins the same guarantee
// one level up, at the exact function the bug lived in: gitEnv's returned
// environment -- built from os.Environ() plus overrides -- must never
// contain a GIT_CURL_VERBOSE key, even when the ambient environment this
// process happens to run under carries a hostile GIT_CURL_VERBOSE=0 (the
// exact value the bead's reporter demonstrated turns tracing on). If
// gitEnv ever goes back to appending "GIT_CURL_VERBOSE=0" as one of its
// own overrides -- which last-value-wins ordering would make win over the
// ambient ""-cleared entry too -- this test would still catch it, since
// it asserts on the key's absence, not on any particular value.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestTransport_GitEnvNeverCarriesGitCurlVerbose(t *testing.T) {
	t.Setenv("GIT_CURL_VERBOSE", "0")
	env := gitEnv(t.TempDir(), "")
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		assert.NotEqual(t, "GIT_CURL_VERBOSE", name, "gitEnv must never emit a GIT_CURL_VERBOSE key -- git presence-checks it, so any value (including \"0\", inherited or overridden) turns curl tracing on")
	}
}

// TestTransport_LsRemoteNeverEmitsCurlTraceUnderHostileAmbientGitCurlVerbose
// is the end-to-end version of the two tests above: it reproduces, via a
// real `git ls-remote` subprocess against a real (fakeforge) HTTP git
// server, the exact defect loam-bot5 reports -- with the parent process's
// own environment carrying GIT_CURL_VERBOSE=0 (set via t.Setenv, standing
// in for whatever ambient value the host running this component happens
// to have), a real git invocation must produce no curl trace output at
// all. Before the fix this failed: gitEnv's own "GIT_CURL_VERBOSE=0"
// override, appended after the ambient os.Environ() copy, still counted
// as "set" to git and the combined output carried "== Info:" trace lines.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestTransport_LsRemoteNeverEmitsCurlTraceUnderHostileAmbientGitCurlVerbose(t *testing.T) {
	requireGit(t)
	t.Setenv("GIT_CURL_VERBOSE", "0")
	const token = "curl-verbose-test-token"
	srv, _ := newFakeForgeServer(t)
	srv.AddToken(token)
	require.NoError(t, srv.SeedRepoFiles(t.Context(), "acme/widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("acme/widgets")
	host := hostOf(t, upstreamURL)
	transport := New(&staticCredentialSource{token: token}, newGitCredsConverter(), testLogger())
	out, err := transport.LsRemote(t.Context(), host, upstreamURL)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "== Info:", "curl trace output must never appear -- an ambient GIT_CURL_VERBOSE=0 must not survive into the git subprocess's environment")
	assert.NotContains(t, string(out), "http.c:", "curl trace output must never appear -- an ambient GIT_CURL_VERBOSE=0 must not survive into the git subprocess's environment")
}
