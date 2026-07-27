//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; see
// main_integration_test.go's package doc for how to run this file (same
// package, same build tag -- it reuses newPostgres/waitForReady/
// runningServer from there).
//
// This file is loam-0pj.8's own missing layer: every existing proof is
// either CLI-side with a mocked git seam (internal/cli's
// TestRunCloneCommand_*/TestExecGitCloner_*) or server-side with real git
// but no CLI (internal/handler/git's TestEndToEnd_RealCloneAndPushSucceeds
// WithNoHookInstalled). Nothing before this file has driven the COMPILED
// loam binary's `clone` against a REAL booted loam server and then a real,
// plain `git push` out of the resulting clone -- the exact acceptance
// criterion loam-0pj.8's DESIGN note names: "a direct git push from the
// clone carries identity (verified in the acceptance harness)".
//
// loam-ofg.12 (EnrollRepo, the production path that creates a mirror on
// disk) is not on main yet, so the fixture below hand-seeds an enrolled
// repo row plus a bare mirror directly, matching this bead's own
// instructions for what is legitimate to stand in for it. See
// seedEnrolledRepoWithMirror's doc comment for exactly what that means --
// and does not mean -- for what this file proves.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorpath"
)

// loamBinaryOnce/loamBinaryPath/loamBinaryErr let every test in this file
// share one compiled cmd/loam binary, built lazily on first use rather
// than in this package's own TestMain (main_integration_test.go already
// defines TestMain to build the server binary, and a package may only
// define one). sync.Once is the standard, minimal-footprint way to share
// that one-time build across tests without introducing package-level
// mutable state anywhere outside this guarded pair.
var (
	loamBinaryOnce sync.Once
	loamBinaryPath string
	loamBinaryErr  error
)

// buildLoamBinary compiles cmd/loam once for the whole test process and
// returns its path, failing t immediately if the build itself failed.
// Building by full module path (not a relative "../loam") makes this
// independent of the test binary's working directory.
func buildLoamBinary(t *testing.T) string {
	t.Helper()
	loamBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "loam-cli-e2e-*")
		if err != nil {
			loamBinaryErr = fmt.Errorf("creating temp dir for loam binary: %w", err)
			return
		}
		loamBinaryPath = filepath.Join(dir, "loam")
		build := exec.Command("go", "build", "-o", loamBinaryPath, "github.com/bobcob7/loam/cmd/loam")
		if out, buildErr := build.CombinedOutput(); buildErr != nil {
			loamBinaryErr = fmt.Errorf("building loam binary: %w: %s", buildErr, out)
		}
	})
	require.NoError(t, loamBinaryErr)
	return loamBinaryPath
}

// loamCLIResult is one `loam` invocation's observable outcome: the real
// process exit code and real stdout/stderr, mirroring cmd/loam/
// main_test.go's own loamResult -- this is a different package, so that
// type is not reachable here.
type loamCLIResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runLoamCLI runs the compiled loam binary with dir as its working
// directory (so `loam clone`'s "./<repo_name>" destination lands there)
// and env as its complete environment, capturing the real exit code.
func runLoamCLI(t *testing.T, dir string, env []string, args ...string) loamCLIResult {
	t.Helper()
	cmd := exec.Command(buildLoamBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		require.True(t, ok, "loam exited abnormally rather than with a normal exit code: %v (stderr: %s)", runErr, stderr.String())
		exitCode = exitErr.ExitCode()
	}
	return loamCLIResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

// loamAgentEnv builds a complete, valid LOAM_* CLI environment (see
// docs/cli-spec.md -> Environment Variables) pointed at serverURL, acting
// as agentName/agentID/agentRole. PATH is preserved so the CLI's own git
// subprocess invocations resolve.
func loamAgentEnv(serverURL, agentName, agentID, agentRole string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_SERVER_URL=" + serverURL,
		"LOAM_AGENT_NAME=" + agentName,
		"LOAM_AGENT_ID=" + agentID,
		"LOAM_AGENT_ROLE=" + agentRole,
	}
}

// startServerWithDataDir mirrors main_integration_test.go's startServer
// exactly, except it takes dataDir as a parameter and hands it back to the
// caller, instead of generating an unreachable t.TempDir() internally --
// this file's fixture needs to create a bare mirror at
// mirrorpath.Dir(dataDir, repoName) itself, so it must know the path the
// running server was actually given. A second copy (rather than changing
// startServer's own signature) keeps this file purely additive: it never
// touches main_integration_test.go, which is out of scope for this bead
// and a file other concurrent work in this tree does not claim either, but
// there is no reason to risk a merge collision over a one-parameter
// signature change to shared test infrastructure.
func startServerWithDataDir(t *testing.T, databaseURL, dataDir string) *runningServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	listenerFile, err := listener.(*net.TCPListener).File()
	require.NoError(t, err)
	require.NoError(t, listener.Close()) // listenerFile holds its own dup; the port stays bound
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_HTTP_ADDR=" + addr,
		"LOAM_LISTENER_FD=3",
		"LOAM_ADMIN_USER=" + testAdminUser,
		"LOAM_ADMIN_PASSWORD=" + testAdminPassword,
		"LOAM_DATABASE_URL=" + databaseURL,
		"LOAM_ENCRYPTION_KEY=" + testEncryptionKey,
		"LOAM_DATA_DIR=" + dataDir,
	}
	cmd := exec.Command(serverBinary)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{listenerFile}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	require.NoError(t, listenerFile.Close()) // the child has its own dup from ExtraFiles
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitForReady(t, addr, &stderr)
	return &runningServer{cmd: cmd, addr: addr, stderr: &stderr}
}

// seedEnrolledRepoWithMirror hand-seeds the fixture loam-ofg.12's not-yet-
// landed EnrollRepo would otherwise produce: a repos row (direct SQL
// insert against the already-migrated database) plus a real bare mirror on
// disk at mirrorpath.Dir(dataDir, repoName), seeded with one commit on
// branch via a throwaway working tree, exactly seedBareMirror does in
// internal/handler/git/testhelpers_test.go (not importable across
// packages, so reproduced here).
//
// Deliberately NOT called: mirrorreconcile.ReconcileMirror. Calling it (or
// letting main.go's Startup step 3 reconcile a mirror that already exists
// when the server boots) installs internal/mirrorreconcile's current
// pre-receive hook stub, which unconditionally exits 1 pending loam-ofg.18
// (verified directly against a throwaway mirror while investigating this
// bead: `git push` against a reconciled mirror fails with "remote: loam:
// pre-receive policy socket not yet implemented (loam-ofg.18)"). Seeding
// this repo AFTER the server has already finished booting (see the tests
// below: startServerWithDataDir runs first, against an empty repos table)
// means main.go's one-time startup reconciliation loop never reaches this
// mirror, leaving it hook-free -- the same "policy is orthogonal to
// transport" state internal/handler/git's own
// TestEndToEnd_RealCloneAndPushSucceedsWithNoHookInstalled deliberately
// tests. This is real, on-disk git plumbing; what is NOT proven is that a
// push against a mirror loam-ofg.12's real EnrollRepo has reconciled (or
// that any server restart's Startup step 3 has reconciled) still succeeds
// -- that is blocked on loam-ofg.18 landing, not on this bead.
func seedEnrolledRepoWithMirror(t *testing.T, dsn, dataDir, repoName, branch string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	id := uuid.Must(uuid.NewV7())
	_, err = conn.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, $3, $4, $5)`,
		id, repoName, "https://example.invalid/"+repoName, "example.invalid", branch,
	)
	require.NoError(t, err)
	mirrorDir := mirrorpath.Dir(dataDir, repoName)
	seedThrowawayBareMirror(t, mirrorDir, branch)
}

// seedThrowawayBareMirror creates a real bare mirror at mirrorDir (parents
// included) seeded with a single commit on branch adding seed.txt="hello\n",
// by committing into a throwaway working tree and bare-cloning it --
// reproduced from internal/handler/git/testhelpers_test.go's
// seedBareMirror (not importable across packages: that file is
// package-private test code in a different package), parametrized on
// branch since that bead's fixture always used "main".
func seedThrowawayBareMirror(t *testing.T, mirrorDir, branch string) {
	t.Helper()
	src := t.TempDir()
	runGitCmd(t, src, "init", "--quiet", "--initial-branch="+branch)
	runGitCmd(t, src, "config", "user.email", "seed@example.com")
	runGitCmd(t, src, "config", "user.name", "seed")
	require.NoError(t, os.WriteFile(filepath.Join(src, "seed.txt"), []byte("hello\n"), 0o644))
	runGitCmd(t, src, "add", "seed.txt")
	runGitCmd(t, src, "commit", "--quiet", "-m", "init")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGitCmd(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
}

// runGitCmd runs a real git subcommand in dir (empty means the test
// process's own cwd), failing t immediately on a nonzero exit.
func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// mirrorLogSubject returns the subject line of the most recent commit on
// ref inside the bare mirror at mirrorDir, addressed via --git-dir (never
// -C, for the same upward-repository-discovery reason
// internal/mirrorreconcile documents).
func mirrorLogSubject(t *testing.T, mirrorDir, ref string) string {
	t.Helper()
	return runGitCmd(t, "", "--git-dir="+mirrorDir, "log", "-1", "--format=%s", ref)
}

// TestClonePush_CompiledCLI_RealServer_ClonesAndPushesPlainGit is this
// bead's central, previously-missing proof: the COMPILED loam binary's
// `clone` against a REAL booted loam server (real Postgres, real HTTP
// listener, real /git/* smart-HTTP transport), followed by a real, plain
// `git commit` + `git push` out of the resulting clone with NO loam
// involvement in the push -- the exact design point of "bootstraps plain
// git" (docs/git-spec.md "The CLI's Role"). It also proves the clone is
// single-branch and identity-configured, per this bead's acceptance
// criteria.
func TestClonePush_CompiledCLI_RealServer_ClonesAndPushesPlainGit(t *testing.T) {
	dsn := newPostgres(t)
	// shortDataDir, not t.TempDir(): the policy socket binds
	// <LOAM_DATA_DIR>/hook.sock, and a t.TempDir() path (which embeds the
	// test name) overruns unix sockets' ~104-byte sun_path limit, so the
	// server exits before this file's readiness poll can ever succeed.
	dataDir := shortDataDir(t)
	rs := startServerWithDataDir(t, dsn, dataDir)
	const repoName = "acme/widgets"
	const branch = "feature-x"
	seedEnrolledRepoWithMirror(t, dsn, dataDir, repoName, branch)
	workspace := t.TempDir()
	env := loamAgentEnv("http://"+rs.addr, "ada-lovelace", "7", "author")
	result := runLoamCLI(t, workspace, env, "clone", repoName, branch)
	require.Equal(t, 0, result.exitCode, "stdout: %s\nstderr: %s", result.stdout, result.stderr)
	assert.Contains(t, result.stdout, `"repo":"acme/widgets"`)
	assert.Contains(t, result.stdout, `"branch":"feature-x"`)
	clonePath := filepath.Join(workspace, "widgets")
	assertSingleBranchClone(t, clonePath, branch)
	assertIdentityHeadersConfigured(t, clonePath, "ada-lovelace", "7", "author")
	pushOut := plainGitCommitAndPush(t, clonePath, "agent.txt", "agent change", "second commit")
	t.Logf("plain git push output: %s", pushOut)
	mirrorDir := mirrorpath.Dir(dataDir, repoName)
	assert.Equal(t, "second commit", mirrorLogSubject(t, mirrorDir, "refs/heads/"+branch),
		"the plain `git push` (no loam involvement) must have landed the commit on the mirror's branch ref")
}

// assertSingleBranchClone proves `loam clone`'s --single-branch shape: the
// clone has exactly one local branch (branch), and remote.origin.fetch is
// narrowed to that one branch's refspec rather than the default
// "all branches" one a plain `git clone` without --single-branch would
// carry.
func assertSingleBranchClone(t *testing.T, clonePath, branch string) {
	t.Helper()
	localBranches := runGitCmd(t, clonePath, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	assert.Equal(t, branch, localBranches, "a single-branch clone must have exactly one local branch: the requested one")
	fetchRefspec := runGitCmd(t, clonePath, "config", "--get", "remote.origin.fetch")
	assert.Equal(t, fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch), fetchRefspec,
		"single-branch clones narrow remote.origin.fetch to the one requested branch")
}

// assertIdentityHeadersConfigured proves the clone's own git config carries
// the three Loam-Agent-* http.extraHeader entries `loam clone` promises
// (docs/cli-spec.md -> clone), persisted from the clone-time --config args
// clone.go's Clone method passes (see identityHeaders' doc comment for why
// they must be set at clone time, not afterward).
func assertIdentityHeadersConfigured(t *testing.T, clonePath, agentName, agentID, agentRole string) {
	t.Helper()
	headers := runGitCmd(t, clonePath, "config", "--get-all", "http.extraheader")
	assert.Contains(t, headers, "Loam-Agent-Name: "+agentName)
	assert.Contains(t, headers, "Loam-Agent-Id: "+agentID)
	assert.Contains(t, headers, "Loam-Agent-Role: "+agentRole)
	assert.Equal(t, agentName, runGitCmd(t, clonePath, "config", "--get", "user.name"))
}

// plainGitCommitAndPush writes filename with content into clonePath,
// commits it with message, and pushes HEAD to the clone's sole remote --
// stock `git`, exactly as an agent's own shell would run it, with no
// `loam` command anywhere in this function.
func plainGitCommitAndPush(t *testing.T, clonePath, filename, content, message string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(clonePath, filename), []byte(content+"\n"), 0o644))
	runGitCmd(t, clonePath, "add", filename)
	runGitCmd(t, clonePath, "commit", "--quiet", "-m", message)
	return runGitCmd(t, clonePath, "push", "--quiet", "origin", "HEAD")
}

// TestClonePush_UnenrolledRepo_ExitsThreeDistinctFromServiceNotRegistered
// is this bead's exit-3 acceptance criterion, run through the COMPILED
// binary against a REAL, freshly migrated (nothing enrolled) server --
// and, per this session's brief, asserts on the MESSAGE, not just the exit
// code: loam-ofg.11's trap is that a genuinely unenrolled repo and a
// literally unregistered RepoService handler both answer CodeNotFound (exit
// 3 either way), so the code alone cannot discriminate "this repo isn't
// enrolled" from "the server's own RepoService wiring is broken" --
// cmd/server/registration_integration_test.go's
// TestServer_RepoServiceIsRegistered_NotGroupFallback proves the raw
// Connect layer distinguishes them; this test proves the SAME thing is
// still true one layer up, through `loam clone`'s own exit-3 message.
func TestClonePush_UnenrolledRepo_ExitsThreeDistinctFromServiceNotRegistered(t *testing.T) {
	dsn := newPostgres(t)
	// shortDataDir, not t.TempDir(): the policy socket binds
	// <LOAM_DATA_DIR>/hook.sock, and a t.TempDir() path (which embeds the
	// test name) overruns unix sockets' ~104-byte sun_path limit, so the
	// server exits before this file's readiness poll can ever succeed.
	dataDir := shortDataDir(t)
	rs := startServerWithDataDir(t, dsn, dataDir)
	workspace := t.TempDir()
	env := loamAgentEnv("http://"+rs.addr, "ada-lovelace", "7", "author")
	result := runLoamCLI(t, workspace, env, "clone", "bobcob7/never-enrolled", "main")
	assert.Equal(t, 3, result.exitCode, "stdout: %s\nstderr: %s", result.stdout, result.stderr)
	assert.Contains(t, result.stdout, "bobcob7/never-enrolled",
		"the real RepoServiceHandler's not-found message names the requested repo; a group-level 'service not registered' fallback never would")
	assert.NotContains(t, result.stdout, "service registered",
		"a genuinely unenrolled repo must be distinguishable from loam-ofg.11's 'no /loam.v1.* service registered' fallback trap, not just share its exit code")
}
