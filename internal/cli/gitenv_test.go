package cli

import (
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
// BEFORE: the server's second request carried "Authorization: Basic
// <base64 of pwned-user:pwned-secret>" -- the enclosing clone's helper ran
// and answered. AFTER: no Authorization header arrives at all.
//
// GIT_TERMINAL_PROMPT=0 keeps the AFTER case from blocking on a terminal
// prompt when no helper answers; it does not affect whether a configured
// helper is consulted, which is the behaviour under test.
//
// Deliberately no t.Parallel(): t.Chdir and t.Setenv are both process-global.
func TestExecGitRefs_LsRemote_IgnoresEnclosingRepoCredentialHelper(t *testing.T) {
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	srv := newRecordingServer(t, http.StatusUnauthorized)
	helper := `!f() { echo username=pwned-user; echo password=pwned-secret; }; f`
	t.Chdir(hostileRepo(t, "credential.helper", helper))

	_, err := execGitRefs{}.LsRemote(t.Context(), srv.url+"/git/bobcob7/doc-server.git", nil, []string{"refs/heads/main"})

	require.Error(t, err, "the server always answers 401, so ls-remote must fail -- what matters is what it sent while failing")
	require.Positive(t, srv.count(), "git must have reached the server at all, or this test proves nothing")
	assert.Empty(t, srv.authorizations(), "the enclosing clone's credential.helper must never supply a credential to a request loam made")
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
	t.Run("CloneRoot", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
		got, err := execGitLookup{}.CloneRoot(named)
		require.NoError(t, err)
		assert.Equal(t, resolvedPath(t, named), resolvedPath(t, got), "an ambient GIT_DIR must not decide which clone root loam reports")
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
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
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
