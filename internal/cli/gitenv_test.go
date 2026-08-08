package cli

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- loam-54ze: the working directory must never decide anything ---
//
// Every test below is shaped the same way, and the shape is the point:
// build a HOSTILE repository (or a hostile ambient environment), put the
// code under test where a real agent would be standing, and assert on what
// the OTHER END actually received. Asserting that loam passed its own
// values proves nothing -- it always did, and the inherited ones still won.
//
// Each of these fails on the pre-loam-54ze tree. The "before" state is
// recorded in each test's own comment, measured by reverting just the one
// line that detaches the call site.

// hostileRepo builds a git repository whose config carries whatever
// key/value pairs the caller names, standing in for "a clone some other
// agent bootstrapped, which this agent has cd'd into". It is a real working
// copy, not a bare repo, because that is what an agent's cwd actually is.
func hostileRepo(t *testing.T, keyValues ...string) string {
	t.Helper()
	require.Zero(t, len(keyValues)%2, "keyValues must be key, value, key, value, ...")
	dir := t.TempDir()
	mustRunGit(t, dir, "init", "--quiet", "-b", "main")
	for i := 0; i < len(keyValues); i += 2 {
		mustRunGit(t, dir, "config", "--add", keyValues[i], keyValues[i+1])
	}
	return dir
}

// recordingServer is an HTTP server that records every request it receives
// and answers with status. Two of them, one standing in for the host loam
// named and one for the host a hostile config redirects to, are what make
// "where did the request actually go" observable at all.
type recordingServer struct {
	url  string
	mu   sync.Mutex
	reqs []*http.Request
}

func newRecordingServer(t *testing.T, status int) *recordingServer {
	t.Helper()
	rec := &recordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.Clone(r.Context()))
		rec.mu.Unlock()
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="loam"`)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	rec.url = srv.URL
	return rec
}

// headerValues returns the values of header name across EVERY request
// recorded so far, under the same lock the handler writes with.
//
// Two deliberate choices, both earned on this bead:
//
//   - Locked, not a bare captured-header variable. A test that reads
//     handler-written state across two requests races the handler goroutine
//     -- caught by -race on exactly this file.
//   - AGGREGATE, not last-request-only. An earlier version returned only the
//     most recent request's values, which is unsafe for the NEGATIVE
//     assertions that use it: "this header was absent" would hold if the
//     header arrived on any request but the last, and the guard on request
//     count is a Positive rather than an Equal. Aggregating makes absence
//     mean absence, and clear() is what scopes it to a single phase. This is
//     the same idiom authorizations() already used.
func (r *recordingServer) headerValues(name string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var got []string
	for _, req := range r.reqs {
		got = append(got, req.Header.Values(name)...)
	}
	return got
}

// clear discards everything recorded so far, so a test can attribute the
// next request unambiguously to the step that follows.
func (r *recordingServer) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = nil
}

func (r *recordingServer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// authorizations returns every distinct Authorization header value the
// server was sent, so a test can assert on what credential arrived rather
// than merely on how many requests did.
func (r *recordingServer) authorizations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var got []string
	for _, req := range r.reqs {
		if v := req.Header.Get("Authorization"); v != "" {
			got = append(got, v)
		}
	}
	return got
}

// TestExecGitRefs_LsRemote_IgnoresEnclosingRepoInsteadOf is the worst of
// the inherited-config family and the one no empty-string reset can close:
// url.<base>.insteadOf rewrites the URL ITSELF, so the request goes
// somewhere loam never named and no header loam sends is even relevant.
//
// BEFORE (LsRemote calling runGitOutput(ctx, "", ...)): the attacker server
// received the request and the intended server received nothing --
// `ls-remote` performed ordinary repository discovery from the cwd and
// honoured the enclosing clone's rewrite. Measured on this tree by
// reverting that one line.
//
// AFTER: GIT_DIR points at a path that does not exist, so no repository is
// discovered, no local config is read, and the request lands where loam
// addressed it.
//
// Deliberately no t.Parallel(): t.Chdir is process-global state, and
// testing.T.Chdir panics from a parallel test.
func TestExecGitRefs_LsRemote_IgnoresEnclosingRepoInsteadOf(t *testing.T) {
	intended := newRecordingServer(t, http.StatusNotFound)
	attacker := newRecordingServer(t, http.StatusNotFound)
	t.Chdir(hostileRepo(t, "url."+attacker.url+"/.insteadOf", intended.url+"/"))

	_, err := execGitRefs{}.LsRemote(t.Context(), intended.url+"/git/bobcob7/doc-server.git", nil, []string{"refs/heads/main"})

	require.Error(t, err, "neither stub is a real smart-HTTP backend, so ls-remote itself must fail -- what matters is which host it failed against")
	assert.Positive(t, intended.count(), "the request must reach the host loam named")
	assert.Zero(t, attacker.count(), "the enclosing clone's url.insteadOf must not redirect a loam request to a host loam never named")
}

// TestExecGitRefs_LsRemote_IgnoresEnclosingRepoCredentialHelper is the
// second of the two settings worth thinking hardest about (the other,
// core.hooksPath, is unreachable here -- see gitenv.go's residual list --
// because no discovery-performing CLI invocation writes a ref or checks out
// a tree, and hooks fire on neither ls-remote nor clone).
//
// A credential helper hands out SECRETS. An inherited one supplies a
// credential loam did not choose to whatever host the request reached, and
// the ONLY way to see that is to make the server demand authentication and
// look at what arrives on the retry.
//
// BEFORE: the server received a second request carrying "Authorization:
// Basic cHduZWQtdXNlcjpwd25lZC1zZWNyZXQ=" -- base64 of exactly this
// fixture's pwned-user:pwned-secret, so the enclosing clone's helper
// demonstrably ran and answered. AFTER: one request, no Authorization
// header at all.
//
// The three environment settings below are what make that a MEASUREMENT
// rather than a coincidence, and every one of them was earned:
//
//   - Written naively, this test passed for the wrong reason. git config's
//     credential.helper is MULTI-valued and system config is consulted
//     first, so on a developer machine the macOS system gitconfig's
//     osxkeychain answered before the enclosing repo's helper was ever
//     reached -- and it answered with anyuser:push-token, a credential
//     internal/fakeforge's own tests had cached against http://127.0.0.1
//     (osxkeychain keys by protocol+host and IGNORES the port, the hazard
//     internal/gittransport and internal/gitrun both document). Worse, the
//     BEFORE run's 401 made git ERASE that keychain entry, so the AFTER run
//     found nothing to send and "passed" on an artifact of the run before
//     it. GIT_CONFIG_NOSYSTEM and a nonexistent GIT_CONFIG_GLOBAL remove
//     every credential source except the one under test.
//   - HOME is redirected because libcurl consults ~/.netrc unconditionally,
//     outside git's credential-helper machinery entirely.
//
// None of the three is on gitSubprocessEnv's deny list, so all three reach
// the subprocess identically in both conditions: the only thing that
// differs between BEFORE and AFTER is the fix.
//
// GIT_TERMINAL_PROMPT=0 keeps the AFTER case from blocking on a terminal
// prompt when no helper answers; it does not affect whether a configured
// helper is consulted, which is the behaviour under test.
//
// Deliberately no t.Parallel(): t.Chdir and t.Setenv are both process-global.
func TestExecGitRefs_LsRemote_IgnoresEnclosingRepoCredentialHelper(t *testing.T) {
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global-gitconfig"))
	t.Setenv("HOME", t.TempDir())
	srv := newRecordingServer(t, http.StatusUnauthorized)
	helper := `!f() { echo username=pwned-user; echo password=pwned-secret; }; f`
	t.Chdir(hostileRepo(t, "credential.helper", helper))

	_, err := execGitRefs{}.LsRemote(t.Context(), srv.url+"/git/bobcob7/doc-server.git", nil, []string{"refs/heads/main"})

	require.Error(t, err, "the server always answers 401, so ls-remote must fail -- what matters is what it sent while failing")
	require.Positive(t, srv.count(), "git must have reached the server at all, or this test proves nothing")
	assert.NotContains(t, srv.authorizations(), "Basic "+base64.StdEncoding.EncodeToString([]byte("pwned-user:pwned-secret")),
		"the enclosing clone's credential.helper must never supply a credential to a request loam made")
	assert.Empty(t, srv.authorizations(), "and with every other credential source removed, nothing at all should have been sent")
}

// TestExecGitRefs_LsRemote_IgnoresEnclosingRepoProxy pins the third
// inherited transport setting. http.proxy routes the request through a host
// of the hostile config's choosing, which sees the URL, the identity
// headers and any credential -- the same silent interception as insteadOf,
// one layer down.
//
// BEFORE: the "proxy" server received a request (an absolute-URI CONNECT or
// GET, depending on scheme) and the intended server received nothing.
// AFTER: the intended server is reached directly.
//
// Deliberately no t.Parallel(): t.Chdir is process-global.
func TestExecGitRefs_LsRemote_IgnoresEnclosingRepoProxy(t *testing.T) {
	intended := newRecordingServer(t, http.StatusNotFound)
	proxy := newRecordingServer(t, http.StatusNotFound)
	t.Chdir(hostileRepo(t, "http.proxy", proxy.url))

	_, err := execGitRefs{}.LsRemote(t.Context(), intended.url+"/git/bobcob7/doc-server.git", nil, []string{"refs/heads/main"})

	require.Error(t, err)
	assert.Positive(t, intended.count(), "the request must reach the host loam named, directly")
	assert.Zero(t, proxy.count(), "the enclosing clone's http.proxy must not interpose on a loam request")
}

// TestExecGitCloner_Clone_IgnoresAmbientGitConfigParameters covers the
// OTHER half of the working-directory boundary, for the one call site that
// was never vulnerable to the repository half.
//
// `git clone` reads no enclosing repository -- verified against git 2.50.1,
// and the reason the originally-reported defect hit ls-remote and not
// clone. But GIT_CONFIG_PARAMETERS is how git propagates `-c` to the
// processes it spawns, so a `loam clone` run from a hook, an alias, or
// `git rebase -x` inherits one, and it carries arbitrary config including
// url.insteadOf.
//
// BEFORE (Clone calling runGitCommand(ctx, "", ...), which passed no
// cmd.Env and so inherited the whole environment): the clone silently came
// from the attacker's repository -- assert on the COMMIT MESSAGE, because
// two clones that both merely "succeed" are indistinguishable.
// AFTER: GIT_CONFIG_PARAMETERS is stripped and the clone comes from the URL
// loam named.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestExecGitCloner_Clone_IgnoresAmbientGitConfigParameters(t *testing.T) {
	intended := bareRepoWithMarkerCommit(t, "INTENDED-UPSTREAM")
	attacker := bareRepoWithMarkerCommit(t, "ATTACKER-UPSTREAM")
	intendedURL, attackerURL := "file://"+intended, "file://"+attacker
	t.Setenv("GIT_CONFIG_PARAMETERS", "'url."+attackerURL+".insteadof'='"+intendedURL+"'")
	dest := filepath.Join(t.TempDir(), "doc-server")

	require.NoError(t, execGitCloner{}.Clone(t.Context(), intendedURL, "main", dest, nil))

	assert.Equal(t, "INTENDED-UPSTREAM", mustRunGit(t, dest, "log", "-1", "--format=%s"),
		"an ambient GIT_CONFIG_PARAMETERS must not rewrite the URL loam cloned from")
}

// TestExecGitRefs_RevParse_IgnoresAmbientGitDir is the finding that makes
// this a sweep rather than a second patch: an ambient absolute GIT_DIR
// OVERRIDES `-C dir`, so every one of this package's explicitly-addressed
// invocations was resolvable against a different repository. Measured
// against git 2.50.1: `GIT_DIR=<other>/.git git -C <dir> rev-parse
// --absolute-git-dir` reports <other>.
//
// BEFORE: RevParse against the named directory returned the OTHER
// repository's HEAD -- a SHA `work diff` then compared against the server's
// tip and reported on. AFTER: it returns the named directory's HEAD.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestExecGitRefs_RevParse_IgnoresAmbientGitDir(t *testing.T) {
	named, other := workingCopyWithMarkerCommit(t, "NAMED"), workingCopyWithMarkerCommit(t, "OTHER")
	namedHead := mustRunGit(t, named, "rev-parse", "HEAD")
	otherHead := mustRunGit(t, other, "rev-parse", "HEAD")
	require.NotEqual(t, namedHead, otherHead, "the fixture must make the two repositories distinguishable")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))

	got, err := execGitRefs{}.RevParse(t.Context(), named, "HEAD")

	require.NoError(t, err)
	assert.Equal(t, namedHead, got, "an ambient GIT_DIR must not override the directory loam addressed with -C")
}

// TestExecGitRefs_RevParse_InCallerCwdIgnoresAmbientGitDir is the same
// guard for the dir == "" form, which deliberately means "the caller's own
// working copy" (checkLocalCommitsPushed asking whether the clone the agent
// is standing in holds unpushed commits). Discovery is the feature there;
// an ambient GIT_DIR silently replacing the answer is not.
//
// BEFORE: returned the other repository's HEAD, so `work diff`'s unpushed-
// commit refusal was evaluated against a repository the agent was not in.
//
// Deliberately no t.Parallel(): t.Chdir and t.Setenv are process-global.
func TestExecGitRefs_RevParse_InCallerCwdIgnoresAmbientGitDir(t *testing.T) {
	cwd, other := workingCopyWithMarkerCommit(t, "CWD"), workingCopyWithMarkerCommit(t, "OTHER")
	cwdHead := mustRunGit(t, cwd, "rev-parse", "HEAD")
	require.NotEqual(t, cwdHead, mustRunGit(t, other, "rev-parse", "HEAD"))
	t.Chdir(cwd)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))

	got, err := execGitRefs{}.RevParse(t.Context(), "", "HEAD")

	require.NoError(t, err)
	assert.Equal(t, cwdHead, got, "an ambient GIT_DIR must not replace the caller's own working copy")
}

// TestExecGitLookup_IgnoresAmbientGitDir covers the workspace-inference
// seam, where the consequence is not a wrong SHA but a wrong REPO and a
// wrong WORK BRANCH: OriginURL feeds repoFromOriginURL, and CurrentBranch
// is what every "[repo] [work-branch]" command infers its second positional
// from. An ambient GIT_DIR made all three answer about the parent git's
// repository.
//
// BEFORE: origin resolved to the other repository's URL and the branch to
// its branch. Each subtest asserts on a value the two fixtures do not
// share, so "it returned something" cannot pass for "it returned the right
// thing".
//
// Deliberately no t.Parallel() anywhere here: t.Setenv is process-global.
func TestExecGitLookup_IgnoresAmbientGitDir(t *testing.T) {
	named := workingCopyWithMarkerCommit(t, "NAMED")
	other := workingCopyWithMarkerCommit(t, "OTHER")
	mustRunGit(t, named, "remote", "add", "origin", "https://loam.example/git/bobcob7/named.git")
	mustRunGit(t, other, "remote", "add", "origin", "https://elsewhere.example/git/attacker/other.git")
	mustRunGit(t, named, "checkout", "--quiet", "-b", "wb-named")
	mustRunGit(t, other, "checkout", "--quiet", "-b", "wb-other")

	t.Run("OriginURL", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		got, err := execGitLookup{}.OriginURL(named)
		require.NoError(t, err)
		assert.Equal(t, "https://loam.example/git/bobcob7/named.git", got, "an ambient GIT_DIR must not decide which repo loam thinks it is in")
	})
	t.Run("CurrentBranch", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		got, err := execGitLookup{}.CurrentBranch(named)
		require.NoError(t, err)
		assert.Equal(t, "wb-named", got, "an ambient GIT_DIR must not decide which work branch loam infers")
	})
	// CloneRoot is a CONTROL, not a third attack, and saying so is the
	// point: measured on the pre-fix tree this subtest PASSED while its two
	// siblings failed. `rev-parse --show-toplevel` reports the WORKING
	// TREE, which `-C` supplies directly, so an ambient GIT_DIR (which
	// redirects the git dir, not the work tree) does not move it. Left in
	// place because a green assertion that is green for a known reason is
	// worth more than a deleted one -- and because it pins that the fix did
	// not break the one method that was already correct.
	t.Run("CloneRootWasNeverVulnerable", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		got, err := execGitLookup{}.CloneRoot(named)
		require.NoError(t, err)
		assert.Equal(t, resolvedPath(t, named), resolvedPath(t, got), "--show-toplevel reports the work tree -C supplied, before and after this fix alike")
	})
}

// --- unit-level pins on the two mechanisms themselves ---

// TestGitSubprocessEnv_DropsEveryRepositoryLocatingVariable pins the deny
// list as a list, so a variable removed from it is a test failure rather
// than a silent regression -- and pins that everything ELSE survives, which
// is the deliberate difference from the server side's from-nothing model
// (see gitenv.go: `loam clone` behind a proxy or a private CA needs the
// user's own environment).
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestGitSubprocessEnv_DropsEveryRepositoryLocatingVariable(t *testing.T) {
	dropped := []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_CONFIG",
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_TEMPLATE_DIR",
		"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_17", "GIT_CONFIG_VALUE_17",
	}
	kept := []string{"HTTPS_PROXY", "SSL_CERT_FILE", "GIT_SSL_CAINFO", "GIT_TERMINAL_PROMPT"}
	for _, name := range append(append([]string{}, dropped...), kept...) {
		t.Setenv(name, "loam-54ze-fixture")
	}

	names := envNames(gitSubprocessEnv(""))

	for _, name := range dropped {
		assert.NotContains(t, names, name, "%s locates a repository or injects config and must never be inherited", name)
	}
	for _, name := range kept {
		assert.Contains(t, names, name, "%s is the user's own environment and must survive -- see gitenv.go on why the CLI's boundary is not the server's", name)
	}
}

// TestGitSubprocessEnv_DetachedSetsExactlyOneGitDir pins that the detached
// form does not merely APPEND a GIT_DIR alongside an inherited one. Env
// resolution is last-wins for exec.Cmd, so an appended override would
// happen to work -- but a single entry is what makes it true regardless,
// and the assertion on the count is what would catch the deny list being
// narrowed to exclude GIT_DIR itself.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestGitSubprocessEnv_DetachedSetsExactlyOneGitDir(t *testing.T) {
	t.Setenv("GIT_DIR", "/an/inherited/repository/.git")

	var got []string
	for _, kv := range gitSubprocessEnv("/detached/no-repository") {
		if name, value, _ := strings.Cut(kv, "="); name == "GIT_DIR" {
			got = append(got, value)
		}
	}

	assert.Equal(t, []string{"/detached/no-repository"}, got, "exactly one GIT_DIR, and it must be the detached one")
}

// TestGitSubprocessEnv_NonDetachedSetsNoGitDir pins the other half: a
// `-C dir` invocation must leave git to resolve the repository from the
// directory it was given, not be handed a GIT_DIR that would win over it.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestGitSubprocessEnv_NonDetachedSetsNoGitDir(t *testing.T) {
	t.Setenv("GIT_DIR", "/an/inherited/repository/.git")
	assert.NotContains(t, envNames(gitSubprocessEnv("")), "GIT_DIR")
}

// TestGitDetached_PointsAtAPathThatDoesNotExist pins the property the whole
// detachment mechanism rests on. git skips repository discovery when
// GIT_DIR is set, and reads a missing config file as "no config" -- both of
// which stop being true the moment something exists at that path.
func TestGitDetached_PointsAtAPathThatDoesNotExist(t *testing.T) {
	t.Parallel()
	gitDir, cleanup, err := gitDetached()
	require.NoError(t, err)

	_, statErr := os.Stat(gitDir)
	assert.True(t, os.IsNotExist(statErr), "the detached GIT_DIR must not exist, or git would discover a repository there")
	require.DirExists(t, filepath.Dir(gitDir), "its parent must exist and be ours, so nothing else can create it underneath us")

	cleanup()
	assert.NoDirExists(t, filepath.Dir(gitDir), "cleanup must remove the temp directory it created")
}

// TestGitDetached_IsPerInvocation pins that two concurrent invocations do
// not share a directory -- the property that makes "does not exist" hold
// rather than merely happen to hold.
func TestGitDetached_IsPerInvocation(t *testing.T) {
	t.Parallel()
	first, cleanupFirst, err := gitDetached()
	require.NoError(t, err)
	defer cleanupFirst()
	second, cleanupSecond, err := gitDetached()
	require.NoError(t, err)
	defer cleanupSecond()

	assert.NotEqual(t, first, second)
}

// --- fixtures ---

// bareRepoWithMarkerCommit builds a bare repo whose single commit on "main"
// carries marker as its subject, so a clone OF it is distinguishable from a
// clone of any other repo this file builds. Two clones that merely
// "succeeded" are not.
func bareRepoWithMarkerCommit(t *testing.T, marker string) string {
	t.Helper()
	src := workingCopyWithMarkerCommit(t, marker)
	bare := filepath.Join(t.TempDir(), "upstream.git")
	mustRunGit(t, "", "clone", "--quiet", "--bare", src, bare)
	return bare
}

// workingCopyWithMarkerCommit builds a non-bare repo on "main" with one
// commit whose subject is marker.
func workingCopyWithMarkerCommit(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	mustRunGit(t, dir, "init", "--quiet", "-b", "main")
	mustRunGit(t, dir, "config", "user.name", "fixture")
	mustRunGit(t, dir, "config", "user.email", "fixture@example.com")
	mustRunGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", marker)
	return dir
}

// envNames extracts just the variable names from an environment slice.
func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return names
}

// resolvedPath resolves symlinks so a comparison against git's own output
// is not defeated by macOS's /tmp -> /private/tmp.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}

// TestExecGitCloner_Clone_IgnoresAmbientGitTemplateDir is the only test here
// whose failure mode is ARBITRARY CODE EXECUTION rather than misdirection,
// and it exists because the reasoning that dismissed this whole family was
// wrong. The claim was "no discovery-performing CLI invocation writes a ref
// or checks out a tree". `git clone` checks out a tree and runs
// post-checkout out of the destination's hooks -- and GIT_TEMPLATE_DIR is
// what decides which hooks land there, since git copies the named
// directory's hooks/ into the new repository before that checkout.
//
// BEFORE (GIT_TEMPLATE_DIR absent from the deny list -- i.e. this change's
// own first revision, with detachment and every other strip in place): the
// attacker's post-checkout RAN and was left installed at
// dest/.git/hooks/post-checkout. AFTER: neither happens.
//
// The marker file is written by the hook itself, so a passing assertion
// means the hook did not run -- not merely that it was not copied. Both are
// asserted, because a hook that lands but does not fire this time is still
// a hook that fires on the agent's next checkout.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestExecGitCloner_Clone_IgnoresAmbientGitTemplateDir(t *testing.T) {
	upstream := bareRepoWithMarkerCommit(t, "INTENDED-UPSTREAM")
	marker := filepath.Join(t.TempDir(), "PWNED")
	template := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(template, "hooks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(template, "hooks", "post-checkout"),
		[]byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755))
	t.Setenv("GIT_TEMPLATE_DIR", template)
	dest := filepath.Join(t.TempDir(), "doc-server")

	require.NoError(t, execGitCloner{}.Clone(t.Context(), "file://"+upstream, "main", dest, nil))

	assert.NoFileExists(t, marker, "an ambient GIT_TEMPLATE_DIR must never execute code during `loam clone`")
	assert.NoFileExists(t, filepath.Join(dest, ".git", "hooks", "post-checkout"),
		"and must not leave a hook installed in the clone to fire on the agent's next checkout")
}

// TestExecGitCloner_Clone_IgnoresAmbientInitTemplateDirViaConfigParameters
// pins the code-execution channel this change closed WITHOUT KNOWING IT
// HAD. init.templatedir does what GIT_TEMPLATE_DIR does, and
// GIT_CONFIG_PARAMETERS -- which git itself sets on children of an alias --
// carries it.
//
// BEFORE (gitSubprocessEnv returning bare os.Environ()): the hook ran.
// AFTER: stripping GIT_CONFIG_PARAMETERS stops it. This is the sibling of
// Clone_IgnoresAmbientGitConfigParameters, which covers the same variable's
// misdirection half; this covers its execution half.
//
// Deliberately no t.Parallel(): t.Setenv is incompatible with a parallel
// ancestor.
func TestExecGitCloner_Clone_IgnoresAmbientInitTemplateDirViaConfigParameters(t *testing.T) {
	upstream := bareRepoWithMarkerCommit(t, "INTENDED-UPSTREAM")
	marker := filepath.Join(t.TempDir(), "PWNED")
	template := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(template, "hooks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(template, "hooks", "post-checkout"),
		[]byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'init.templatedir'='"+template+"'")
	dest := filepath.Join(t.TempDir(), "doc-server")

	require.NoError(t, execGitCloner{}.Clone(t.Context(), "file://"+upstream, "main", dest, nil))

	assert.NoFileExists(t, marker, "an ambient GIT_CONFIG_PARAMETERS carrying init.templatedir must never execute code during `loam clone`")
}

// TestGitSubprocessEnv_DetachedGitDirPointsOutsideTheEnclosingRepository
// pins gitSubprocessEnv, and NOT what its previous name and comment
// claimed.
//
// It was written as "the pin that makes reverting Clone to
// runGitCommand(ctx, \"\") fail". It does not: the reviewer performed
// exactly that revert and all four Clone tests passed, because this test
// never calls Clone at all -- it constructs the environment itself.
//
// The honest finding is better than the claim, and it is why no such pin is
// added instead: THAT GAP IS NOT CLOSABLE, because there is nothing to
// observe. Both runGitCommand and runDetachedGitCommand strip GIT_DIR from
// the environment; only the detached one then sets its own; and `git clone`
// consults GIT_DIR for nothing -- it establishes its own repository at the
// destination and reads no enclosing one. Measured on git 2.50.1 from
// inside a repository carrying a hostile url.insteadOf, core.hooksPath and
// http.extraHeader: cloning with and without a detached GIT_DIR produced
// BYTE-IDENTICAL destination configs and identical behaviour.
//
// So Clone's use of the detached path is genuine belt-and-braces -- it
// costs nothing, it is the right default for a call site that must read no
// repository, and it would matter immediately if git ever changed its mind
// about clone consulting GIT_DIR. What it is not is testable, and a test
// claiming otherwise is worse than no test.
//
// What IS pinned here is the mechanism the detached path depends on: that
// gitSubprocessEnv hands back a GIT_DIR outside whatever repository the
// caller is standing in, and that nothing exists at it.
//
// Deliberately no t.Parallel(): t.Chdir is process-global.
func TestGitSubprocessEnv_DetachedGitDirPointsOutsideTheEnclosingRepository(t *testing.T) {
	enclosing := workingCopyWithMarkerCommit(t, "ENCLOSING")
	t.Chdir(enclosing)

	gitDir, cleanup, err := gitDetached()
	require.NoError(t, err)
	defer cleanup()

	var got string
	for _, kv := range gitSubprocessEnv(gitDir) {
		if name, value, _ := strings.Cut(kv, "="); name == "GIT_DIR" {
			got = value
		}
	}
	require.Equal(t, gitDir, got, "the detached environment must carry its own GIT_DIR")
	assert.NotContains(t, got, enclosing, "and it must point outside the repository the caller is standing in")
	assert.NoFileExists(t, got)
}

// TestExecGitRefs_LsRemote_ResetsAGlobalExtraHeader is the test the
// `-c http.extraHeader=` reset in LsRemote never had.
//
// That line's own comment calls it "the one defence that covers a layer
// detachment does not", and that was true and unpinned: deleting the line
// left the entire internal/cli suite green. The test that used to cover it,
// SendsOnlyTheHeadersItWasGiven, silently became DETACHMENT's test once
// detachment made the enclosing clone unreadable -- so the reset lost its
// only coverage to a fix that did not replace it. That is the same shape as
// the fixture trap caught for the header FIELDS, missed for the header
// RESET.
//
// The hostile header lives in GIT_CONFIG_GLOBAL, deliberately: that is the
// layer detachment does NOT reach (this package keeps honouring the user's
// own config -- see gitenv.go), so it isolates the reset as the only thing
// that can be doing the work. GIT_CONFIG_GLOBAL is not on the deny list, so
// it reaches the subprocess unmolested; GIT_CONFIG_NOSYSTEM and a
// redirected HOME remove the remaining ambient layers so the assertion has
// exactly one explanation.
//
// BEFORE (reset deleted): the server received Loam-Agent-Name twice --
// GLOBAL-ATTACKER first, then loam's own, and git accumulates rather than
// replaces, so the first one wins. AFTER: exactly the caller's three.
//
// Asserting Equal on the full value list, not Contains, is what makes this
// distinguish "reset away" from "dropped everything": `git config
// --get-all` is useless here (it dumps all values including the empty one
// and never applies reset semantics), so only the wire shows it.
//
// Deliberately no t.Parallel(): t.Chdir and t.Setenv are process-global.
func TestExecGitRefs_LsRemote_ResetsAGlobalExtraHeader(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig,
		[]byte("[http]\n\textraHeader = Loam-Agent-Name: GLOBAL-ATTACKER\n\textraHeader = Loam-Agent-Id: 999\n\textraHeader = Loam-Agent-Role: reviewer\n"), 0o600))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("HOME", t.TempDir())
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	headers := []string{"Loam-Agent-Name: grace-hopper", "Loam-Agent-Id: 3", "Loam-Agent-Role: author"}

	_, err := execGitRefs{}.LsRemote(t.Context(), srv.URL+"/git/bobcob7/doc-server.git", headers, []string{"refs/heads/main"})

	require.Error(t, err, "the stub is not a real smart-HTTP backend, so ls-remote must fail -- what matters is what it sent")
	require.NotNil(t, captured, "git must have sent a request before failing")
	assert.Equal(t, []string{"grace-hopper"}, captured.Values("Loam-Agent-Name"), "the global layer's identity must be reset away, and loam's own must survive")
	assert.Equal(t, []string{"3"}, captured.Values("Loam-Agent-Id"))
	assert.Equal(t, []string{"author"}, captured.Values("Loam-Agent-Role"))
}

// TestExecGitCloner_Clone_ResetsAGlobalExtraHeader is the wire-level
// counterpart to Clone's leading empty --config, and the sibling of
// LsRemote's ResetsAGlobalExtraHeader. It closes the last declared residual
// on `loam clone` (loam-54ze round 2).
//
// The hostile header lives in GIT_CONFIG_GLOBAL because that is the layer
// detachment deliberately does NOT reach -- so the reset is the only thing
// that can be doing the work, and a fixture that poisoned the enclosing
// repository instead would pass on detachment alone and prove nothing.
//
// BEFORE (no leading reset): the initial upload-pack GET carried
// Loam-Agent-Name twice, GLOBAL-ATTACKER first, and git accumulates rather
// than replaces -- so `loam clone` authenticated its own bootstrap as the
// attacker. AFTER: exactly the caller's three.
//
// Deliberately no t.Parallel(): t.Setenv is process-global.
func TestExecGitCloner_Clone_ResetsAGlobalExtraHeader(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig,
		[]byte("[http]\n\textraHeader = Loam-Agent-Name: GLOBAL-ATTACKER\n\textraHeader = Loam-Agent-Id: 999\n\textraHeader = Loam-Agent-Role: reviewer\n"), 0o600))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("HOME", t.TempDir())
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	headers := []string{"Loam-Agent-Name: grace-hopper", "Loam-Agent-Id: 3", "Loam-Agent-Role: author"}

	err := execGitCloner{}.Clone(t.Context(), srv.URL+"/git/bobcob7/doc-server.git", "main", filepath.Join(t.TempDir(), "doc-server"), headers)

	require.Error(t, err, "the stub is not a real smart-HTTP backend, so clone must fail -- what matters is what its FIRST request carried")
	require.NotNil(t, captured, "git must have sent the upload-pack info/refs GET before failing")
	assert.Equal(t, []string{"grace-hopper"}, captured.Values("Loam-Agent-Name"), "`loam clone` must bootstrap as the agent running it, never as a global config's identity")
	assert.Equal(t, []string{"3"}, captured.Values("Loam-Agent-Id"))
	assert.Equal(t, []string{"author"}, captured.Values("Loam-Agent-Role"))
}

// TestExecGitCloner_Clone_ResetPersistsForLaterOperations pins the
// behaviour change the reset brings beyond the initial fetch, because it is
// a change and should fail loudly if it ever stops holding. --config
// persists, so the clone's own later fetches and pushes carry the reset
// too: an agent's clone asserts that agent's identity and nothing the
// user's global config adds.
//
// Asserted on the WIRE, from inside the clone, rather than by reading
// `config --get-all` -- which cannot show reset semantics at all.
//
// Deliberately no t.Parallel(): t.Setenv is process-global.
func TestExecGitCloner_Clone_ResetPersistsForLaterOperations(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig,
		[]byte("[http]\n\textraHeader = Loam-Agent-Name: GLOBAL-ATTACKER\n"), 0o600))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("HOME", t.TempDir())
	upstream := bareRepoWithMarkerCommit(t, "INTENDED-UPSTREAM")
	dest := filepath.Join(t.TempDir(), "doc-server")
	require.NoError(t, execGitCloner{}.Clone(t.Context(), "file://"+upstream, "main", dest, []string{"Loam-Agent-Name: grace-hopper"}))
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	// A later operation run from INSIDE the clone, reading the clone's own
	// persisted config -- the shape every plain `git fetch`/`git push` an
	// agent runs afterward takes.
	_, _ = runGitOutput(t.Context(), dest, "ls-remote", srv.URL+"/git/bobcob7/doc-server.git")

	require.NotNil(t, captured, "git must have reached the server")
	assert.Equal(t, []string{"grace-hopper"}, captured.Values("Loam-Agent-Name"),
		"the persisted reset must keep a global http.extraHeader out of the clone's later operations too")
}

// TestExecGitCloner_Clone_ResetDoesNotBlockHeadersAddedAfterward pins the
// documented cost-and-workaround of Clone's persisted reset (see Clone's
// doc comment), so the documentation cannot quietly become false.
//
// The cost is real: a LEGITIMATE global http.extraHeader -- a corporate
// gateway token, a routing header -- is dropped from the clone's later
// operations along with any hostile one, and presents as an unexplained
// network failure with nothing implicating loam. The workaround is to
// re-add it to the clone's own config, which lands it AFTER the reset in
// the clone's own multi-valued list.
//
// Both halves are asserted in one test deliberately, because the pair is
// the claim: dropped before, present after, with loam's identity surviving
// throughout. Asserting only the second half would pass even if the reset
// had stopped working.
//
// The recording server captures ALL headers, not just Loam-*. That is not
// incidental: a fixture that filtered for loam's own headers could not see
// X-Corp-Route at all, and its absence would read as "not sent" whether or
// not it was -- the same filtered-instrument false negative this bead's
// review has now hit three times.
//
// Deliberately no t.Parallel(): t.Setenv is process-global.
func TestExecGitCloner_Clone_ResetDoesNotBlockHeadersAddedAfterward(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig,
		[]byte("[http]\n\textraHeader = X-Corp-Route: eu-west\n"), 0o600))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("HOME", t.TempDir())
	upstream := bareRepoWithMarkerCommit(t, "INTENDED-UPSTREAM")
	dest := filepath.Join(t.TempDir(), "doc-server")
	require.NoError(t, execGitCloner{}.Clone(t.Context(), "file://"+upstream, "main", dest, []string{"Loam-Agent-Name: grace-hopper"}))
	srv := newRecordingServer(t, http.StatusNotFound)

	_, _ = runGitOutput(t.Context(), dest, "ls-remote", srv.url+"/git/bobcob7/doc-server.git")
	require.Positive(t, srv.count(), "git must have reached the server")
	assert.Empty(t, srv.headerValues("X-Corp-Route"),
		"the documented COST: a legitimate global header is dropped from the clone's operations too")
	assert.Equal(t, []string{"grace-hopper"}, srv.headerValues("Loam-Agent-Name"))

	// The documented workaround, verbatim: re-add it to the clone's own
	// config, where it lands after the reset.
	require.NoError(t, execGitCloner{}.AddConfig(t.Context(), dest, "http.extraHeader", "X-Corp-Route: eu-west"))
	srv.clear()
	_, _ = runGitOutput(t.Context(), dest, "ls-remote", srv.url+"/git/bobcob7/doc-server.git")

	require.Positive(t, srv.count())
	assert.Equal(t, []string{"eu-west"}, srv.headerValues("X-Corp-Route"),
		"the documented WORKAROUND: a header re-added to the clone's own config lands after the reset and survives")
	assert.Equal(t, []string{"grace-hopper"}, srv.headerValues("Loam-Agent-Name"),
		"and loam's own identity must still be the only one asserted")
}
