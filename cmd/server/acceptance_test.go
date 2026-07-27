//go:build acceptance

// This file, and every other acceptance_*_test.go file in this package, is
// docs/testing-spec.md's Layer 1 acceptance harness (loam-li0.5): godog
// runs features/ against the server wired through THIS package's own run()
// -- the exact composition root cmd/server/main.go's main() calls, not a
// hand-rolled subset -- plus the compiled loam CLI binary as a real
// subprocess per actor. It lives in package main (not a separate importable
// package) for the same reason main_integration_test.go and
// clonepush_integration_test.go already do: buildRouter, connectDatabase,
// run, and every other collaborator this bead reuses are unexported, so the
// only way to call them without widening this package's public surface is
// a test file in the same package.
//
// Run it with (CGO_ENABLED=1 required repo-wide, per this repo's Go
// standards):
//
//	TESTCONTAINERS_RYUK_DISABLED=true CGO_ENABLED=1 \
//	  go test -tags=acceptance ./cmd/server/... -run TestFeatures -v
//
// The default run excludes every @wip-tagged scenario (LOAM_ACCEPTANCE_TAGS
// defaults to "~@wip"): a feature landing is a tag removal in features/*.feature,
// never a change to this harness. To run a still-@wip scenario locally
// during development -- without touching the committed tag -- override the
// tag expression and narrow to it by name, e.g.:
//
//	LOAM_ACCEPTANCE_TAGS="@wip" LOAM_ACCEPTANCE_NAME="Pushing commits with plain git" \
//	  TESTCONTAINERS_RYUK_DISABLED=true go test -tags=acceptance ./cmd/server/... -run TestFeatures -v
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/testdb"
)

// acceptanceLogger returns a discard-everything structured logger, per this
// repo's test-logger convention (slog.NewJSONHandler(io.Discard, nil)),
// used for every collaborator this harness constructs directly rather than
// through run() (which already builds its own logger from cfg.Logger).
func acceptanceLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// acceptanceAdminUser/Password/EncryptionKey mirror
// main_integration_test.go's own test constants (a distinct copy, per this
// repo's established convention of small, self-contained duplication
// across independently build-tagged test files rather than a shared,
// tag-crossing helper file -- see startServerWithDataDir's own doc comment
// for the precedent).
const (
	acceptanceAdminUser     = "admin"
	acceptanceAdminPassword = "s3cret-acceptance-pass"
	acceptanceEncryptionKey = "nMjGBpIoO1n40SGBc7WQEnT/FHff/dpDkHu5cB527fg="
)

// acceptanceSyncInterval pushes the PRODUCTION sync scheduler -- which
// run() now constructs and runs for real (loam-0do) -- far beyond this
// suite's own runtime, so it never fires a tick while the suite is
// running.
//
// This is not belt-and-braces. Every "the next sync runs" step drives
// newSyncHarness's own, separate Scheduler through testsched, and the
// scenarios then assert on what that one deterministic cycle produced. A
// wall-clock tick landing in the middle of a scenario would cycle the same
// repo concurrently through a SECOND scheduler -- two git fetches into one
// mirror, two racing repos.sync_state writers, mergeability re-evaluated
// against a half-fetched tip -- and would do it nondeterministically,
// which is the one failure mode an acceptance suite must not have.
//
// The per-repo in-flight guard does not help here: it is per Scheduler
// instance (a map on the struct), so it cannot see the other scheduler's
// cycle at all. Interval, not the guard, is what keeps them apart. The
// default (60s) is well inside a suite run that takes minutes.
const acceptanceSyncInterval = "24h"

// acceptanceLoamBinary is the path to the compiled `loam` CLI, built once
// by TestMain for the whole acceptance test binary -- the Author/Reviewer
// actor driver testing-spec Layer 1's table names (per-actor workspace
// tmpdir + LOAM_AGENT_* env, real subprocess, real git). Package-level
// mutable state set exactly once from TestMain before any test runs
// mirrors cmd/server/clonepush_integration_test.go's identical
// loamBinaryPath convention.
var acceptanceLoamBinary string

// TestMain builds the two sibling binaries this suite's in-process server
// and CLI actor driver both need before any test runs:
//
//   - loamhook, as a sibling of THIS TEST BINARY's own executable. run()
//     (called directly, in-process, by TestFeatures below) resolves its
//     hook binary via loamhookBinaryPath(os.Executable, os.Stat) with no
//     override seam -- exactly as it does for the real "server" binary in
//     production -- so for run() to boot inside a `go test` process, a
//     real loamhook binary must sit next to that process's own compiled
//     test executable, not next to some other binary this suite happens
//     to also build.
//   - loam, the CLI's own binary, built once into a throwaway temp
//     directory (never colliding with the test binary's own directory) and
//     shared by every scenario's Author/Reviewer actor driver.
func TestMain(m *testing.M) {
	if err := buildAcceptanceLoamhookSibling(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	loamBinary, cleanup, err := buildAcceptanceLoamBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer cleanup()
	acceptanceLoamBinary = loamBinary
	os.Exit(m.Run())
}

// buildAcceptanceLoamhookSibling resolves this test binary's own executable
// path and compiles cmd/loamhook to "loamhook" right beside it -- see
// TestMain's doc comment for why that exact location is load-bearing.
func buildAcceptanceLoamhookSibling() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving acceptance test binary's own executable path: %w", err)
	}
	hookPath := filepath.Join(filepath.Dir(exePath), "loamhook")
	build := exec.Command("go", "build", "-o", hookPath, "github.com/bobcob7/loam/cmd/loamhook")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		return fmt.Errorf("building loamhook binary at %s: %w: %s", hookPath, buildErr, out)
	}
	return nil
}

// buildAcceptanceLoamBinary compiles cmd/loam once into a fresh temp
// directory, returning its path and a cleanup func removing that directory.
func buildAcceptanceLoamBinary() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "loam-acceptance-cli-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir for loam CLI binary: %w", err)
	}
	path = filepath.Join(dir, "loam")
	build := exec.Command("go", "build", "-o", path, "github.com/bobcob7/loam/cmd/loam")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("building loam CLI binary: %w: %s", buildErr, out)
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}

// TestFeatures is the acceptance suite's single entry point: one shared
// Postgres container (testcontainers, per docs/testing-spec.md's "real
// infrastructure by default"), one shared in-process server (this
// package's own run(), the same composition root main() uses), one shared
// fakeforge instance, and one godog run over features/ with @wip filtered
// out by default. Everything shares one instance for the whole suite --
// not one per scenario -- for two reasons this repo's own constraints
// spell out: containers run ONE at a time, and go-task and CI both budget
// this suite in minutes, not per-scenario container-boot minutes. Test
// isolation between scenarios instead comes from each scenario generating
// its own uniquely-named repo/work-branch/workspace (see
// acceptance_world_test.go's newScenarioWorld), never from shared state.
func TestFeatures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dsn := acceptancePostgres(t)
	dataDir := acceptanceShortDataDir(t)
	cfg := acceptanceConfig(t, dsn, dataDir)
	srv := startAcceptanceServer(t, ctx, cancel, cfg)
	forge, forgeBaseURL, forgeHost := startAcceptanceForge(t)
	harness := newAcceptanceHarness(t, srv, forge, forgeBaseURL, forgeHost, cfg)
	suite := godog.TestSuite{
		Name:                "loam-acceptance",
		ScenarioInitializer: harness.initializeScenario,
		Options: &godog.Options{
			Format:    "pretty",
			Paths:     []string{acceptanceFeaturesDir(t)},
			Tags:      acceptanceTagExpression(),
			Strict:    true,
			TestingT:  t,
			Randomize: 0,
		},
	}
	if code := suite.Run(); code != 0 {
		t.Fatalf("godog acceptance suite reported failure (exit code %d)", code)
	}
}

// acceptanceTagExpression returns the godog tag filter the suite runs
// with: "~@wip" (exclude every @wip-tagged scenario) unless
// LOAM_ACCEPTANCE_TAGS overrides it -- the one env var a developer sets to
// prove a scenario still tagged @wip actually passes, without editing the
// committed feature file. Landing a feature for real is still a plain tag
// removal in features/*.feature: with no override set, removing @wip is
// entirely sufficient for the default run to pick the scenario up. This
// godog version's Options has no scenario-name filter, so narrowing a
// temporary "@wip" override down to one scenario (rather than every
// still-@wip one, most of which have no step definitions yet) means
// either a distinguishing tag or a temporary local @wip removal in the
// feature file itself -- see this bead's own report for how it was run.
func acceptanceTagExpression() string {
	if tags := os.Getenv("LOAM_ACCEPTANCE_TAGS"); tags != "" {
		return tags
	}
	return "~@wip"
}

// acceptanceFeaturesDir resolves the repo's features/ directory as an
// absolute path, independent of the working directory `go test` happens to
// invoke this binary from.
func acceptanceFeaturesDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Join(wd, "..", "..", "features")
}

// acceptancePostgres starts one pgvector-enabled Postgres testcontainer for
// the whole suite and returns its DSN, registering cleanup to terminate it.
// Callers must set TESTCONTAINERS_RYUK_DISABLED=true in their environment,
// per this repo's container-usage constraints (documented on every other
// integration suite's own container helper).
func acceptancePostgres(t *testing.T) string {
	t.Helper()
	container, err := postgres.Run(t.Context(), testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)
	return dsn
}

// acceptanceShortDataDir returns a fresh, short-named LOAM_DATA_DIR:
// hooksocket.Listen binds "<dir>/hook.sock", and unix domain sockets cap
// sun_path at ~104 bytes, so a t.TempDir()-style path (which embeds the
// full, subtest-qualified test name) can make the server exit before it
// ever listens (see cmd/server/main_integration_test.go's shortDataDir,
// reproduced here rather than shared across build tags).
func acceptanceShortDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "loam-a")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// acceptanceConfig builds a real config.Config by setting every required
// LOAM_* environment variable once for the whole test process and calling
// config.Load() -- the same loader main() itself calls, so a config
// mistake in this harness (e.g. an invalid encryption key) is caught the
// same way it would be in production, rather than bypassed by constructing
// a config.Config struct literal directly. Setting the environment once,
// before the single shared server in this suite ever boots, is safe
// despite LOAM_* variables being process-global: TestFeatures builds
// exactly one server for the whole suite (see its own doc comment), so
// there is no concurrent config.Load() call anywhere in this binary for
// these variables to race with.
func acceptanceConfig(t *testing.T, databaseURL, dataDir string) config.Config {
	t.Helper()
	httpAddr := acceptanceFreeAddr(t)
	t.Setenv("LOAM_HTTP_ADDR", httpAddr)
	t.Setenv("LOAM_ADMIN_USER", acceptanceAdminUser)
	t.Setenv("LOAM_ADMIN_PASSWORD", acceptanceAdminPassword)
	t.Setenv("LOAM_DATABASE_URL", databaseURL)
	t.Setenv("LOAM_ENCRYPTION_KEY", acceptanceEncryptionKey)
	t.Setenv("LOAM_DATA_DIR", dataDir)
	t.Setenv("LOAM_SYNC_INTERVAL", acceptanceSyncInterval)
	t.Setenv("LOAM_PR_ATTRIBUTION", "")
	t.Setenv("LOAM_EMBEDDER_URL", "")
	t.Setenv("LOAM_EMBEDDER_MODEL", "")
	t.Setenv("LOAM_INGEST_WORKERS", "")
	t.Setenv("LOAM_LOG_LEVEL", "")
	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg
}

// acceptanceFreeAddr reserves an ephemeral TCP port on 127.0.0.1 and
// returns its address string, closing the probe listener immediately:
// run() binds cfg.HTTPAddr itself (no inherited-fd seam is wired for this
// suite, unlike main_integration_test.go's compiled-subprocess harness),
// so unlike that file this cannot hand across an already-bound listener --
// the ordinary reserve-then-rebind race this accepts is the same one
// loam-2m0 eliminated for the compiled-binary harness, judged acceptable
// here since only this one process ever binds ports for the whole suite.
func acceptanceFreeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

// acceptanceServer bundles the live handles TestFeatures and the harness
// builder need from one in-process run() call: cfg.HTTPAddr as the base
// URL every actor driver (CLI subprocess, admin connect-go client) talks
// to, plus the pool/ingestPool/hookBinaryPath run()'s onReady callback
// hands back (see main.go's run doc comment for why that callback exists).
type acceptanceServer struct {
	baseURL        string
	pool           *pgxpool.Pool
	ingestPool     *ingest.Pool
	hookBinaryPath string
	dataDir        string
}

// startAcceptanceServer calls run() -- THE exact function main() calls,
// not a hand-rolled subset of its steps -- in a background goroutine with
// a cancelable context this test controls (ctx/stop are run's own
// parameters precisely so a test harness can do this; see run's doc
// comment), waits for it to become ready by polling /healthz exactly as
// every other integration test in this package does, and registers
// cleanup that cancels ctx and waits (bounded) for run() to return cleanly.
func startAcceptanceServer(t *testing.T, ctx context.Context, cancel context.CancelFunc, cfg config.Config) acceptanceServer {
	t.Helper()
	ready := make(chan acceptanceServer, 1)
	runErr := make(chan error, 1)
	go func() {
		onReady := func(pool *pgxpool.Pool, ingestPool *ingest.Pool, hookBinaryPath string) {
			ready <- acceptanceServer{baseURL: "http://" + cfg.HTTPAddr, pool: pool, ingestPool: ingestPool, hookBinaryPath: hookBinaryPath, dataDir: cfg.DataDir}
		}
		runErr <- run(ctx, cancel, cfg, onReady)
	}()
	var srv acceptanceServer
	select {
	case srv = <-ready:
	case err := <-runErr:
		t.Fatalf("in-process server exited before becoming ready: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("in-process server never called onReady within 30s")
	}
	acceptanceWaitHealthy(t, srv.baseURL)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("in-process server did not shut down within 10s of cancellation")
		}
	})
	return srv
}

// acceptanceWaitHealthy polls GET /healthz until it returns 200, the same
// honest readiness signal (real Startup has already run: migrations, pool,
// policy socket) every other integration suite in this package uses.
func acceptanceWaitHealthy(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("in-process server at %s never became ready", baseURL)
}

// startAcceptanceForge starts one in-process fakeforge.Server (li0.1) for
// the whole suite -- the Upstream forge actor's driver (testing-spec Layer
// 1 table) -- wrapped in a real httptest.Server so its git smart-HTTP and
// provider REST surfaces are reachable over real HTTP, exactly as
// internal/mirrorsync's own fetcher_gittransport_test.go already
// establishes this combination.
//
// It returns the httptest base URL and its bare host:port alongside the
// Server: the base URL is what a *fakeforge.Client's provider REST calls
// target, and the host:port is the repos.forge_host value every repo
// seeded against this fake carries -- the key gittransport resolves a
// credential under, and the reason both must come from the SAME
// httptest.Server the git smart-HTTP surface is reachable on.
func startAcceptanceForge(t *testing.T) (server *fakeforge.Server, baseURL, host string) {
	t.Helper()
	srv, err := fakeforge.New(acceptanceLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.SetBaseURL(ts.URL)
	parsed, err := url.Parse(ts.URL)
	require.NoError(t, err)
	return srv, ts.URL, parsed.Host
}
