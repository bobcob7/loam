//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and is
// excluded from the default `go test ./...` run by the integration build
// tag, so CI stays green without one. Run explicitly with:
//
//	go test -tags=integration ./cmd/server/... -v
//
// On podman (e.g. a `podman machine` forwarding /var/run/docker.sock), also
// set TESTCONTAINERS_RYUK_DISABLED=true (see internal/db/migrations/
// integration_test.go's package doc for why).
//
// These tests drive the COMPILED BINARY -- real process, real listening
// socket, real signals, real Postgres via testcontainers-go -- because a
// unit test calling run()/serve() in process cannot observe the actual exit
// code, whether the OS-level listener genuinely closed, or whether
// migrations actually ran against a real database, which is exactly what
// this bead's acceptance criteria are about. Fast, fake/spy-based coverage
// of serve's order and drain logic lives in serve_test.go and
// database_test.go, which need no container at all.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/testdb"
)

const (
	testAdminUser     = "admin"
	testAdminPassword = "s3cret-pass"
	// testEncryptionKey is a base64-encoded, arbitrary-but-fixed 32 random
	// bytes -- config.Load only validates shape (decodes to exactly 32
	// bytes), never uses the key itself, since this binary never wires an
	// encryptor (loam-ofg.2's scope stops at the listener).
	testEncryptionKey = "nMjGBpIoO1n40SGBc7WQEnT/FHff/dpDkHu5cB527fg="
)

// serverBinary is the path to the compiled server binary, built once for
// the whole test process by TestMain. Mirrors cmd/loam/main_test.go's
// loamBinary: package-level mutable state is against this repo's Go
// standards for production code, but there is no clean alternative for
// sharing one compiled binary across every test in this file.
var serverBinary string

// TestMain compiles cmd/server, AND cmd/loamhook as its sibling in the
// same directory, once before any test in this file runs. The loamhook
// sibling is required for real: main.go's loamhookBinaryPath resolves the
// hook binary as a sibling of the running server executable, and
// (loam-ofg.18's review) that resolution is now a hard startup error, not
// merely a per-repo one -- so every test in this file needs a genuine
// "loamhook" binary sitting next to "server" to start at all, exactly as
// a real deployment must (loam-mce tracks the still-missing Taskfile/
// Dockerfile step that would build+place both binaries outside tests).
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
	hookBinary := filepath.Join(dir, "loamhook")
	hookBuild := exec.Command("go", "build", "-o", hookBinary, "github.com/bobcob7/loam/cmd/loamhook")
	if out, buildErr := hookBuild.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "building loamhook binary: %v\n%s", buildErr, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// newPostgres starts one pgvector-enabled Postgres container and returns
// its DSN, registering cleanup that terminates it. Every test in this file
// calls this itself (rather than sharing one container via TestMain) and
// none of them run t.Parallel(), so at most one container from this file
// is ever running at a time.
func newPostgres(t *testing.T) string {
	t.Helper()
	_, dsn := newPostgresContainer(t)
	return dsn
}

// newPostgresContainer is newPostgres with the container handle returned
// alongside the DSN, for the one test that has to STOP Postgres out from
// under an already-running server (loam-ofg.22's readiness proof). Every
// other caller wants only the DSN and goes through newPostgres.
func newPostgresContainer(t *testing.T) (*postgres.PostgresContainer, string) {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, container.Terminate(context.Background()))
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return container, dsn
}

// runningServer is one started, listening instance of the compiled binary.
//
// Both of the child's output streams are captured, and which one a test
// wants is not interchangeable: internal/config builds cfg.Logger over
// os.STDOUT (config.go), so every structured log line the server emits --
// including handler.ErrorMapper's record of an unmapped error -- lands in
// stdout, while stderr carries only what the runtime and a fatal startup
// path write. A test asserting on log CONTENT (e.g. that a secret never
// reaches a log line, credential_integration_test.go) must read stdout;
// reading stderr would find it empty and pass vacuously.
type runningServer struct {
	cmd    *exec.Cmd
	addr   string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// shortDataDir returns a fresh, short-named temp directory for
// LOAM_DATA_DIR, registered for removal via t.Cleanup. Unlike t.TempDir()
// -- whose path embeds the full, often long, subtest-qualified test name
// -- this stays well under the ~104-byte sun_path limit unix domain
// sockets are subject to on macOS/BSD. loam-ofg.18's policy socket binds
// "<LOAM_DATA_DIR>/hook.sock" at startup (before this file's readiness
// poll can ever succeed): a t.TempDir()-based LOAM_DATA_DIR made that bind
// fail loudly for every test in this file once the policy socket was
// wired into Startup -- internal/hooksocket.Listen deliberately fails
// loudly rather than working around a too-long path itself (see that
// package's bindUnixSocket doc comment for why a same-package fallback was
// tried and reverted), so the fix belongs here, at the actual source of
// the long path.
func shortDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "loam-data")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startServer launches the compiled binary against databaseURL, handing it
// an already-bound listener via os/exec's ExtraFiles (LOAM_LISTENER_FD=3)
// instead of the older "reserve a free port, close it, tell the child to
// rebind" approach cmd/server's freeAddr helper used. That approach had an
// unavoidable, small race window between the reserving Close and the
// child's own bind, where anything else on the machine could steal the
// port (loam-2m0); handing across the real *os.File closes that window
// entirely, since the port is never unbound. t.Cleanup kills the process if
// the test itself did not already stop it.
func startServer(t *testing.T, databaseURL string) *runningServer {
	t.Helper()
	return startServerWithEnv(t, databaseURL)
}

// startServerWithEnv is startServer with extra LOAM_* environment entries
// ("KEY=value") appended after the defaults, so a single test can vary one
// setting -- LOAM_SYNC_INTERVAL, today -- without every other test in this
// file inheriting it. Order matters: os/exec takes the LAST occurrence of a
// duplicated key, so an entry here overrides the default above it.
func startServerWithEnv(t *testing.T, databaseURL string, extraEnv ...string) *runningServer {
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
		"LOAM_DATA_DIR=" + shortDataDir(t),
	}
	env = append(env, extraEnv...)
	cmd := exec.Command(serverBinary)
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{listenerFile}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
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
	return &runningServer{cmd: cmd, addr: addr, stdout: &stdout, stderr: &stderr}
}

// waitForReady polls GET /healthz until it returns 200 or the deadline
// passes -- the readiness signal this bead recommends a Taskfile use (see
// main.go's package doc). A bare TCP dial would not do here: with
// loam-2m0's fix, addr is already bound at the OS level before the server
// subprocess even starts (this file's own harness bound it), so a dial
// would report "listening" immediately regardless of whether the server
// process has validated its config, run migrations, connected the pool, or
// even started at all. Polling a real HTTP response is what genuinely
// observes the process has gotten through Startup. Polling with a short
// sleep, rather than a tick/hook, is the same honest exemption the
// previous version of this helper (waitForListening) documented: there is
// no deterministic hook to drive an external OS process's startup with.
func waitForReady(t *testing.T, addr string, stderr *bytes.Buffer) {
	t.Helper()
	client := newIsolatedHTTPClient(t)
	client.Timeout = 200 * time.Millisecond
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready\nstderr: %s", addr, stderr.String())
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
	resp, err := newIsolatedHTTPClient(t).Do(req)
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
	rs := startServer(t, newPostgres(t))
	noAuthBody, noAuthStatus := getWithAuthorization(t, rs.addr, "/healthz", "")
	garbageAuthBody, garbageAuthStatus := getWithAuthorization(t, rs.addr, "/healthz", "Bogus not-a-real-scheme")
	assert.Equal(t, http.StatusOK, noAuthStatus)
	assert.Equal(t, http.StatusOK, garbageAuthStatus)
	assert.Equal(t, noAuthBody, garbageAuthBody)
}

// TestServer_Readyz_ReachableWithAndWithoutAuthorizationHeader is
// /healthz's sibling proof for the OTHER exempt route. It matters
// separately: /readyz is the endpoint that consults the database, so it is
// the one a reviewer would most expect to have been quietly tucked behind
// the admin wrapper, and docs/web-spec.md -> Auth names both, not one.
func TestServer_Readyz_ReachableWithAndWithoutAuthorizationHeader(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	noAuthBody, noAuthStatus := getWithAuthorization(t, rs.addr, "/readyz", "")
	garbageAuthBody, garbageAuthStatus := getWithAuthorization(t, rs.addr, "/readyz", "Bogus not-a-real-scheme")
	assert.Equal(t, http.StatusOK, noAuthStatus, "serverLog: %s", rs.serverLog())
	assert.Equal(t, "ready", noAuthBody)
	assert.Equal(t, http.StatusOK, garbageAuthStatus)
	assert.Equal(t, noAuthBody, garbageAuthBody)
	// A 401 would come with this challenge header; asserting its ABSENCE
	// distinguishes "the route is exempt" from "the route happens to
	// answer 200 to anyone", which a future mis-wiring could not.
	assert.Empty(t, headerOf(t, rs.addr, "/readyz", "WWW-Authenticate"))
}

// headerOf reads one response header from an unauthenticated GET.
func headerOf(t *testing.T, addr, path, header string) string {
	t.Helper()
	resp, err := newIsolatedHTTPClient(t).Get("http://" + addr + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.Header.Get(header)
}

// probe issues an unauthenticated GET and returns the status and body
// without touching *testing.T. require/assert must not be called from the
// goroutine testify runs an Eventually predicate on, so the polling tests
// below need a helper that reports failure by return value.
func probe(addr, path string, client *http.Client) (int, string, error) {
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(body), nil
}

// TestServer_Readyz_ReportsNotReadyWhenPostgresGoesDown is the
// "reports not-ready when X is down" half of loam-ofg.22, and it is
// deliberately driven by stopping the REAL Postgres container out from
// under an already-booted server rather than by a fault injected through
// any test seam. Startup's own fail-fast cannot produce this state at all
// -- run() exits before the listener binds if the pool will not connect --
// so the only way to observe it is to break the dependency AFTER the
// process is serving, which is exactly the failure mode a readiness probe
// exists for.
//
// It asserts three things, and all three are load-bearing:
//
//  1. /readyz answered 200 BEFORE the container stopped. Without this the
//     test would pass against a /readyz that returns 503 unconditionally.
//  2. /readyz becomes 503 naming the database check, polled with
//     require.Eventually rather than sampled once: the pool notices its
//     backend is gone when it next tries to use it, not at the instant
//     the container dies, so a single sample is a race (loam-4q2).
//  3. /healthz is STILL 200 at that same moment. This is the asymmetry
//     the whole design rests on -- an orchestrator must take this
//     instance out of rotation, not kill and restart it, because
//     restarting it would not bring Postgres back. It is also the
//     regression guard for every integration test and `task demo:*`
//     target in this repo, all of which poll /healthz as their startup
//     signal.
func TestServer_Readyz_ReportsNotReadyWhenPostgresGoesDown(t *testing.T) {
	container, dsn := newPostgresContainer(t)
	rs := startServer(t, dsn)
	client := newIsolatedHTTPClient(t)
	client.Timeout = 10 * time.Second
	status, body, err := probe(rs.addr, "/readyz", client)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "readiness must be 200 while Postgres is up, or this test proves nothing; body: %s, serverLog: %s", body, rs.serverLog())
	stopTimeout := 30 * time.Second
	require.NoError(t, container.Stop(context.Background(), &stopTimeout))
	observed := &lastObserved{}
	require.Eventuallyf(t, func() bool {
		status, body, err := probe(rs.addr, "/readyz", client)
		if err != nil {
			observed.set("transport error: " + err.Error())
			return false
		}
		observed.set(strconv.Itoa(status) + " " + body)
		return status == http.StatusServiceUnavailable && strings.Contains(body, "database unreachable")
	}, 60*time.Second, 250*time.Millisecond,
		"with Postgres stopped, /readyz must report 503 and name the database check; last observed %s. serverLog: %s", observed, rs.serverLog())
	liveBody, liveStatus := getWithAuthorization(t, rs.addr, "/healthz", "")
	assert.Equal(t, http.StatusOK, liveStatus, "liveness must survive a database outage: restarting this process would not repair Postgres. serverLog: %s", rs.serverLog())
	assert.Equal(t, "live", liveBody)
	assert.Contains(t, rs.serverLog(), "readiness check failed",
		"the operator's log must carry the failure the 503 body deliberately withholds")
}

// TestServer_Readyz_ReportsNotReadyWhenTheSchemaIsNotCurrent covers the
// migration half of docs/server-spec.md -> Health, and it does so without
// stopping anything: golang-migrate's own bookkeeping row is flipped to
// dirty underneath the running server, which is precisely the state a
// migration that started and never finished leaves behind.
//
// The recovery leg -- restoring the row and watching /readyz return to
// 200 -- is what proves the check is a live per-request read rather than
// a verdict latched at startup or cached after the first failure. A test
// that only drove the failure direction would pass against a handler that
// went unready permanently on first error.
func TestServer_Readyz_ReportsNotReadyWhenTheSchemaIsNotCurrent(t *testing.T) {
	dsn := newPostgres(t)
	rs := startServer(t, dsn)
	client := newIsolatedHTTPClient(t)
	client.Timeout = 10 * time.Second
	status, body, err := probe(rs.addr, "/readyz", client)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "readiness must be 200 against a freshly migrated database; body: %s, serverLog: %s", body, rs.serverLog())
	setSchemaDirty(t, dsn, true)
	status, body, err = probe(rs.addr, "/readyz", client)
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, status, "a dirty schema must take the instance out of rotation; serverLog: %s", rs.serverLog())
	assert.Contains(t, body, "migrations not current")
	_, liveStatus := getWithAuthorization(t, rs.addr, "/healthz", "")
	assert.Equal(t, http.StatusOK, liveStatus, "a schema problem is not a reason to kill and restart the process")
	setSchemaDirty(t, dsn, false)
	status, body, err = probe(rs.addr, "/readyz", client)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status, "readiness must be re-derived per request, so a repaired schema restores it with no restart; body: %s", body)
	assert.Equal(t, "ready", body)
}

// setSchemaDirty flips golang-migrate's schema_migrations.dirty flag
// directly, connecting straight to Postgres rather than through anything
// in this module -- the question is what the RUNNING SERVER observes in
// the real table, so the fixture must not share code with the check under
// test.
func setSchemaDirty(t *testing.T, dsn string, dirty bool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	tag, err := conn.Exec(ctx, `UPDATE schema_migrations SET dirty = $1`, dirty)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "golang-migrate keeps exactly one bookkeeping row; this test's premise is wrong if it does not")
}

// TestServer_Root_ValidAdminAuth_ServesEmbeddedIndex proves the embedded
// SPA (web.Dist, wired in via RegisterSPA in buildRouter) is actually
// reachable through the real binary and serves the real embedded
// index.html content -- the only coverage web.Dist() has anywhere, since
// internal/server's own tests exercise RegisterSPA against an in-memory
// fstest.MapFS, never the production embed.
func TestServer_Root_ValidAdminAuth_ServesEmbeddedIndex(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	req, err := http.NewRequest(http.MethodGet, "http://"+rs.addr+"/", nil)
	require.NoError(t, err)
	req.SetBasicAuth(testAdminUser, testAdminPassword)
	resp, err := newIsolatedHTTPClient(t).Do(req)
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
	rs := startServer(t, newPostgres(t))
	resp, err := newIsolatedHTTPClient(t).Get("http://" + rs.addr + "/")
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
	rs := startServer(t, newPostgres(t))
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
	testGracefulShutdown(t, syscall.SIGINT)
}

// TestServer_SIGTERM_ExitsCleanlyAndClosesListener mirrors the SIGINT case
// for SIGTERM (docs/server-spec.md -> Shutdown: "On SIGTERM: stop
// accepting ... connections"), the signal a container orchestrator sends.
func TestServer_SIGTERM_ExitsCleanlyAndClosesListener(t *testing.T) {
	testGracefulShutdown(t, syscall.SIGTERM)
}

// TestServer_StartupSucceedsAgainstAVirginDatabase is the end-to-end proof
// that run() calls migrate before connecting the pool, against a REAL,
// never-before-migrated Postgres: if cmd/server's startup sequence called
// db.NewPool before migrations.Migrate (loam-ut9's ordering bug, reversed
// here), NewPool would fail loudly the instant it tried to register the
// pgvector type against a database with no vector extension yet
// (internal/db/pool_integration_test.go's
// TestNewPoolFailsLoudlyWithoutExtension proves that failure mode
// directly) -- the process would exit(1) before ever reaching the
// listener, and startServer's readiness poll would time out and fail this
// test. A fresh container is deliberately used, rather than reusing
// another test's already-migrated one, since a database that already has
// the extension would not discriminate a reordering bug at all.
func TestServer_StartupSucceedsAgainstAVirginDatabase(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	body, status := getWithAuthorization(t, rs.addr, "/healthz", "")
	assert.Equal(t, http.StatusOK, status, "stderr: %s", rs.stderr.String())
	assert.Equal(t, "live", body)
}

// TestServer_RequeuesOrphanedIngestJobsOnStartup directly proves
// docs/server-spec.md Startup step 4 runs for real: an ingest_jobs row
// seeded with status='running' -- simulating a job orphaned by a prior
// crash -- is picked back up by the server's own startup, before this test
// ever calls ingest.Pool.RequeueOrphaned itself. Connecting directly with
// pgxpool (not through this package) to seed and later assert keeps the
// proof independent of the server binary's own database code.
//
// The signal is `attempts`, NOT `status`, and that choice is load-bearing.
// This binary wires a real, live ingest.Pool, so a requeued job does not
// come to rest anywhere: RequeueOrphaned flips the row to 'queued', a
// worker claims it (ingest.Pool.claim sets status='running' again), the
// real orchestrator (loam-c94.12) errors -- this seeded repo has no
// enrolled target branch and no mirror on disk -- fail records 'failed',
// and scheduleRetry puts it back to 'queued' after a backoff -- forever. So
// 'running' is a RECURRING state on the happy path, not a one-time
// window, and any single-sample assertion of "status is not running" is
// unfalsifiable: it cannot tell a job still stuck from a crash apart from
// a job correctly requeued and merely mid-attempt. This test asserted
// exactly that and turned CI red intermittently (run 30292966379), which
// is how the flaw was found.
//
// attempts has none of that trouble. It is incremented only by fail
// (`attempts = attempts + 1`) and is never reset -- scheduleRetry sets
// status and queued_at but deliberately leaves attempts alone -- so it is
// monotonic. A job that is never requeued is never claimed (claim only
// selects status='queued'), so it never fails, so attempts stays 0
// forever. attempts >= 1 is therefore reachable if and only if
// RequeueOrphaned ran, with no race to lose. Delete the RequeueOrphaned
// call from main.go and this test hangs on the poll and fails; that is
// the mutation it is built to catch.
func TestServer_RequeuesOrphanedIngestJobsOnStartup(t *testing.T) {
	dsn := newPostgres(t)
	migrateOnce(t, dsn)
	_, jobID := seedOrphanedIngestJob(t, dsn)
	rs := startServer(t, dsn)
	_, status := getWithAuthorization(t, rs.addr, "/healthz", "")
	require.Equal(t, http.StatusOK, status, "stderr: %s", rs.stderr.String())
	assertOrphanedJobWasRequeued(t, dsn, jobID, rs)
}

// TestServer_RunsARealSyncCycleForEnrolledRepos is loam-0do's own
// acceptance line and the thing Demo M3 (loam-bwu) depends on: the SHIPPED
// BINARY, started with a short LOAM_SYNC_INTERVAL and then simply left
// alone, runs a genuine Mirror Sync cycle against every enrolled repo. No
// force-sync RPC, no test seam, no in-process scheduler -- exactly the
// "start the server with LOAM_SYNC_INTERVAL=2s and just wait a few
// seconds" that M3's design calls for.
//
// The fixture is an enrolled repo whose forge host has NO credential row,
// so step 1 (fetch) fails at credential resolution inside
// gittransport.Transport -- deterministically, with no network access and
// no mirror on disk required. The cycle therefore reaches ReportError,
// which is the observable.
//
// THE SIGNAL IS A SPECIFIC repos.sync_error TEXT, NOT repos.sync_state,
// and that choice is load-bearing (loam-4q2). sync_state CYCLES while a
// live scheduler runs: every tick writes 'syncing' at cycle start and
// 'error' at cycle end, forever, so a single sample of it cannot
// distinguish "never ticked" (still the 'idle' default) from "ticked and
// currently mid-cycle" -- and polling for one specific value of a cycling
// column is how TestServer_RequeuesOrphanedIngestJobsOnStartup turned CI
// red.
//
// sync_error has no such trouble here, on two counts.
//
// It does not cycle for THIS fixture: it is NULL on every freshly inserted
// repos row (0001_init.up.sql leaves it nullable with no default; the
// helper below asserts that precondition rather than assuming it), it is
// set by internal/mirrorsync/state's ReportError, and the only writer that
// clears it is that package's ReportIdle -- which is unreachable here,
// because step 1 can never succeed without a credential. So once written
// it stays written; there is no window to lose a race in.
//
// And it is attributed, which is what keeps this honest as more writers
// appear. loam-c94.13 makes ingest.Pool a SECOND writer of sync_state and
// sync_error (claim/succeed/fail), so "sync_error is non-empty" would stop
// being proof of a SYNC tick the moment that lands. The polled predicate
// is therefore not "non-empty" but "contains 'fetching repo <name>'" --
// the wrapper mirrorsync.Scheduler.runSteps puts on step 1's error, which
// no other writer produces (the ingest-side writer prefixes its own text
// with ingest.SyncErrorPrefix). Classifying by author, not by
// non-emptiness, is what makes the assertion survive that merge; the
// fixture additionally never enqueues an ingest job at all, since the
// cycle aborts at step 1 and never reaches step 4.
func TestServer_RunsARealSyncCycleForEnrolledRepos(t *testing.T) {
	dsn := newPostgres(t)
	migrateOnce(t, dsn)
	const repoName = "acme/sync-tick"
	seedEnrolledRepo(t, dsn, repoName)
	rs := startServerWithEnv(t, dsn, "LOAM_SYNC_INTERVAL=1s")
	_, status := getWithAuthorization(t, rs.addr, "/healthz", "")
	require.Equal(t, http.StatusOK, status, "stderr: %s", rs.stderr.String())
	assertSyncCycleRan(t, dsn, repoName, rs)
}

// seedEnrolledRepo inserts one enrolled repos row pointing at a forge host
// with no credential on record -- the fixture
// TestServer_RunsARealSyncCycleForEnrolledRepos needs for a sync cycle
// that reliably reaches ReportError without any network access.
func seedEnrolledRepo(t *testing.T, dsn, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	repoID := uuid.Must(uuid.NewV7())
	_, err = conn.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, $3, $4, $5)`,
		repoID, name, "https://forge.invalid/"+name, "forge.invalid", "main",
	)
	require.NoError(t, err)
	var syncError *string
	require.NoError(t, conn.QueryRow(ctx, `SELECT sync_error FROM repos WHERE id = $1`, repoID).Scan(&syncError))
	require.Nil(t, syncError, "a freshly enrolled repo must start with a NULL sync_error, or this test's signal proves nothing")
	return repoID
}

// assertSyncCycleRan polls repos.sync_error until mirrorsync.Scheduler's
// OWN step-1 failure text lands there. The author-identifying fragment is
// inside the polled predicate, not asserted after it: any other writer of
// this column (loam-c94.13's ingest-side writer, once it lands) must not
// be able to satisfy the wait. See
// TestServer_RunsARealSyncCycleForEnrolledRepos's doc comment for why this
// signal neither cycles nor races.
func assertSyncCycleRan(t *testing.T, dsn, repoName string, rs *runningServer) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	want := "fetching repo " + repoName
	observed := &lastObserved{}
	require.Eventuallyf(t, func() bool {
		var syncError *string
		if err := conn.QueryRow(ctx, `SELECT sync_error FROM repos WHERE name = $1`, repoName).Scan(&syncError); err != nil {
			return false
		}
		if syncError == nil {
			return false
		}
		observed.set(*syncError)
		return strings.Contains(*syncError, want)
	}, 30*time.Second, 100*time.Millisecond,
		"the shipped binary must run a sync cycle on its own ticker and record %q in repos.sync_error; last observed value was %s. stderr: %s",
		want, observed, rs.stderr.String())
}

// lastObserved carries the most recent value a polling predicate saw, so a
// require.Eventuallyf failure message can report it. The indirection is
// needed because Eventuallyf's message ARGUMENTS are evaluated before
// polling begins (when nothing has been observed yet) while the message
// itself is FORMATTED only on failure -- a *lastObserved passed with %s
// therefore renders whatever the last poll stored. testify runs the
// predicate on its own goroutine, hence the mutex.
type lastObserved struct {
	mu    sync.Mutex
	value string
}

func (l *lastObserved) set(value string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.value = value
}

// String implements fmt.Stringer.
func (l *lastObserved) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strconv.Quote(l.value)
}

// TestServer_NonPositiveSyncInterval_FailsFastInsteadOfPanicking proves
// LOAM_SYNC_INTERVAL=0s is rejected as a configuration error rather than
// crashing the process. time.NewTicker panics on a non-positive duration
// and this binary installs no recover() anywhere, so without run()'s
// validateSyncInterval guard this exact environment produces a panic and a
// stack trace on startup. internal/config accepts the value today: it
// parses the duration and range-checks nothing (loam-35b).
//
// It needs no Postgres at all, and that is itself part of the assertion:
// the guard runs at the very top of run(), before connectDatabase, so an
// unreachable DSN cannot be what killed the process. The stderr assertions
// pin both halves -- the message names the variable, and no "panic:" line
// appears.
func TestServer_NonPositiveSyncInterval_FailsFastInsteadOfPanicking(t *testing.T) {
	cmd := exec.Command(serverBinary)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"LOAM_HTTP_ADDR=127.0.0.1:0",
		"LOAM_ADMIN_USER=" + testAdminUser,
		"LOAM_ADMIN_PASSWORD=" + testAdminPassword,
		"LOAM_DATABASE_URL=postgres://loam:loam@127.0.0.1:1/loam?sslmode=disable",
		"LOAM_ENCRYPTION_KEY=" + testEncryptionKey,
		"LOAM_DATA_DIR=" + shortDataDir(t),
		"LOAM_SYNC_INTERVAL=0s",
	}
	// Both streams, into one buffer: config.Load builds cfg.Logger over
	// os.Stdout, so run()'s own returned error is logged there, while the
	// pre-config bootLogger and any runtime panic go to os.Stderr. A test
	// asserting both "the message names the variable" and "there is no
	// panic" has to watch both.
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "the server must exit non-zero on a non-positive LOAM_SYNC_INTERVAL; stderr: %s", output.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the server neither started nor exited on a non-positive LOAM_SYNC_INTERVAL; stderr: %s", output.String())
	}
	require.NotNil(t, cmd.ProcessState)
	assert.Equal(t, 1, cmd.ProcessState.ExitCode(), "stderr: %s", output.String())
	assert.Contains(t, output.String(), "LOAM_SYNC_INTERVAL",
		"the failure must name the variable the operator has to fix, not surface as a database error")
	assert.NotContains(t, output.String(), "panic:",
		"a non-positive interval must be a configuration error, never a time.NewTicker panic")
}

// migrateOnce runs the server binary once, just long enough to reach
// readiness, purely so this test's own direct seeding INSERTs below
// satisfy ingest_jobs' foreign key against a migrated schema. Using the
// real binary rather than calling migrations.Migrate directly keeps this
// test entirely inside the "drive the compiled binary" convention this
// file otherwise follows, and it is itself an assertion that startup
// succeeds against a fresh database, on top of
// TestServer_RequeuesOrphanedIngestJobsOnStartup's own seeded-row proof.
func migrateOnce(t *testing.T, dsn string) {
	t.Helper()
	rs := startServer(t, dsn)
	require.NoError(t, rs.cmd.Process.Signal(syscall.SIGTERM))
	waitExit(t, rs.cmd, 10*time.Second)
}

// seedOrphanedIngestJob inserts a repo and an ingest_jobs row already in
// status='running', standing in for a job a prior crash left mid-flight.
func seedOrphanedIngestJob(t *testing.T, dsn string) (repoID, jobID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	repoID = uuid.Must(uuid.NewV7())
	_, err = conn.Exec(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch) VALUES ($1, $2, $3, $4, $5)`,
		repoID, "acme/orphan-test", "https://example.invalid/acme/orphan-test", "example.invalid", "main",
	)
	require.NoError(t, err)
	jobID = uuid.Must(uuid.NewV7())
	_, err = conn.Exec(ctx,
		`INSERT INTO ingest_jobs (id, repo_id, target_branch, kind, status, started_at) VALUES ($1, $2, $3, $4, 'running', now())`,
		jobID, repoID, "main", "incremental",
	)
	require.NoError(t, err)
	return repoID, jobID
}

// assertOrphanedJobWasRequeued polls jobID until its attempts counter
// leaves 0, proving a worker claimed and ran it -- which can only happen
// after RequeueOrphaned moved it off the 'running' state a crash left it
// in. See TestServer_RequeuesOrphanedIngestJobsOnStartup's doc comment for
// why attempts rather than status is the signal.
//
// Once attempts has moved, the recorded error must be the labeled one the
// real orchestrator produces for this fixture: seedOrphanedIngestJob
// inserts a repos row with NO repo_target_branches row, so the very first
// thing loam-c94.12's Run does after resolving the repo is fail with
// "target branch not enrolled". The job is EXPECTED to fail here (this
// test is about RequeueOrphaned, not about a successful ingest), but it
// must fail for THAT reason and not some other one this test would
// otherwise mask -- in particular not a panic-turned-error or a database
// fault. error is read in the same row read as attempts so the two cannot
// describe different attempts, and it is only asserted when the row is at
// rest in 'failed' -- mid-claim the column still holds the previous
// attempt's text.
func assertOrphanedJobWasRequeued(t *testing.T, dsn string, jobID uuid.UUID, rs *runningServer) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	var attempts int
	var status string
	var jobErr *string
	require.Eventuallyf(t, func() bool {
		if err := conn.QueryRow(ctx, `SELECT attempts, status, error FROM ingest_jobs WHERE id = $1`, jobID).Scan(&attempts, &status, &jobErr); err != nil {
			return false
		}
		return attempts >= 1
	}, 30*time.Second, 50*time.Millisecond,
		"a crash-orphaned job must be requeued on startup and attempted; attempts never left 0. stderr: %s", rs.stderr.String())
	if status == "failed" {
		require.NotNil(t, jobErr)
		assert.Contains(t, *jobErr, "target branch not enrolled", "a claimed job for a repo with no enrolled target branch must fail with that labeled orchestrator error, not some other one")
	}
}
