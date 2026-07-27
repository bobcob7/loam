package gitpushsuite

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/handler"
	gitpkg "github.com/bobcob7/loam/internal/handler/git"
	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/mirrorreconcile"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/server"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// repoName is the one enrolled repo every test in this package pushes
// against -- there is no need for more than one, since every case varies
// the ref/identity/role, never the repo.
const repoName = "acme/widgets"

// testLogger builds a discard-everything *slog.Logger, matching this
// repo's test-logger convention.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// shortTempDir returns a fresh, short-named temp directory. Unlike
// t.TempDir() -- whose path embeds the full, often long, subtest-qualified
// test name -- this stays well under unix domain sockets' ~104-byte
// sun_path limit, which the policy socket below binds into
// (dataDir + "/hook.sock"). See cmd/server/main_integration_test.go's
// shortDataDir (same shape, same reason) and internal/hooksocket/
// e2e_test.go's own identical duplicate for this repo's established
// precedent of reproducing this helper per-package rather than exporting
// it, since every existing copy lives in an external test package that
// cannot import an unexported sibling helper.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gps")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// runGit runs a real git subcommand in dir, failing the test immediately
// on a nonzero exit.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// seedBareMirror creates a real bare mirror seeded with one commit on
// "main", exactly what internal/handler/git and internal/hooksocket's own
// tests seed.
func seedBareMirror(t *testing.T, mirrorDir string) {
	t.Helper()
	src := shortTempDir(t)
	runGit(t, src, "init", "--quiet", "--initial-branch=main")
	runGit(t, src, "config", "user.email", "seed@example.com")
	runGit(t, src, "config", "user.name", "seed")
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("hello\n"), 0o644))
	runGit(t, src, "add", "f.txt")
	runGit(t, src, "commit", "--quiet", "-m", "init")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGit(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
}

// mirrorRefSHA reads back ref's current commit SHA directly from the bare
// mirror via a separate `git --git-dir=... rev-parse`, never trusting this
// suite's own push helpers for the "did it actually move" proof. An empty
// string means ref does not exist (git rev-parse --verify --quiet exits 1
// for that, and only that, case -- confirmed against real git); any other
// failure (a malformed mirrorDir, git missing, a permissions error) is a
// hard test-fixture error, not silently folded into "absent", so it fails
// the test immediately instead of letting a broken fixture masquerade as
// "the ref was never created."
func mirrorRefSHA(t *testing.T, mirrorDir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+mirrorDir, "rev-parse", "--verify", "--quiet", ref)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		require.ErrorAsf(t, err, &exitErr, "mirrorRefSHA: git rev-parse failed in a way that was not a plain nonzero exit: %v", err)
		require.Equal(t, 1, exitErr.ExitCode(), "mirrorRefSHA: git rev-parse --verify --quiet exited %d, not the documented 1-for-absent: %s", exitErr.ExitCode(), exitErr.Stderr)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// fakeRepoStore is a trivial, hand-written stand-in for
// internal/handler/git.RepoStore -- that package's own moq-generated mock
// lives in its own package-internal moq_test.go and is not importable from
// here, exactly as internal/hooksocket/e2e_test.go's own identical fixture
// documents.
type fakeRepoStore map[string]reposstore.Repo

func (f fakeRepoStore) GetRepoByName(_ context.Context, name string) (reposstore.Repo, error) {
	if repo, ok := f[name]; ok {
		return repo, nil
	}
	return reposstore.Repo{}, reposstore.ErrNotFound
}

// callTracker records every work-branch name the policy socket's store was
// asked to resolve. This is the suite's proof that a rejection happened
// (or did not happen) at the mechanism it claims: cases rejected by the
// hook (or accepted by it, then blocked by git's own config) always
// resolve at least one branch; cases rejected by httpauth.GitIdentity or
// handler.GitRoleGate -- BEFORE any git process is even spawned -- resolve
// none at all, because the hook itself never runs.
type callTracker struct {
	mu    sync.Mutex
	calls []string
}

func (c *callTracker) record(branch string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, branch)
}

// Calls returns every branch name looked up so far, in order.
func (c *callTracker) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// trackingWorkBranchStore is hooksocket.WorkBranchStore backed by a plain
// map of branchName -> workbranchstore.WorkBranch (repoName is fixed, this
// suite never exercises more than one repo), instrumented by tracker so
// tests can assert whether the policy socket was ever actually consulted.
type trackingWorkBranchStore struct {
	branches map[string]workbranchstore.WorkBranch
	tracker  *callTracker
}

func (s trackingWorkBranchStore) GetWorkBranch(_ context.Context, repo, branch string) (workbranchstore.WorkBranch, error) {
	s.tracker.record(branch)
	if repo == repoName {
		if wb, ok := s.branches[branch]; ok {
			return wb, nil
		}
	}
	return workbranchstore.WorkBranch{}, fmt.Errorf("branch %s/%s: %w", repo, branch, workbranchstore.ErrNotFound)
}

// stubRoleStore backs handler.NewCapabilityChecker with the same fixed
// role->capability table docs/web-spec.md -> RoleService's two built-in
// roles define (and internal/db/migrations/files/0001_init.up.sql actually
// seeds in production): author carries git.clone+git.push, reviewer
// carries git.clone only. Mirrors internal/server/gitrolegate_test.go's own
// identical stubRoleStore -- not importable from here, since that one is
// unexported in a different package.
type stubRoleStore struct{}

func (stubRoleStore) RoleCapabilities(_ context.Context, role string) ([]handler.Capability, error) {
	switch role {
	case "author":
		return []handler.Capability{handler.CapabilityGitClone, handler.CapabilityGitPush}, nil
	case "reviewer":
		return []handler.Capability{handler.CapabilityGitClone}, nil
	default:
		return nil, nil
	}
}

// stackEnv is one fully-wired real stack: a real bare mirror with the real
// (or, for the cross-checks in crosscheck_test.go, a deliberately fake)
// hook installed, an httptest.Server fronting the EXACT production chain
// cmd/server/main.go composes -- httpauth.Auth.GitIdentity ->
// handler.GitRoleGate -> internal/handler/git.Handler -- and (unless
// startSocket is false) a real running hooksocket.Server the hook can
// actually reach.
type stackEnv struct {
	srv       *httptest.Server
	mirrorDir string
	tracker   *callTracker
}

// newStack wires the full stack. branches seeds the policy socket's
// work-branch registry (nil/empty is legitimate: every push is then
// against an unregistered ref). hookBinaryPath is the executable installed
// at the mirror's hooks/pre-receive -- production always passes
// loamhookBinary; crosscheck_test.go substitutes a deliberately different
// one to prove which mechanism actually rejects each case. startSocket
// false never starts the policy socket at all, reproducing the fail-
// closed scenario failclosed_test.go's TestFailClosed_PolicySocketDown_
// PushRejectedThroughRealHTTPHeaders exercises through this suite's own
// real-HTTP-identity fixture -- the one thing internal/hooksocket/
// e2e_test.go's own TestE2E_PolicySocketDown_PushFailsClosed does not
// cover, since that test injects identity into request context rather
// than sending real Loam-Agent-* headers over HTTP.
func newStack(t *testing.T, branches map[string]workbranchstore.WorkBranch, hookBinaryPath string, startSocket bool) stackEnv {
	t.Helper()
	dataDir := shortTempDir(t)
	mirrorDir := mirrorpath.Dir(dataDir, repoName)
	seedBareMirror(t, mirrorDir)
	require.NoError(t, mirrorreconcile.ReconcileMirror(t.Context(), mirrorDir, hookBinaryPath))
	tracker := &callTracker{}
	if startSocket {
		store := trackingWorkBranchStore{branches: branches, tracker: tracker}
		socketPath := filepath.Join(dataDir, "hook.sock")
		policyServer, err := hooksocket.Listen(socketPath, store, nil, testLogger())
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			policyServer.Run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("policy socket server did not shut down within 5s")
			}
		})
	}
	repos := fakeRepoStore{repoName: reposstore.Repo{Name: repoName}}
	gitHandler := gitpkg.New(dataDir, repos, testLogger())
	checker := handler.NewCapabilityChecker(stubRoleStore{})
	gate := handler.NewGitRoleGate(checker, testLogger())
	auth := httpauth.New("admin", "unused-admin-password")
	// Reusing internal/server.Router.RegisterGit itself -- rather than a
	// hand-rolled http.NewServeMux() carrying the identical two-line
	// wrap -- makes this the SAME code cmd/server/main.go's
	// registerGitService runs, not merely code that looks equivalent by
	// inspection: a future wrapper added inside RegisterGit is picked up
	// here automatically.
	router := server.New(auth)
	router.RegisterGit("/git/", gate.Middleware(gitHandler))
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	return stackEnv{srv: srv, mirrorDir: mirrorDir, tracker: tracker}
}

// cloneWithIdentity clones env's repo, and -- unless agentName is empty,
// which stands in for a plain, never-configured clone -- writes the three
// Loam-Agent-* headers into the clone's OWN git config as real
// http.extraHeader entries, exactly as `loam clone` does (docs/git-spec.md
// -> "Identity on Git Operations": "loam clone writes these into the
// clone's git config as http.extraHeader entries"). This is what makes
// this suite's identity travel as real HTTP headers a real git client
// sends, not a context value a test wires in directly -- the fidelity gap
// internal/hooksocket/e2e_test.go's own fixture (which injects identity
// straight into request context via a withIdentity wrapper) does not
// close.
func cloneWithIdentity(t *testing.T, env stackEnv, agentName, agentID, agentRole string) string {
	t.Helper()
	workspace := shortTempDir(t)
	clonePath := filepath.Join(workspace, "clone")
	// The identity headers are passed to `git clone` itself via --config
	// (never added afterward): cloning is gated by git.clone exactly like
	// pushing is gated by git.push (docs/git-spec.md -> "Operations & Role
	// Gates"), so the very first info/refs request must already carry
	// them -- this is why internal/cli/clone.go's own Clone method passes
	// these same three headers as clone-time --config arguments (via
	// identityHeaders) rather than writing them into the config after the
	// clone completes.
	args := []string{"clone", "--quiet"}
	if agentName != "" {
		args = append(args, "--config", "http.extraheader=Loam-Agent-Name: "+agentName)
	}
	if agentID != "" {
		args = append(args, "--config", "http.extraheader=Loam-Agent-Id: "+agentID)
	}
	if agentRole != "" {
		args = append(args, "--config", "http.extraheader=Loam-Agent-Role: "+agentRole)
	}
	args = append(args, env.srv.URL+"/git/acme/widgets.git", clonePath)
	runGit(t, workspace, args...)
	runGit(t, clonePath, "config", "user.email", "pusher@example.com")
	runGit(t, clonePath, "config", "user.name", "pusher")
	return clonePath
}

// clearIdentity strips every http.extraheader entry cloneWithIdentity may
// have written, reproducing exactly the request shape a plain,
// never-`loam clone`-bootstrapped git client sends: no Loam-Agent-* headers
// at all.
func clearIdentity(t *testing.T, clonePath string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "config", "--unset-all", "http.extraheader")
	cmd.Dir = clonePath
	// A clone with no extraheader entries at all makes --unset-all exit
	// nonzero (nothing to unset); that is not a failure of this helper's
	// own contract, so its result is deliberately ignored.
	_ = cmd.Run()
}

// commitFile writes fileName with fixed content into clonePath and commits
// it, returning nothing -- callers push separately, since some tests need
// more than one commit (the force-push and delete cases) or want to push
// the SAME commit to two different refs (the atomicity case).
func commitFile(t *testing.T, clonePath, fileName, message string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(clonePath, fileName), []byte("content\n"), 0o644))
	runGit(t, clonePath, "add", fileName)
	runGit(t, clonePath, "commit", "--quiet", "-m", message)
}

// pushRefs pushes clonePath's HEAD to every one of refspecs on the real
// server in a SINGLE `git push` invocation, returning git's own combined
// stdout+stderr and whether it succeeded. Never fails the test itself:
// every caller asserts on the outcome directly.
func pushRefs(t *testing.T, clonePath string, refspecs ...string) (string, error) {
	t.Helper()
	args := append([]string{"push", "origin"}, refspecs...)
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = clonePath
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pushRef is pushRefs for the common single-ref case, using the
// "HEAD:<ref>" shape every other case in this suite needs.
func pushRef(t *testing.T, clonePath, refName string) (string, error) {
	t.Helper()
	return pushRefs(t, clonePath, "HEAD:"+refName)
}
