package forge

import (
	"encoding/base64"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestForgejo_LsRemoteProbe_IsolatesFromHostileAmbientNetrc reproduces, at
// internal/forge's own boundary, the class of defect documented in
// internal/gittransport/isolation_test.go's
// TestTransport_IsolatesFromHostileAmbientNetrc: an ambient credential
// source silently authenticating a request that was supposed to be
// validated against only the bound token, turning CheckRepo's
// credential-validation probe into something that validates the wrong
// credential. lsRemoteProbe is deliberately called here with f.token ==
// "" (the anonymous case) -- gitAuthEnv injects no Authorization header
// at all for that case, and lsRemoteProbe's own `-c credential.helper=`
// already neutralizes any ambient credential HELPER regardless of
// gitAuthEnv's isolation, so a hostile credential.helper alone would not
// distinguish this test from a no-op. netrc is the one ambient
// credential source libcurl consults unconditionally, outside git's own
// credential-helper machinery, that gitAuthEnv's HOME redirection is
// what actually has to defeat. If isolation holds, the poisoned
// ~/.netrc under the ambient HOME is never read and ls-remote fails
// cleanly against the token-gated repo; if HOME's redirection is
// dropped (as it was before this fix -- gitAuthEnv used to append only
// three GIT_CONFIG_* entries to a straight copy of os.Environ()),
// libcurl finds the ambient .netrc, answers with the correct token, and
// the probe silently succeeds -- exactly the false positive the bead
// exists to rule out: CheckRepo would report the repo readable using
// the operator's ambient credential, not the bound token under test.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a
// parallel ancestor.
func TestForgejo_LsRemoteProbe_IsolatesFromHostileAmbientNetrc(t *testing.T) {
	requireGit(t)
	const correctToken = "correct-token-the-ambient-netrc-knows"
	bare := newBareRepoWithCommit(t)
	server := httptest.NewServer(&authenticatingGitHandler{
		token:   correctToken,
		handler: &gitSmartHTTPHandler{bareDir: bare, receivePackAllowed: true},
	})
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	hostname, _, ok := strings.Cut(u.Host, ":")
	require.True(t, ok, "httptest server URL must carry an explicit port")
	ambientHome := t.TempDir()
	netrc := "machine " + hostname + "\nlogin any\npassword " + correctToken + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(ambientHome, ".netrc"), []byte(netrc), 0o600))
	t.Setenv("HOME", ambientHome)
	f := NewForgejo(server.URL, "", server.Client(), testLogger())
	err = f.lsRemoteProbe(t.Context(), server.URL)
	require.Error(t, err, "an anonymous ls-remote against a token-gated repo must fail -- it must NEVER be rescued by an ambient ~/.netrc entry the host machine happens to have")
}

// TestForgejo_LsRemoteProbe_NeutralizesHostileAmbientGitConfigCount covers
// the other ambient channel called out in the bead: GIT_CONFIG_COUNT/
// GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 set in the PARENT process's own
// environment (simulating whatever the host running loam happens to have
// set) must not survive into lsRemoteProbe's anonymous git subprocess.
// Before this fix, gitAuthEnv returned nil for the anonymous case (f.token
// == "") and lsRemoteProbe appended it to a plain copy of os.Environ(), so
// an inherited GIT_CONFIG_COUNT was never overridden at all -- the ambient
// header would have silently authenticated a call that was supposed to be
// anonymous.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestForgejo_LsRemoteProbe_NeutralizesHostileAmbientGitConfigCount(t *testing.T) {
	requireGit(t)
	const correctToken = "correct-token-the-ambient-config-knows"
	bare := newBareRepoWithCommit(t)
	server := httptest.NewServer(&authenticatingGitHandler{
		token:   correctToken,
		handler: &gitSmartHTTPHandler{bareDir: bare, receivePackAllowed: true},
	})
	defer server.Close()
	encoded := base64.StdEncoding.EncodeToString([]byte(gitUsername + ":" + correctToken))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Basic "+encoded)
	f := NewForgejo(server.URL, "", server.Client(), testLogger())
	err := f.lsRemoteProbe(t.Context(), server.URL)
	require.Error(t, err, "an anonymous ls-remote against a token-gated repo must fail -- it must NEVER be rescued by a hostile ambient GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 the parent process happens to have set")
}

// TestForgejo_LsRemoteProbe_NeutralizesAmbientGitConfigParameters is the
// same guard for the OTHER ambient config channel: GIT_CONFIG_PARAMETERS
// is how git itself propagates `-c` to subprocesses, so an inherited
// value is ordinary rather than exotic, and git honours it ALONGSIDE
// GIT_CONFIG_COUNT -- clearing only the latter would leave this route
// wide open, exactly the interaction the bead warns has bitten this repo
// before (loam-ys1, ported into internal/gittransport).
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestForgejo_LsRemoteProbe_NeutralizesAmbientGitConfigParameters(t *testing.T) {
	requireGit(t)
	const correctToken = "correct-token-the-ambient-parameters-know"
	bare := newBareRepoWithCommit(t)
	server := httptest.NewServer(&authenticatingGitHandler{
		token:   correctToken,
		handler: &gitSmartHTTPHandler{bareDir: bare, receivePackAllowed: true},
	})
	defer server.Close()
	encoded := base64.StdEncoding.EncodeToString([]byte(gitUsername + ":" + correctToken))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.extraHeader'='Authorization: Basic "+encoded+"'")
	f := NewForgejo(server.URL, "", server.Client(), testLogger())
	err := f.lsRemoteProbe(t.Context(), server.URL)
	require.Error(t, err, "an anonymous ls-remote against a token-gated repo must fail -- it must NEVER be rescued by a hostile ambient GIT_CONFIG_PARAMETERS the parent process happens to have set")
}

// TestDropGitCurlVerbose_RemovesTheKeyRegardlessOfValue mirrors
// internal/gittransport's identically-named test for the copy of
// dropGitCurlVerbose ported into this package: unlike every other
// GIT_TRACE* variable, git only presence-checks GIT_CURL_VERBOSE, so "0"
// and "" both still count as "set" and both turn curl tracing (which
// would print the injected Authorization header to stderr) on. Only an
// absent key is guaranteed to leave it off.
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

// TestForgejo_GitAuthEnv_NeverCarriesGitCurlVerbose pins the same
// guarantee at the exact function the bug lived in: gitAuthEnv's returned
// environment must never contain a GIT_CURL_VERBOSE key, even when the
// ambient environment this process happens to run under carries a
// hostile GIT_CURL_VERBOSE=0.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestForgejo_GitAuthEnv_NeverCarriesGitCurlVerbose(t *testing.T) {
	t.Setenv("GIT_CURL_VERBOSE", "0")
	f := NewForgejo("forgejo.example.com", "some-token", nil, testLogger())
	env := f.gitAuthEnv(t.TempDir())
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		assert.NotEqual(t, "GIT_CURL_VERBOSE", name, "gitAuthEnv must never emit a GIT_CURL_VERBOSE key -- git presence-checks it, so any value (including \"0\", inherited or overridden) turns curl tracing on")
	}
}
