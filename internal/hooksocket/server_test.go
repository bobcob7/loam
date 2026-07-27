package hooksocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// shortTempDir returns a fresh, short-named temp directory, registered for
// removal via t.Cleanup. Unlike t.TempDir() -- whose path embeds the full
// (often long, subtest-qualified) test name -- this stays well under the
// ~104-byte sun_path limit unix domain sockets are subject to on macOS/BSD,
// which a socket file path built from t.TempDir() in this package's own
// tests was observed to exceed ("bind: invalid argument").
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hs")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startTestServer binds a *Server at a fresh, short socket path and runs
// it for the duration of the test, shutting it down (and waiting for Run
// to return) via t.Cleanup.
func startTestServer(t *testing.T, store WorkBranchStore) string {
	t.Helper()
	return startTestServerWithDeadline(t, store, defaultConnDeadline)
}

func startTestServerWithDeadline(t *testing.T, store WorkBranchStore, connDeadline time.Duration) string {
	t.Helper()
	socketPath := filepath.Join(shortTempDir(t), "hook.sock")
	srv, err := listen(socketPath, store, nil, testLogger(), connDeadline)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("policy socket server did not shut down within 5s of cancellation")
		}
	})
	return socketPath
}

// registeredBranchStore builds a WorkBranchStoreMock resolving exactly one
// (repoName, branchName) pair, matching refpolicy's own test fixture
// shape.
func registeredBranchStore(repoName, branchName string, wb workbranchstore.WorkBranch) *WorkBranchStoreMock {
	return &WorkBranchStoreMock{
		GetWorkBranchFunc: func(_ context.Context, gotRepo, gotBranch string) (workbranchstore.WorkBranch, error) {
			if gotRepo == repoName && gotBranch == branchName {
				return wb, nil
			}
			return workbranchstore.WorkBranch{}, fmt.Errorf("branch %s/%s: %w", gotRepo, gotBranch, workbranchstore.ErrNotFound)
		},
	}
}

// TestListen_SucceedsUnderAPathTooLongForADirectBind is the regression
// test for a real failure this package caused once wired into
// cmd/server's Startup: a LOAM_DATA_DIR long enough that
// "<dataDir>/hook.sock" exceeds unix domain sockets' sun_path limit
// (~104 bytes on macOS/BSD) made net.Listen fail with "bind: invalid
// argument" -- observed for real via cmd/server/main_integration_test.go's
// t.TempDir()-based LOAM_DATA_DIR, which nests the full test name into the
// path. bindUnixSocket's chdir-and-retry fallback must make this succeed
// regardless of path length.
func TestListen_SucceedsUnderAPathTooLongForADirectBind(t *testing.T) {
	t.Parallel()
	base := shortTempDir(t)
	longSubdir := filepath.Join(base, "TestServer_Healthz_ReachableWithAndWithoutAuthorizationHeaderVeryLongTestName1234567890", "001")
	require.NoError(t, os.MkdirAll(longSubdir, 0o755))
	socketPath := filepath.Join(longSubdir, "hook.sock")
	require.Greater(t, len(socketPath), 104, "this test's own fixture must actually exceed the sun_path limit it is proving a workaround for")
	srv, err := Listen(socketPath, &WorkBranchStoreMock{}, nil, testLogger())
	require.NoError(t, err, "Listen must succeed even when the absolute socket path is too long for a direct bind")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

// TestServer_AllowedPush_RoundTrip proves a genuinely allowed push travels
// the FULL real wire: a real unix socket, real JSON encode/decode on both
// sides, real refpolicy.EvaluatePush underneath.
func TestServer_AllowedPush_RoundTrip(t *testing.T) {
	t.Parallel()
	store := registeredBranchStore("acme/widgets", "wb-good", workbranchstore.WorkBranch{
		Name: "wb-good", Author: "alice", State: workbranchstore.StateDraft,
	})
	socketPath := startTestServer(t, store)
	req := Request{
		Repo:  "acme/widgets",
		Agent: AgentIdentity{Name: "alice", ID: "agent-1", Role: "author"},
		Updates: []RefUpdateWire{
			{OldSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ref: "refs/heads/wb-good"},
		},
	}
	resp, err := Call(socketPath, req, DialTimeout, RPCTimeout)
	require.NoError(t, err)
	assert.True(t, resp.Accepted)
	require.Len(t, resp.Verdicts, 1)
	assert.True(t, resp.Verdicts[0].Allowed)
	assert.Empty(t, resp.Verdicts[0].Reason)
}

// TestServer_RejectedPush_ReasonSurfacesOverTheWire proves a rejected
// ref's exact loam:-prefixed reason string crosses the wire unmodified.
func TestServer_RejectedPush_ReasonSurfacesOverTheWire(t *testing.T) {
	t.Parallel()
	store := registeredBranchStore("acme/widgets", "wb-owned", workbranchstore.WorkBranch{
		Name: "wb-owned", Author: "grace-hopper-3-author", State: workbranchstore.StateDraft,
	})
	socketPath := startTestServer(t, store)
	req := Request{
		Repo:  "acme/widgets",
		Agent: AgentIdentity{Name: "alice", ID: "agent-1", Role: "author"},
		Updates: []RefUpdateWire{
			{OldSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ref: "refs/heads/wb-owned"},
		},
	}
	resp, err := Call(socketPath, req, DialTimeout, RPCTimeout)
	require.NoError(t, err)
	assert.False(t, resp.Accepted)
	require.Len(t, resp.Verdicts, 1)
	assert.False(t, resp.Verdicts[0].Allowed)
	assert.Equal(t, "loam: wb-owned belongs to grace-hopper-3-author", resp.Verdicts[0].Reason)
}

// TestServer_MixedPush_WholeResponseRejectedOverTheWire proves atomicity
// survives the wire: a push with one good ref and one bad ref must come
// back Accepted: false even though the good ref's own VerdictWire.Allowed
// is still true.
func TestServer_MixedPush_WholeResponseRejectedOverTheWire(t *testing.T) {
	t.Parallel()
	store := registeredBranchStore("acme/widgets", "wb-good", workbranchstore.WorkBranch{
		Name: "wb-good", Author: "alice", State: workbranchstore.StateDraft,
	})
	socketPath := startTestServer(t, store)
	req := Request{
		Repo:  "acme/widgets",
		Agent: AgentIdentity{Name: "alice", ID: "agent-1", Role: "author"},
		Updates: []RefUpdateWire{
			{OldSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ref: "refs/heads/wb-good"},
			{OldSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ref: "refs/heads/main"},
		},
	}
	resp, err := Call(socketPath, req, DialTimeout, RPCTimeout)
	require.NoError(t, err)
	assert.False(t, resp.Accepted, "one bad ref must reject the whole push over the wire")
	require.Len(t, resp.Verdicts, 2)
	assert.True(t, resp.Verdicts[0].Allowed)
	assert.False(t, resp.Verdicts[1].Allowed)
}

// TestServer_StoreError_FailsClosedOverTheWire proves a Postgres-layer
// failure never turns into an accepted push at the wire level.
func TestServer_StoreError_FailsClosedOverTheWire(t *testing.T) {
	t.Parallel()
	store := &WorkBranchStoreMock{
		GetWorkBranchFunc: func(context.Context, string, string) (workbranchstore.WorkBranch, error) {
			return workbranchstore.WorkBranch{}, errors.New("connection refused")
		},
	}
	socketPath := startTestServer(t, store)
	req := Request{
		Repo:    "acme/widgets",
		Agent:   AgentIdentity{Name: "alice"},
		Updates: []RefUpdateWire{{OldSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ref: "refs/heads/wb-good"}},
	}
	resp, err := Call(socketPath, req, DialTimeout, RPCTimeout)
	require.NoError(t, err, "the SOCKET round trip itself succeeds; it is the push that must be reported unaccepted")
	assert.False(t, resp.Accepted, "a store error must fail the push closed, never accept it")
	assert.Empty(t, resp.Verdicts)
}

// TestServer_MalformedRequestDoesNotCrashOrWedgeTheServer proves garbage
// bytes on the wire (not valid JSON at all) close that one connection
// without a response, and critically do not take down the server for
// later, well-formed connections -- proving the accept loop survives a
// single bad actor.
func TestServer_MalformedRequestDoesNotCrashOrWedgeTheServer(t *testing.T) {
	t.Parallel()
	store := registeredBranchStore("acme/widgets", "wb-good", workbranchstore.WorkBranch{
		Name: "wb-good", Author: "alice", State: workbranchstore.StateDraft,
	})
	socketPath := startTestServer(t, store)
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	_, writeErr := conn.Write([]byte("this is not json at all {{{"))
	require.NoError(t, writeErr)
	require.NoError(t, conn.Close())

	// The server must still answer a well-formed follow-up request.
	req := Request{
		Repo:    "acme/widgets",
		Agent:   AgentIdentity{Name: "alice"},
		Updates: []RefUpdateWire{{OldSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ref: "refs/heads/wb-good"}},
	}
	resp, err := Call(socketPath, req, DialTimeout, RPCTimeout)
	require.NoError(t, err)
	assert.True(t, resp.Accepted)
}

// TestServer_StalledClientIsClosedByServerSideDeadline proves a client
// that connects and never sends anything does not tie up a server
// goroutine forever: using a short connDeadline (rather than waiting out
// defaultConnDeadline's real production duration), the server must close
// the connection on its own once the deadline elapses.
func TestServer_StalledClientIsClosedByServerSideDeadline(t *testing.T) {
	t.Parallel()
	store := registeredBranchStore("acme/widgets", "wb-good", workbranchstore.WorkBranch{
		Name: "wb-good", Author: "alice", State: workbranchstore.StateDraft,
	})
	socketPath := startTestServerWithDeadline(t, store, 200*time.Millisecond)
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1)
	start := time.Now()
	_, readErr := conn.Read(buf)
	elapsed := time.Since(start)
	assert.Error(t, readErr, "the server must close a stalled connection rather than hold it open forever")
	assert.Less(t, elapsed, 3*time.Second, "the server-side deadline must actually bound the wait, not merely exist unused")
}

// TestCall_NoListenerFailsClosedQuickly proves dialing a socket path
// nothing is listening on fails immediately, never hangs -- the "socket
// unreachable" half of docs/git-spec.md's fail-closed contract.
func TestCall_NoListenerFailsClosedQuickly(t *testing.T) {
	t.Parallel()
	socketPath := filepath.Join(shortTempDir(t), "no-such-socket.sock")
	start := time.Now()
	_, err := Call(socketPath, Request{}, 500*time.Millisecond, 500*time.Millisecond)
	elapsed := time.Since(start)
	assert.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second)
}

// TestCall_RPCTimeoutFiresWhenServerNeverResponds proves the read
// deadline Call sets is real: a bare listener that accepts a connection
// and then never writes anything back must still cause Call to return an
// error within (roughly) rpcTimeout, not hang forever. This is the exact
// "make the socket timeout not fire" mutation this bead's own
// instructions call out -- a Call that forgot conn.SetDeadline would pass
// every other test in this file but hang here indefinitely.
func TestCall_RPCTimeoutFiresWhenServerNeverResponds(t *testing.T) {
	t.Parallel()
	socketPath := filepath.Join(shortTempDir(t), "silent.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		// Deliberately never read or write anything; hold the connection
		// open until the test's own Call gives up.
		<-t.Context().Done()
		_ = conn.Close()
	}()
	start := time.Now()
	_, callErr := Call(socketPath, Request{}, DialTimeout, 300*time.Millisecond)
	elapsed := time.Since(start)
	assert.Error(t, callErr, "a server that never responds must fail Call closed, not hang")
	assert.Less(t, elapsed, 2*time.Second, "the rpcTimeout deadline must actually bound the wait")
}
