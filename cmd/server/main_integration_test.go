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

// TestMain compiles cmd/server once before any test in this file runs.
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

// newPostgres starts one pgvector-enabled Postgres container and returns
// its DSN, registering cleanup that terminates it. Every test in this file
// calls this itself (rather than sharing one container via TestMain) and
// none of them run t.Parallel(), so at most one container from this file
// is ever running at a time.
func newPostgres(t *testing.T) string {
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
	return dsn
}

// runningServer is one started, listening instance of the compiled binary.
type runningServer struct {
	cmd    *exec.Cmd
	addr   string
	stderr *bytes.Buffer
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
		"LOAM_DATA_DIR=" + t.TempDir(),
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
// docs/server-spec.md Startup step 4 runs for real: a repo and ingest_jobs
// row seeded with status='running' -- simulating a job orphaned by a prior
// crash -- is no longer 'running' by the time the server has finished
// starting, before this test ever calls ingest.Pool.RequeueOrphaned
// itself. It cannot assert the row lands on exactly 'queued' and stays
// there: this binary wires a real, live ingest.Pool (see main.go's
// notImplementedOrchestrator), so once RequeueOrphaned flips the row to
// 'queued' a real worker goroutine can race in and claim it before this
// test's own request even returns -- and since notImplementedOrchestrator
// always errors, a claimed row immediately becomes 'failed'. Both
// outcomes are acceptable proof that RequeueOrphaned ran (the row is
// provably no longer stuck at 'running', the state a crash would have
// left it in); only 'failed' additionally requires the recorded error to
// be the expected, clearly-labeled placeholder, not some other failure
// this test would otherwise mask. Connecting directly with pgxpool (not
// through this package) to seed and later assert keeps the proof
// independent of the server binary's own database code.
func TestServer_RequeuesOrphanedIngestJobsOnStartup(t *testing.T) {
	dsn := newPostgres(t)
	migrateOnce(t, dsn)
	_, jobID := seedOrphanedIngestJob(t, dsn)
	rs := startServer(t, dsn)
	_, status := getWithAuthorization(t, rs.addr, "/healthz", "")
	require.Equal(t, http.StatusOK, status, "stderr: %s", rs.stderr.String())
	assertJobLeftRunningState(t, dsn, jobID)
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

// assertJobLeftRunningState asserts jobID is no longer status='running'
// (RequeueOrphaned's job) and, if a live worker has already claimed and
// failed it, that the recorded error is the expected placeholder rather
// than some other, masked failure. See
// TestServer_RequeuesOrphanedIngestJobsOnStartup's doc comment for why
// 'queued' and 'failed' are both acceptable outcomes here.
func assertJobLeftRunningState(t *testing.T, dsn string, jobID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	var status string
	var jobErr *string
	require.NoError(t, conn.QueryRow(ctx, `SELECT status, error FROM ingest_jobs WHERE id = $1`, jobID).Scan(&status, &jobErr))
	require.NotEqual(t, "running", status, "RequeueOrphaned should have reset this crash-orphaned job off running on startup")
	require.Contains(t, []string{"queued", "failed"}, status)
	if status == "failed" {
		require.NotNil(t, jobErr)
		assert.Contains(t, *jobErr, "not implemented", "a claimed job should only fail via the labeled ingest-orchestrator placeholder, not some other error")
	}
}
