package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAdminUser     = "admin"
	testAdminPassword = "s3cret-pass"
	// testEncryptionKey is a base64-encoded, arbitrary-but-fixed 32 random
	// bytes -- config.Load only validates shape (decodes to exactly 32
	// bytes), never uses the key itself, since this binary never wires an
	// encryptor (loam-ofg.2's scope stops at the listener).
	testEncryptionKey = "nMjGBpIoO1n40SGBc7WQEnT/FHff/dpDkHu5cB527fg="
	// testDatabaseURL only needs to parse as a postgres DSN -- config.Load
	// never connects, and this binary never builds a pool (that ordering
	// is loam-ofg.21's, see main.go's package doc), so no real Postgres is
	// required to run these tests.
	testDatabaseURL = "postgres://user:pass@localhost:5432/loam"
)

// serverBinary is the path to the compiled server binary, built once for
// the whole test process by TestMain. Mirrors cmd/loam/main_test.go's
// loamBinary: package-level mutable state is against this repo's Go
// standards for production code, but there is no clean alternative for
// sharing one compiled binary across every test in this file.
var serverBinary string

// TestMain compiles cmd/server once before any test in this file runs.
// These tests drive the COMPILED BINARY -- real process, real listening
// socket, real signals -- because a unit test calling run()/serve() in
// process cannot observe the actual exit code or whether the OS-level
// listener genuinely closed, which is exactly what the graceful-shutdown
// acceptance criteria are about.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loam-server-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	serverBinary = filepath.Join(dir, "server")
	build := exec.Command("go", "build", "-o", serverBinary, ".")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "building server binary: %v\n%s", buildErr, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// runningServer is one started, listening instance of the compiled binary.
type runningServer struct {
	cmd    *exec.Cmd
	addr   string
	stderr *bytes.Buffer
}

// freeAddr reserves an ephemeral TCP port by binding then immediately
// closing it, so each test gets its own address instead of racing on a
// fixed one. There is an unavoidable, small window between this Close and
// the server binary's own Listen -- the same tradeoff every "find a free
// port for a subprocess" helper makes.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// startServer launches the compiled binary with a valid, self-contained
// environment (no real Postgres required -- see testDatabaseURL) and
// blocks until it is accepting TCP connections. t.Cleanup kills it if the
// test itself did not already stop it.
func startServer(t *testing.T) *runningServer {
	t.Helper()
	addr := freeAddr(t)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_HTTP_ADDR=" + addr,
		"LOAM_ADMIN_USER=" + testAdminUser,
		"LOAM_ADMIN_PASSWORD=" + testAdminPassword,
		"LOAM_DATABASE_URL=" + testDatabaseURL,
		"LOAM_ENCRYPTION_KEY=" + testEncryptionKey,
		"LOAM_DATA_DIR=" + t.TempDir(),
	}
	cmd := exec.Command(serverBinary)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitForListening(t, addr)
	return &runningServer{cmd: cmd, addr: addr, stderr: &stderr}
}

// waitForListening polls addr until it accepts a TCP connection or the
// deadline passes. Polling an OS-level socket is the honest way to observe
// "has this real subprocess finished starting up" -- there is no tick or
// hook to drive here, unlike the sync scheduler's deterministic-time
// harness (docs/testing-spec.md).
func waitForListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not start listening on %s in time", addr)
}

// waitExit waits up to timeout for cmd to exit, failing the test (and
// force-killing it) rather than hanging forever if it does not.
func waitExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err, "server exited abnormally")
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("server did not exit within %s of the shutdown signal", timeout)
	}
}

// getWithAuthorization issues a GET to path against addr, setting the raw
// Authorization header value verbatim when non-empty, and returns the
// response body and status code.
func getWithAuthorization(t *testing.T, addr, path, authorization string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, err)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body), resp.StatusCode
}

// TestServer_Healthz_ReachableWithAndWithoutAuthorizationHeader proves
// against the real compiled binary -- not just the in-process mux tests in
// internal/server -- that /healthz is reachable with no Authorization
// header and with a garbage one, returning byte-identical responses either
// way (docs/server-spec.md -> Health: "the only such exemption").
func TestServer_Healthz_ReachableWithAndWithoutAuthorizationHeader(t *testing.T) {
	t.Parallel()
	rs := startServer(t)
	noAuthBody, noAuthStatus := getWithAuthorization(t, rs.addr, "/healthz", "")
	garbageAuthBody, garbageAuthStatus := getWithAuthorization(t, rs.addr, "/healthz", "Bogus not-a-real-scheme")
	assert.Equal(t, http.StatusOK, noAuthStatus)
	assert.Equal(t, http.StatusOK, garbageAuthStatus)
	assert.Equal(t, noAuthBody, garbageAuthBody)
}

// TestServer_Root_ValidAdminAuth_ServesEmbeddedIndex proves the embedded
// SPA (web.Dist, wired in via RegisterSPA in buildRouter) is actually
// reachable through the real binary and serves the real embedded
// index.html content -- the only coverage web.Dist() has anywhere, since
// internal/server's own tests exercise RegisterSPA against an in-memory
// fstest.MapFS, never the production embed.
func TestServer_Root_ValidAdminAuth_ServesEmbeddedIndex(t *testing.T) {
	t.Parallel()
	rs := startServer(t)
	req, err := http.NewRequest(http.MethodGet, "http://"+rs.addr+"/", nil)
	require.NoError(t, err)
	req.SetBasicAuth(testAdminUser, testAdminPassword)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "Loam admin interface")
	assert.Contains(t, string(body), "loam-placeholder-spa")
}

// TestServer_Root_NoCredentials_Returns401WithChallenge proves the static/
// SPA path group is really behind AdminOnly in the real binary: no
// credentials gets 401 plus the WWW-Authenticate challenge that makes a
// browser prompt.
func TestServer_Root_NoCredentials_Returns401WithChallenge(t *testing.T) {
	t.Parallel()
	rs := startServer(t)
	resp, err := http.Get("http://" + rs.addr + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, `Basic realm="loam"`, resp.Header.Get("WWW-Authenticate"))
}

// testGracefulShutdown signals the running server with sig and proves it
// exits 0 within a bounded grace period AND that the listening socket is
// actually closed afterward -- not just that the process died, which a
// crash would also produce.
func testGracefulShutdown(t *testing.T, sig syscall.Signal) {
	t.Helper()
	rs := startServer(t)
	require.NoError(t, rs.cmd.Process.Signal(sig))
	waitExit(t, rs.cmd, 10*time.Second)
	require.NotNil(t, rs.cmd.ProcessState, "stderr: %s", rs.stderr.String())
	assert.Equal(t, 0, rs.cmd.ProcessState.ExitCode(), "stderr: %s", rs.stderr.String())
	_, dialErr := net.DialTimeout("tcp", rs.addr, 200*time.Millisecond)
	assert.Error(t, dialErr, "listener should be closed after graceful shutdown")
}

// TestServer_SIGINT_ExitsCleanlyAndClosesListener is the bead's own
// acceptance line ("graceful shutdown on SIGINT") against the real binary.
func TestServer_SIGINT_ExitsCleanlyAndClosesListener(t *testing.T) {
	t.Parallel()
	testGracefulShutdown(t, syscall.SIGINT)
}

// TestServer_SIGTERM_ExitsCleanlyAndClosesListener mirrors the SIGINT case
// for SIGTERM (docs/server-spec.md -> Shutdown: "On SIGTERM: stop
// accepting ... connections"), the signal a container orchestrator sends.
func TestServer_SIGTERM_ExitsCleanlyAndClosesListener(t *testing.T) {
	t.Parallel()
	testGracefulShutdown(t, syscall.SIGTERM)
}
