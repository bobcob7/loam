package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getHealth issues an unauthenticated GET against srv and returns the
// status and body. No Authorization header is set anywhere, which is the
// point: docs/web-spec.md -> Auth makes the two health routes "the only
// such exemption".
func getHealth(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// TestBuildRouter_HealthzIsRegisteredEvenWithNoPool pins the one
// asymmetry in registerHealth: /healthz has no collaborator to guard, so
// it must be reachable in every configuration buildRouter can be built
// in, including the nil-pool one buildRouter's own tests use. Every
// integration test in this package and every `task demo:*` target polls
// this endpoint as its startup signal, so a nil-pool guard accidentally
// wrapped around it would be a wide, quiet breakage.
func TestBuildRouter_HealthzIsRegisteredEvenWithNoPool(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(buildRouter(testConfigForRouter(), nil, nil, "", nil).Handler())
	t.Cleanup(srv.Close)
	status, body := getHealth(t, srv, "/healthz")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "live", body)
}

// TestBuildRouter_WithPool_ReadyzIsGenuinelyWiredToThatPool proves
// /readyz is registered against the REAL pool rather than a stand-in that
// always says yes: the pool here is pointed at a port nothing listens on
// (unreachablePool), so the only way to get a 503 naming the database
// check is for the handler to have actually pinged it.
//
// This is the wiring-level counterpart to internal/health's own unit
// tests, which prove the handler's branches over moq'd collaborators but
// cannot prove cmd/server handed it the process's live pool.
func TestBuildRouter_WithPool_ReadyzIsGenuinelyWiredToThatPool(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(buildRouter(testConfigForRouter(), unreachablePool(t), nil, "", nil).Handler())
	t.Cleanup(srv.Close)
	status, body := getHealth(t, srv, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "not ready: database unreachable", body)
	liveStatus, liveBody := getHealth(t, srv, "/healthz")
	assert.Equal(t, http.StatusOK, liveStatus, "liveness must not consult the pool at all")
	assert.Equal(t, "live", liveBody)
}
