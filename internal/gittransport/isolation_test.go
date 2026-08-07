package gittransport

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// TestTransport_LsRemoteIgnoresEnclosingRepositoryConfig closes the last
// gap loam-54ze's sweep found on this side. Every other isolation test in
// this file poisons the SYSTEM, GLOBAL or AMBIENT-ENVIRONMENT config layer.
// None of them touched the LOCAL one -- the config of whatever repository
// encloses this process's working directory -- and gitEnv's defences do not
// reach it: measured against git 2.50.1, GIT_CONFIG_NOSYSTEM plus a
// nonexistent GIT_CONFIG_GLOBAL leave an enclosing url.insteadOf fully in
// effect.
//
// insteadOf is used as the probe rather than a header because it is the
// setting no reset can undo and the one with the worst consequence: it
// rewrites the URL, so the request goes to a host loam never named and
// nothing loam sends is even relevant. Clone and LsRemote are the two
// methods that reach runRaw directly, with no --git-dir in argv, so they
// were the two that performed discovery at all.
//
// BEFORE gitEnv set GIT_DIR: the attacker server received the request and
// the intended server received nothing. AFTER: the reverse. Asserting on
// which SERVER was reached, rather than on the arguments passed, is the
// only way to see it -- the arguments were always correct.
//
// Deliberately no t.Parallel(): t.Chdir is process-global state, and
// testing.T.Chdir panics from a parallel test.
func TestTransport_LsRemoteIgnoresEnclosingRepositoryConfig(t *testing.T) {
	requireGit(t)
	intended, intendedHits := newHitCountingServer(t)
	attacker, attackerHits := newHitCountingServer(t)
	t.Chdir(repoWithConfig(t, "url."+attacker+"/.insteadOf", intended+"/"))

	_, err := New(&staticCredentialSource{}, newGitCredsConverter(), testLogger()).LsRemote(t.Context(), "", intended+"/acme/widgets.git")

	require.Error(t, err, "neither stub is a real smart-HTTP backend, so ls-remote must fail -- what matters is which host it failed against")
	assert.Positive(t, intendedHits(), "the request must reach the host this call named")
	assert.Zero(t, attackerHits(), "an enclosing repository's url.insteadOf must never redirect a loam transport request")
}

// TestTransport_LsRemoteIgnoresAmbientGitDir is the same guarantee reached
// by the other door. GIT_DIR does not merely point discovery somewhere
// else, it REPLACES it: a loam server process that inherited one (git sets
// it on every process it spawns, so anything running under a hook or an
// alias has one) would read that repository's config on every Clone and
// LsRemote regardless of its own working directory.
//
// BEFORE: the attacker server was reached. AFTER: gitEnv drops the
// inherited value and supplies its own, pointing at a path that does not
// exist.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestTransport_LsRemoteIgnoresAmbientGitDir(t *testing.T) {
	requireGit(t)
	intended, intendedHits := newHitCountingServer(t)
	attacker, attackerHits := newHitCountingServer(t)
	t.Setenv("GIT_DIR", filepath.Join(repoWithConfig(t, "url."+attacker+"/.insteadOf", intended+"/"), ".git"))

	_, err := New(&staticCredentialSource{}, newGitCredsConverter(), testLogger()).LsRemote(t.Context(), "", intended+"/acme/widgets.git")

	require.Error(t, err)
	assert.Positive(t, intendedHits(), "the request must reach the host this call named")
	assert.Zero(t, attackerHits(), "an ambient GIT_DIR must never decide which repository's config a loam transport request reads")
}

// TestDropInheritedRepoVars_RemovesEveryRepositoryLocatingVariable pins the
// deny list against a HARDCODED LITERAL, and that is the entire point of
// how it is written.
//
// The previous version built its input with `for name := range
// inheritedRepoVars` and then checked membership against that same map. It
// could not fail: deleting an entry removed it from the input and from the
// expectation together, and the `assert.Len` on the survivors moved in
// lockstep too. Deleting GIT_TEMPLATE_DIR left it GREEN. Assertion and
// subject shared a code path, so no deletion from the server-side deny list
// was detectable by any test in this package -- while the internal/cli
// sibling, which spells its list out, caught exactly that mutation.
//
// The round-2 note about this test being "adequate only as a pair with
// TestTransport_GitEnvCarriesExactlyOneGitDir" was true about WIRING and
// silent about MEMBERSHIP, which made the hole harder to spot rather than
// easier. Both are now covered: the literal below catches a deletion, and
// the sibling catches gitEnv ceasing to call this at all.
//
// Keeping the two lists in sync is a manual obligation, deliberately: an
// automated cross-check would reintroduce the shared code path that caused
// this.
func TestDropInheritedRepoVars_RemovesEveryRepositoryLocatingVariable(t *testing.T) {
	t.Parallel()
	mustDrop := []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_CONFIG", "GIT_TEMPLATE_DIR",
	}
	environ := []string{"PATH=/usr/bin", "HTTPS_PROXY=http://proxy.example"}
	for _, name := range mustDrop {
		environ = append(environ, name+"=/some/hostile/path")
	}

	filtered := dropInheritedRepoVars(environ)

	surviving := make(map[string]struct{}, len(filtered))
	for _, kv := range filtered {
		name, _, _ := strings.Cut(kv, "=")
		surviving[name] = struct{}{}
	}
	for _, name := range mustDrop {
		_, stillThere := surviving[name]
		assert.False(t, stillThere, "%s must be dropped -- it relocates a repository or plants executable code, and must never be inherited", name)
	}
	assert.Len(t, filtered, 2, "only the two unrelated entries should survive -- this package's env is os.Environ() plus overrides, so over-filtering is its own defect")
	assert.Contains(t, surviving, "HTTPS_PROXY", "a real network git invocation legitimately wants the host's proxy configuration")
}

// newHitCountingServer returns an HTTP server's URL and a func reporting how
// many requests it has received. Two of them, one standing in for the host
// loam named and one for the host a hostile config redirects to, are what
// make "where did the request actually go" observable at all.
func newHitCountingServer(t *testing.T) (string, func() int) {
	t.Helper()
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// repoWithConfig builds a real git repository whose config carries one
// key/value pair -- standing in for "the repository this process happens to
// be sitting in", which on a server is whatever directory it was started
// from and on a developer machine is a checkout.
func repoWithConfig(t *testing.T, key, value string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet"}, {"config", key, value}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	return dir
}
