package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/config"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
)

// identityRoundTripper injects the three trusted Loam-Agent-* request
// headers (docs/cli-spec.md -> Environment Variables) onto every request,
// the way the real CLI does, so a test client can reach a /loam.v1.*
// handler as an identified agent rather than being rejected 401 by
// internal/httpauth.Auth.CLI before ever reaching it.
type identityRoundTripper struct {
	role string
}

func (rt identityRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Loam-Agent-Name", "ada-lovelace")
	req.Header.Set("Loam-Agent-Id", "7")
	req.Header.Set("Loam-Agent-Role", rt.role)
	return http.DefaultTransport.RoundTrip(req)
}

func testConfigForRouter() config.Config {
	return config.Config{
		AdminUser:     "admin",
		AdminPassword: "s3cret-pass",
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

// unreachablePool builds a *pgxpool.Pool that never actually connects
// (pgxpool.NewWithConfig dials lazily, on first acquire -- see
// internal/db/pool.go's own doc comment on this exact property) pointed at
// 127.0.0.1 on a port nothing listens on, so a real request through it
// fails fast (typically "connection refused") without needing a live
// Postgres or a testcontainers-go container. Good enough to prove
// registration: the point is that the request reaches the REAL handler
// and attempts REAL work, not that the work succeeds.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolCfg, err := pgxpool.ParseConfig("postgres://user:pass@127.0.0.1:1/loam")
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(t.Context(), poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// TestBuildRouter_NilPool_RepoServiceStillHitsGroupFallback is the
// "before" half of this bead's registration proof, exercising
// registerMetadataServices's nil guard directly: buildRouter given no
// pool (a case run() itself never hits -- it always passes
// connectDatabase's live pool through, see main.go's package doc; this is
// buildRouter's own defensive path) still answers RepoService requests
// through the group-level 404 fallback (internal/server -> loam-cjq),
// exactly as every /loam.v1.RepoService/* request did before this bead
// existed. This is the baseline the "with a pool" test below is
// contrasted against; TestServer_RepoServiceIsRegistered_NotGroupFallback
// (registration_integration_test.go) is the same proof against the real,
// booted binary.
func TestBuildRouter_NilPool_RepoServiceStillHitsGroupFallback(t *testing.T) {
	t.Parallel()
	router := buildRouter(testConfigForRouter(), nil)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	client := loamv1connect.NewRepoServiceClient(&http.Client{Transport: identityRoundTripper{role: "author"}}, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.GetRepo(ctx, connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/doc-server"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code())
	assert.Contains(t, connectErr.Message(), "no /loam.v1. service registered",
		"with no pool wired in, RepoService must still be entirely unregistered -- the group fallback's own message")
}

// TestBuildRouter_WithPool_RepoServiceIsGenuinelyRegistered is a fast,
// container-free registration proof: a request to
// /loam.v1.RepoService/GetRepo no longer hits internal/server's
// group-level 404 fallback once a pool is supplied -- the shape of pool
// run() always passes since loam-ofg.21 landed connectDatabase. It does
// NOT need a live, reachable Postgres to prove this -- only that the
// request reaches the REAL handler (whose capability check attempts a
// real query and fails with a connection error, mapped to CodeInternal by
// handler.ErrorMapper) rather than the fallback's hand-written "no
// service registered" 404. Compare directly against
// TestBuildRouter_NilPool_RepoServiceStillHitsGroupFallback above: same
// request, same identity, only the pool differs, and the code AND message
// are both different as a result.
// TestServer_RepoServiceIsRegistered_NotGroupFallback
// (registration_integration_test.go) is the slower, real-Postgres version
// of this same proof against the actual booted binary -- run this pair
// together when the question is "does this run for real", not just "is
// the wiring correct".
func TestBuildRouter_WithPool_RepoServiceIsGenuinelyRegistered(t *testing.T) {
	t.Parallel()
	pool := unreachablePool(t)
	router := buildRouter(testConfigForRouter(), pool)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	client := loamv1connect.NewRepoServiceClient(&http.Client{Transport: identityRoundTripper{role: "author"}}, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.GetRepo(ctx, connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/doc-server"}))
	require.Error(t, err, "the underlying pool is unreachable, so this must still fail -- but for a REAL reason")
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.NotEqual(t, connect.CodeNotFound, connectErr.Code(),
		"a real (if unreachable-database) failure inside the handler must not coincidentally look like the fallback's CodeNotFound")
	assert.NotContains(t, connectErr.Message(), "service registered",
		"the request must reach the real RepoServiceHandler, never the group fallback's canned message, once a pool is wired in")
}

// TestBuildRouter_WithPool_MetaServiceIsGenuinelyRegistered mirrors the
// RepoService proof above for MetaService.GetInstructions -- the other
// handler this bead registers.
func TestBuildRouter_WithPool_MetaServiceIsGenuinelyRegistered(t *testing.T) {
	t.Parallel()
	pool := unreachablePool(t)
	router := buildRouter(testConfigForRouter(), pool)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	client := loamv1connect.NewMetaServiceClient(&http.Client{Transport: identityRoundTripper{role: "author"}}, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := client.GetInstructions(ctx, connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.NotContains(t, connectErr.Message(), "service registered",
		"the request must reach the real MetaServiceHandler, never the group fallback's canned message, once a pool is wired in")
}
