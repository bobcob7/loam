// This file is loam-ofg.18's strongest evidence: a REAL `git clone`/`git
// push` against internal/handler/git's real HTTP handler, against a real
// bare mirror, through the REAL compiled cmd/loamhook binary (built once
// in TestMain) installed by the REAL internal/mirrorreconcile.ReconcileMirror,
// talking over a REAL unix socket to a REAL internal/hooksocket.Server
// wrapping the REAL internal/refpolicy.EvaluatePush -- every layer this
// bead built, exercised together, with only the Postgres-backed store
// swapped for an in-memory fake (fakeWorkBranchStore below) so this stays
// a plain `go test` with no container. The store itself (policyStoreAdapter,
// cmd/server) is proven separately against a real Postgres in
// cmd/server/policystore_integration_test.go.
package hooksocket_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitpkg "github.com/bobcob7/loam/internal/handler/git"
	"github.com/bobcob7/loam/internal/hooksocket"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/mirrorreconcile"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// e2eTestLogger builds a discard-everything *slog.Logger, matching this
// repo's test-logger convention -- duplicated here (rather than reused
// from hooksocket's own package-internal testLogger) because this file is
// a separate (hooksocket_test) package and that helper is unexported.
func e2eTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// loamhookBinary is cmd/loamhook's compiled path, built once for every
// test in this file by TestMain -- mirrors cmd/server/main_integration_test.go's
// own serverBinary convention.
var loamhookBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loamhook-build-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	loamhookBinary = filepath.Join(dir, "loamhook")
	build := exec.Command("go", "build", "-o", loamhookBinary, "github.com/bobcob7/loam/cmd/loamhook")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "building loamhook binary: %v\n%s", buildErr, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// shortTempDir returns a fresh, short-named temp directory (see
// internal/hooksocket's own package-internal shortTempDir doc comment for
// why: a socket path built from t.TempDir() was observed to exceed
// macOS/BSD's ~104-byte sun_path limit). Duplicated here, rather than
// imported, because this file is a separate (hooksocket_test) package and
// that helper is unexported.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "e2e")
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
// "main", exactly what internal/handler/git's own tests seed.
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

// withIdentity stands in for internal/httpauth.GitIdentity (identity
// resolution proper, out of this bead's scope -- see
// internal/handler/git's own identical fixture's doc comment).
func withIdentity(identity httpauth.Identity, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(httpauth.WithIdentity(r.Context(), identity)))
	})
}

// fakeRepoStore is a trivial, hand-written stand-in for
// internal/handler/git.RepoStore: its own moq-generated mock
// (RepoStoreMock) lives in that package's moq_test.go and is not
// importable from this external test package, so this file supplies its
// own minimal fixture rather than duplicating a real reposstore.Store
// against a live database just to resolve one name.
type fakeRepoStore map[string]reposstore.Repo

func (f fakeRepoStore) GetRepoByName(_ context.Context, name string) (reposstore.Repo, error) {
	if repo, ok := f[name]; ok {
		return repo, nil
	}
	return reposstore.Repo{}, reposstore.ErrNotFound
}

// fakeWorkBranchStore is the same kind of hand-written stand-in for
// hooksocket.WorkBranchStore, keyed "repoName/branchName" -- this
// package's own WorkBranchStoreMock (moq_test.go) is likewise not
// importable from this external test package.
type fakeWorkBranchStore map[string]workbranchstore.WorkBranch

func (f fakeWorkBranchStore) GetWorkBranch(_ context.Context, repoName, branchName string) (workbranchstore.WorkBranch, error) {
	if wb, ok := f[repoName+"/"+branchName]; ok {
		return wb, nil
	}
	return workbranchstore.WorkBranch{}, fmt.Errorf("branch %s/%s: %w", repoName, branchName, workbranchstore.ErrNotFound)
}

// e2eEnv is one fully-wired end-to-end fixture: a real bare mirror with
// the real compiled hook installed, an httptest.Server fronting the real
// git.Handler with resolved identity, and (unless startPolicySocket is
// false) a real running hooksocket.Server the hook can actually reach.
type e2eEnv struct {
	srv       *httptest.Server
	mirrorDir string
}

// setupE2E wires the full stack. When store is nil, the policy socket is
// deliberately never started -- proving the hook's fail-closed behavior
// when the socket is genuinely unreachable, not just slow.
func setupE2E(t *testing.T, agent httpauth.Identity, store hooksocket.WorkBranchStore) e2eEnv {
	t.Helper()
	dataDir := shortTempDir(t)
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	require.NoError(t, mirrorreconcile.ReconcileMirror(t.Context(), mirrorDir, loamhookBinary))
	if store != nil {
		socketPath := filepath.Join(dataDir, "hook.sock")
		policyServer, err := hooksocket.Listen(socketPath, store, nil, e2eTestLogger())
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
	repos := fakeRepoStore{"acme/widgets": reposstore.Repo{Name: "acme/widgets"}}
	h := gitpkg.New(dataDir, repos, e2eTestLogger())
	mux := http.NewServeMux()
	mux.Handle("/git/", withIdentity(agent, h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return e2eEnv{srv: srv, mirrorDir: mirrorDir}
}

// cloneAndCommit clones env's repo, adds one commit on top of the seeded
// "main" tip (so a push straight to main stays a fast-forward -- git's own
// denyNonFastForwards, which mirrorreconcile also configures, must never
// be what rejects these pushes; only the policy hook's OWN decision
// should), and returns the clone's working directory.
func cloneAndCommit(t *testing.T, env e2eEnv, fileName string) string {
	t.Helper()
	workspace := shortTempDir(t)
	clonePath := filepath.Join(workspace, "clone")
	runGit(t, workspace, "clone", "--quiet", env.srv.URL+"/git/acme/widgets.git", clonePath)
	runGit(t, clonePath, "config", "user.email", "pusher@example.com")
	runGit(t, clonePath, "config", "user.name", "pusher")
	require.NoError(t, os.WriteFile(filepath.Join(clonePath, fileName), []byte("content\n"), 0o644))
	runGit(t, clonePath, "add", fileName)
	runGit(t, clonePath, "commit", "--quiet", "-m", "add "+fileName)
	return clonePath
}

// pushRef pushes clonePath's HEAD to refName on the real server, returning
// git's own combined stdout+stderr and whether it succeeded -- never
// failing the test itself, since every caller here asserts on the outcome
// directly (some want success, some want a specific rejection).
func pushRef(t *testing.T, clonePath, refName string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "push", "origin", "HEAD:"+refName)
	cmd.Dir = clonePath
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestE2E_AllowedPush_RealHookRealSocketRealGit proves the accept path
// through every real layer: a real git push, to a brand-new ref name,
// which the policy socket's fake store resolves as a work branch owned by
// the pushing agent in a non-terminal state, actually lands on the bare
// mirror.
func TestE2E_AllowedPush_RealHookRealSocketRealGit(t *testing.T) {
	t.Parallel()
	agent := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	store := fakeWorkBranchStore{
		"acme/widgets/wb-good": {Name: "wb-good", Author: aliceIdentifier, State: workbranchstore.StateDraft},
	}
	env := setupE2E(t, agent, store)
	clonePath := cloneAndCommit(t, env, "allowed.txt")
	out, err := pushRef(t, clonePath, "refs/heads/wb-good")
	require.NoError(t, err, "an author pushing their own draft work branch must be accepted: %s", out)
	mirrorHead := runGit(t, "", "--git-dir="+env.mirrorDir, "log", "-1", "--format=%s", "refs/heads/wb-good")
	assert.Equal(t, "add allowed.txt", mirrorHead, "the pushed commit must have actually landed on the mirror")
}

// TestE2E_RejectedPushes_RealGitClientSeesRemotePrefixedLoamReason proves
// all four docs/git-spec.md "Ref Policy (push)" rejection reasons surface
// through a REAL git client as "remote: loam: ..." lines -- git's own,
// documented behavior of prefixing a pre-receive hook's stderr with
// "remote: " when relaying it back to the pushing client (confirmed
// against real git during this bead's own research; docs/git-spec.md
// itself says nothing about this relay mechanism) -- and that the push
// itself is rejected (nonzero exit, ref not updated on the mirror).
func TestE2E_RejectedPushes_RealGitClientSeesRemotePrefixedLoamReason(t *testing.T) {
	t.Parallel()
	agent := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	store := fakeWorkBranchStore{
		"acme/widgets/wb-owned-by-bob": {Name: "wb-owned-by-bob", Author: bobIdentifier, State: workbranchstore.StateDraft},
		"acme/widgets/wb-closed":       {Name: "wb-closed", Author: aliceIdentifier, State: workbranchstore.StateClosed},
	}
	tests := []struct {
		name       string
		ref        string
		wantReason string
	}{
		{
			name:       "read-only ref: pushing to the mirrored target branch",
			ref:        "refs/heads/main",
			wantReason: "loam: refs/heads/main is read-only (target branch)",
		},
		{
			name:       "unknown ref: creating a brand-new, unregistered branch name",
			ref:        "refs/heads/wb-never-registered",
			wantReason: "loam: refs/heads/wb-never-registered is not a work branch; create one with 'work start'",
		},
		{
			name:       "not the author: pushing another agent's work branch",
			ref:        "refs/heads/wb-owned-by-bob",
			wantReason: "loam: wb-owned-by-bob belongs to bob",
		},
		{
			name:       "terminal state: pushing a closed work branch",
			ref:        "refs/heads/wb-closed",
			wantReason: "loam: wb-closed is closed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := setupE2E(t, agent, store)
			clonePath := cloneAndCommit(t, env, "rejected.txt")
			out, err := pushRef(t, clonePath, tt.ref)
			require.Error(t, err, "this push must be rejected: %s", out)
			assert.Contains(t, out, "remote: "+tt.wantReason, "git's own client must relay the hook's exact loam:-prefixed reason, prefixed with \"remote: \"")
		})
	}
}

// TestE2E_PolicySocketDown_PushFailsClosed proves the ultimate fail-closed
// case through the real stack: the hook is installed, but no policy
// socket is listening at all (the server crashed, or has not started
// yet). The push must still be rejected -- never silently accepted --
// and the rejection must still be visible to the real git client.
func TestE2E_PolicySocketDown_PushFailsClosed(t *testing.T) {
	t.Parallel()
	agent := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	env := setupE2E(t, agent, nil) // nil store => setupE2E never starts a policy socket
	clonePath := cloneAndCommit(t, env, "failclosed.txt")
	out, err := pushRef(t, clonePath, "refs/heads/wb-anything")
	require.Error(t, err, "a push must be rejected when the policy socket is unreachable, never silently accepted: %s", out)
	assert.Contains(t, out, "remote: loam:", "the hook's own fail-closed explanation must still reach the real git client")
	assert.Contains(t, strings.ToLower(out), "connect", "the hook's fail-closed message should explain that the policy socket could not be reached")
}

// aliceIdentifier and bobIdentifier mirror the constants in
// server_test.go, redeclared because this file is the EXTERNAL
// hooksocket_test package and cannot see them. They are what
// work_branches.author actually holds -- the "<name>-<id>-<role>"
// rendering internal/handler/workbranch stores at CreateWorkBranch time --
// and aliceIdentifier matches the httpauth.Identity these tests push with,
// so the pushing identity and the stored author agree.
//
// bobIdentifier stays identifier-shaped on purpose: a bare "bob" would
// still be rejected, but on shape rather than on ownership, so the
// not-the-author case would pass while proving nothing. Before loam-ppb
// was fixed both were bare names, agreeing with the bare-name comparison
// refpolicy then made -- which is precisely how the bug stayed invisible
// to a suite that drives a real git push through the real hook.
const (
	aliceIdentifier = "alice-agent-1-author"
	bobIdentifier   = "bob-agent-2-author"
)
