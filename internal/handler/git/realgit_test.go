package git

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
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

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
)

// newTestServer wires a *Handler behind withTestIdentity into a real
// httptest.Server -- the same shape production's mux gives this handler
// (identity already resolved into context) minus internal/handler.GitRoleGate,
// which is out of this bead's scope (see ServeHTTP's doc comment).
func newTestServer(t *testing.T, dataDir string, repos RepoStore, identity httpauth.Identity) *httptest.Server {
	t.Helper()
	h := New(dataDir, repos, discardLogger())
	mux := http.NewServeMux()
	mux.Handle("/git/", withTestIdentity(identity, h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// enrolledRepoStore returns a RepoStore that resolves exactly repoName as
// enrolled (with Name set to repoName, mirroring reposstore.Repo's own
// "Name is the natural key" convention) and reports every other name as
// reposstore.ErrNotFound, per docs/git-spec.md "Repo not enrolled -> 404".
func enrolledRepoStore(repoName string) RepoStore {
	return &RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			if name == repoName {
				return reposstore.Repo{Name: repoName}, nil
			}
			return reposstore.Repo{}, reposstore.ErrNotFound
		},
	}
}

// TestEndToEnd_RealCloneAndPushSucceedWithNoHookInstalled is this bead's
// own Definition of Done: "a manual git clone/push against an enrolled
// repo's mirror succeeds end-to-end with no hook installed (proves
// plumbing independent of policy)". The mirror here carries no pre-
// receive hook and no receive.deny* config at all (mirrorreconcile,
// loam-ofg.19's job, never runs in this test) -- if this handler's own
// transport plumbing depended on policy being installed, this test would
// fail; it does not, because upload-pack/receive-pack are stock git with
// nothing hooked in.
func TestEndToEnd_RealCloneAndPushSucceedWithNoHookInstalled(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	workspace := t.TempDir()
	clonePath := filepath.Join(workspace, "clone")
	runGit(t, workspace, "clone", "--quiet", srv.URL+"/git/acme/widgets.git", clonePath)
	content, err := os.ReadFile(filepath.Join(clonePath, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(content), "cloned working tree must contain the seeded commit's content")
	runGit(t, clonePath, "config", "user.email", "pusher@example.com")
	runGit(t, clonePath, "config", "user.name", "pusher")
	require.NoError(t, os.WriteFile(filepath.Join(clonePath, "g.txt"), []byte("world\n"), 0o644))
	runGit(t, clonePath, "add", "g.txt")
	runGit(t, clonePath, "commit", "--quiet", "-m", "second commit")
	pushOut := runGit(t, clonePath, "push", "--quiet", "origin", "HEAD:main")
	t.Logf("git push output: %s", pushOut)
	mirrorHead := runGit(t, "", "--git-dir="+mirrorDir, "log", "-1", "--format=%s", "refs/heads/main")
	assert.Equal(t, "second commit", mirrorHead, "the pushed commit must have landed on the bare mirror's refs/heads/main")
}

// TestEndToEnd_UnenrolledRepo404sForRealGitClone proves docs/git-spec.md's
// "Repo not enrolled -> 404" against an actual `git clone` invocation, not
// just a raw HTTP assertion: real git must fail the clone and surface a
// "not found" style message, never silently succeed.
func TestEndToEnd_UnenrolledRepo404sForRealGitClone(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	workspace := t.TempDir()
	clonePath := filepath.Join(workspace, "clone")
	cmd := exec.CommandContext(t.Context(), "git", "clone", "--quiet", srv.URL+"/git/acme/ghost.git", clonePath)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "cloning an unenrolled repo must fail, not silently succeed: %s", out)
	_, statErr := os.Stat(clonePath)
	assert.True(t, os.IsNotExist(statErr), "no working tree must be created for an unenrolled repo")
}

// TestServeInfoRefs_FramingAndContentType hits GET info/refs directly (not
// through the git binary) so it can assert on the EXACT response bytes
// and headers real git's smart-HTTP protocol requires (git-scm.com/docs/
// http-protocol; docs/git-spec.md names the endpoint and method but does
// not itself spell out the pkt-line framing, MIME types, or Cache-Control
// value): the pkt-line service header immediately followed by a flush,
// then real git's own advertisement, under the "application/x-git-upload-
// pack-advertisement" Content-Type with Cache-Control: no-cache. This is
// the test that would catch "drop the service header" or "omit the
// Content-Type" mutations by assertion rather than a client-side parse
// failure three layers away.
func TestServeInfoRefs_FramingAndContentType(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	infoRefsReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/git/acme/widgets.git/info/refs?service=git-upload-pack", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(infoRefsReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-git-upload-pack-advertisement", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	wantHeader := append(pktLine("# service=git-upload-pack\n"), flushPkt...)
	require.GreaterOrEqual(t, len(body), len(wantHeader))
	assert.Equal(t, wantHeader, body[:len(wantHeader)], "the pkt-line service header + flush must be the exact first bytes of the response")
	assert.Contains(t, string(body[len(wantHeader):]), "refs/heads/main", "real git upload-pack --advertise-refs output must follow the hand-written header")
}

// TestServeRPC_GzipRequestBodyIsDecompressed proves this handler actually
// decompresses a gzip-encoded POST body rather than piping the raw
// compressed bytes into the subprocess's stdin. docs/git-spec.md says
// nothing about gzip at all; this is an empirical property of real git,
// confirmed against git's own remote-curl.c source (loam-ofg.16
// research): gzip is used for upload-pack request bodies over 1024
// bytes. The request body here is a genuine pkt-line
// upload-pack negotiation (want <seeded HEAD sha> ... / flush / done) --
// byte-for-byte the same shape a real git client sends (captured against
// real git 2.50.1 during this bead's own research) -- gzip-compressed and
// posted directly with Content-Encoding: gzip. If decompression were
// skipped, upload-pack would receive raw gzip bytes (which do not parse
// as pkt-lines), fail immediately, and never emit a packfile -- so the
// "PACK" magic asserted below is present if and only if decompression ran.
func TestServeRPC_GzipRequestBodyIsDecompressed(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	headSHA := seedBareMirror(t, mirrorDir)
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	caps := "multi_ack_detailed no-done side-band-64k thin-pack no-progress ofs-delta agent=loam-test"
	var plain bytes.Buffer
	plain.Write(pktLine("want " + headSHA + " " + caps + "\n"))
	plain.Write(flushPkt)
	plain.Write(pktLine("done\n"))
	var gz bytes.Buffer
	gzw := gzip.NewWriter(&gz)
	_, err := gzw.Write(plain.Bytes())
	require.NoError(t, err)
	require.NoError(t, gzw.Close())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/git/acme/widgets.git/git-upload-pack", &gz)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-git-upload-pack-result", resp.Header.Get("Content-Type"))
	assert.Contains(t, string(body), "PACK", "a decompressed, valid upload-pack negotiation must produce a real packfile in the response")
}

// TestServeRPC_InvalidGzipIs400NotAHang proves a Content-Encoding: gzip
// header over a body that is not actually gzip-compressed fails cleanly
// (400) instead of hanging (gzip.NewReader would otherwise block or error
// deep inside the subprocess pipe) or panicking.
func TestServeRPC_InvalidGzipIs400NotAHang(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/git/acme/widgets.git/git-upload-pack", bytes.NewBufferString("not gzip"))
	require.NoError(t, err)
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestServeRPC_ReceivePackPropagatesIdentityToHookEnvironment is the
// CRITICAL SEAM test docs/git-spec.md "Enforcement Mechanics" and this
// bead's own instructions demand: install a trivial pre-receive hook that
// dumps its own environment to a file, push through this handler with a
// resolved identity in context, and assert the four env vars
// (LOAM_AGENT_NAME/_ID/_ROLE, LOAM_REPO) the hook process actually
// observed -- not merely that this handler constructed them, which would
// not prove they survive across the exec.Cmd boundary into a REAL
// subprocess's REAL child (git receive-pack invoking hooks/pre-receive is
// itself a second layer of process inheritance this test exercises for
// real, not by assumption).
func TestServeRPC_ReceivePackPropagatesIdentityToHookEnvironment(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	envDumpPath := filepath.Join(t.TempDir(), "hook-env.txt")
	hookScript := "#!/bin/sh\nenv > " + envDumpPath + "\nexit 0\n"
	hookPath := filepath.Join(mirrorDir, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hookPath, []byte(hookScript), 0o755))
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	workspace := t.TempDir()
	clonePath := filepath.Join(workspace, "clone")
	runGit(t, workspace, "clone", "--quiet", srv.URL+"/git/acme/widgets.git", clonePath)
	runGit(t, clonePath, "config", "user.email", "pusher@example.com")
	runGit(t, clonePath, "config", "user.name", "pusher")
	require.NoError(t, os.WriteFile(filepath.Join(clonePath, "h.txt"), []byte("hook test\n"), 0o644))
	runGit(t, clonePath, "add", "h.txt")
	runGit(t, clonePath, "commit", "--quiet", "-m", "trigger hook")
	runGit(t, clonePath, "push", "--quiet", "origin", "HEAD:main")
	dumped, err := os.ReadFile(envDumpPath)
	require.NoError(t, err, "the pre-receive hook must have run and dumped its environment")
	env := string(dumped)
	assert.Contains(t, env, "LOAM_AGENT_NAME=alice\n")
	assert.Contains(t, env, "LOAM_AGENT_ID=agent-1\n")
	assert.Contains(t, env, "LOAM_AGENT_ROLE=author\n")
	assert.Contains(t, env, "LOAM_REPO=acme/widgets\n")
}

// TestServeRPC_ReceivePackWithNoIdentityFailsClosed proves the CRITICAL
// SEAM's negative case: if receive-pack is somehow reached with no
// resolved caller identity in context (defence in depth -- production
// wiring never allows this, see serveRPC's own doc comment), this handler
// must refuse with 500 rather than silently running `git receive-pack`
// with no LOAM_AGENT_* environment at all, which would make loam-ofg.18's
// hook fail open or misattribute the push.
func TestServeRPC_ReceivePackWithNoIdentityFailsClosed(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	h := New(dataDir, enrolledRepoStore("acme/widgets"), discardLogger())
	req := httptest.NewRequest(http.MethodPost, "/git/acme/widgets.git/git-receive-pack", bytes.NewBufferString(""))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestServeRPC_SubprocessDoesNotOutliveAClientDisconnect_StdinNeverArrived
// proves a client disconnecting before sending any request body does not
// leave `git upload-pack` running forever. It speaks raw TCP rather than
// going through http.Client deliberately: a client-supplied io.Reader
// that never produces a byte (the natural way to make upload-pack block
// waiting for more stdin) blocks entirely INSIDE the Go http.Client's own
// body-upload goroutine, which context cancellation cannot interrupt
// (nothing ever reads or closes that Reader) -- that is a quirk of
// testing through net/http's client abstraction, not a description of a
// real client disconnecting, whose stdin/socket IS torn down at the OS
// level the moment its process dies or its network drops.
//
// What this test actually demonstrates, precisely: upload-pack is
// blocked reading its OWN stdin (it has consumed zero bytes of the
// request), so closing the raw connection makes the server's read of
// r.Body fail, which closes the pipe feeding the child's stdin, which
// upload-pack itself observes as a normal EOF and exits ON ITS OWN --
// this is the ordinary "no request, client gone" case, not proof that
// exec.CommandContext's Kill fired. A mutant that replaces
// exec.CommandContext with exec.Command in gitCommand survives THIS test
// unchanged, exactly because EOF propagation does the work here, not
// cancellation -- TestServeRPC_ContextCancellationAloneKillsAStdinBlockedSubprocess
// below is the test that isolates the cancellation-driven kill instead.
func TestServeRPC_SubprocessDoesNotOutliveAClientDisconnect_StdinNeverArrived(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available on this platform")
	}
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	request := "POST /git/acme/widgets.git/git-upload-pack HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Type: application/x-git-upload-pack-request\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n"
	_, err = conn.Write([]byte(request))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return subprocessRunningFor(t, mirrorDir)
	}, 3*time.Second, 50*time.Millisecond, "git upload-pack subprocess for %s never started", mirrorDir)
	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool {
		return !subprocessRunningFor(t, mirrorDir)
	}, 3*time.Second, 50*time.Millisecond, "git upload-pack subprocess for %s is still running after the client connection closed", mirrorDir)
}

// TestServeRPC_SubprocessDoesNotOutliveAClientDisconnect_WhileProducingOutput
// drives upload-pack past reading its stdin (a genuine, complete, valid
// want/flush/done request, fully sent with a real Content-Length) into
// producing a large packfile response that this test never reads, then
// closes the connection. This is a real and useful property -- the
// subprocess does not outlive a disconnect even mid-response, on a
// different code path than the stdin-blocked test above -- but it is NOT
// a proof that exec.CommandContext's context-driven Kill is what causes
// the exit, and this bead's own probing found the actual mechanism: once
// the broken connection makes cmd.Stdout's Write fail, Go's own exec
// package closes its internal copy goroutine's pipe as cleanup, which
// delivers the child a SIGPIPE on its own next write and kills it via
// that signal's default disposition -- independent of ctx. Replacing
// exec.CommandContext with exec.Command in gitCommand does NOT make this
// test fail, confirmed by running it against that exact mutation, which
// is exactly why this test's name says "does not outlive a disconnect"
// rather than claiming anything about cancellation.
// TestServeRPC_ContextCancellationAloneKillsAStdinBlockedSubprocess below
// is the test that isolates the cancellation-driven kill specifically.
// The seeded mirror carries an 8 MiB incompressible blob so the resulting
// pack exceeds any OS socket/receive-window buffer this test's un-drained
// connection could absorb, guaranteeing the child is genuinely producing
// (and blocked on) output by the time subprocessRunningFor's poll below
// observes it.
func TestServeRPC_SubprocessDoesNotOutliveAClientDisconnect_WhileProducingOutput(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available on this platform")
	}
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	headSHA := seedBareMirrorWithLargeBlob(t, mirrorDir, 8<<20)
	identity := httpauth.Identity{Name: "alice", ID: "agent-1", Role: "author"}
	srv := newTestServer(t, dataDir, enrolledRepoStore("acme/widgets"), identity)
	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	caps := "multi_ack_detailed no-done side-band-64k thin-pack no-progress ofs-delta agent=loam-test"
	var body bytes.Buffer
	body.Write(pktLine("want " + headSHA + " " + caps + "\n"))
	body.Write(flushPkt)
	body.Write(pktLine("done\n"))
	request := fmt.Sprintf("POST /git/acme/widgets.git/git-upload-pack HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Content-Type: application/x-git-upload-pack-request\r\n"+
		"Content-Length: %d\r\n"+
		"\r\n", addr, body.Len())
	_, err = conn.Write([]byte(request))
	require.NoError(t, err)
	_, err = conn.Write(body.Bytes())
	require.NoError(t, err)
	// Deliberately never read the response: upload-pack must block
	// writing its multi-MB pack once the unread connection's buffers
	// fill, which is the write-blocked state this test needs to reach
	// before it closes the connection.
	require.Eventually(t, func() bool {
		return subprocessRunningFor(t, mirrorDir)
	}, 5*time.Second, 50*time.Millisecond, "git upload-pack subprocess for %s never started", mirrorDir)
	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool {
		return !subprocessRunningFor(t, mirrorDir)
	}, 5*time.Second, 50*time.Millisecond, "a write-blocked git upload-pack subprocess for %s is still running after the client connection closed", mirrorDir)
}

// TestServeRPC_ContextCancellationAloneKillsAStdinBlockedSubprocess is the
// mechanism-precise proof the two network-based tests above cannot be:
// both of those, as their own doc comments now say, are actually resolved
// by EOF propagation or a SIGPIPE from Go's own pipe cleanup -- neither
// isolates exec.CommandContext's ctx-driven Kill on its own. This test
// does, by calling Handler.ServeHTTP directly (no network, no real
// client) with an httptest.ResponseRecorder as cmd.Stdout -- whose Write
// never blocks or errors, so no SIGPIPE-style death is possible -- and an
// io.Pipe as the request body that nothing ever writes to, so upload-pack
// blocks reading its own stdin indefinitely with no I/O failure anywhere
// in reach. The only remaining way the OS-level subprocess can die is
// exec.CommandContext's Cancel (Process.Kill) firing when this test's own
// context is canceled directly -- confirmed by running this test against
// a gitCommand mutated to use exec.Command instead of exec.CommandContext:
// the process survives the require.Eventually window below and the test
// fails, unlike the two tests above, which pass unchanged under that same
// mutation.
//
// This test deliberately asserts on the real OS process (via pgrep), NOT
// on ServeHTTP/cmd.Run returning: (*exec.Cmd).Wait also waits for this
// test's own stdin-copying goroutine to finish, which is permanently
// blocked reading the io.Pipe this test never writes to -- a documented
// property of Go's exec package (Cmd.WaitDelay's forced pipe-close only
// reaches the pipe BETWEEN Cmd and the child, never a caller-supplied
// upstream io.Reader that the caller itself never closes), not a defect
// in gitCommand. Cancelling ctx still kills the actual child process
// promptly regardless; ServeHTTP itself only returns once t.Cleanup below
// closes pw, letting the stdin-copy goroutine observe EOF and Wait finish
// -- exactly mirroring how a real disconnect closes the underlying
// connection Stdin is drawn from in production.
func TestServeRPC_ContextCancellationAloneKillsAStdinBlockedSubprocess(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available on this platform")
	}
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	seedBareMirror(t, mirrorDir)
	h := New(dataDir, enrolledRepoStore("acme/widgets"), discardLogger())
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/git/acme/widgets.git/git-upload-pack", pr)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	go h.ServeHTTP(rec, req)
	require.Eventually(t, func() bool {
		return subprocessRunningFor(t, mirrorDir)
	}, 3*time.Second, 50*time.Millisecond, "git upload-pack subprocess for %s never started", mirrorDir)
	cancel()
	require.Eventually(t, func() bool {
		return !subprocessRunningFor(t, mirrorDir)
	}, 3*time.Second, 50*time.Millisecond, "the OS-level git upload-pack process for %s must be killed promptly by context cancellation alone, independent of whether ServeHTTP's own Wait has returned", mirrorDir)
}

// subprocessRunningFor reports whether any process on the machine has
// mirrorDir in its command line, via a real `pgrep -f` -- the only
// reliable, black-box way to prove a subprocess this test never gets a
// handle to has actually exited.
func subprocessRunningFor(t *testing.T, mirrorDir string) bool {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "pgrep", "-f", mirrorDir).CombinedOutput()
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}
