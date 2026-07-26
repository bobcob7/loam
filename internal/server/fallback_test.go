package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRouter_UnregisteredCLIServicePrefix_GetAndPost proves loam-cjq's core
// claim: a request under /loam.v1.* naming a service this Router never
// registered (unlike cliPrefix, which newTestRouter DOES register) gets a
// Connect 404 envelope -- never the SPA's index.html -- for both GET and
// POST, and never text/html on the wire.
func TestRouter_UnregisteredCLIServicePrefix_GetAndPost(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(method, "/loam.v1.NoSuchService/DoThing", nil)
			withValidAdminAuth(req)
			rec := httptest.NewRecorder()
			router.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.NotContains(t, rec.Header().Get("Content-Type"), "text/html")
			assert.NotContains(t, rec.Body.String(), "SPA-INDEX")
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "not_found", body["code"])
		})
	}
}

// TestRouter_UnregisteredAdminServicePrefix_GetAndPost mirrors the CLI test
// above for /loam.admin.v1.*.
func TestRouter_UnregisteredAdminServicePrefix_GetAndPost(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(method, "/loam.admin.v1.NoSuchService/DoThing", nil)
			withValidAdminAuth(req)
			rec := httptest.NewRecorder()
			router.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.NotContains(t, rec.Header().Get("Content-Type"), "text/html")
			assert.NotContains(t, rec.Body.String(), "SPA-INDEX")
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "not_found", body["code"])
		})
	}
}

// TestRouter_UnregisteredCLIServicePrefix_AuthMatchesCLIGroup proves the
// fallback is wrapped in the SAME auth wrapper RegisterCLI uses (Auth.CLI),
// not AdminOnly (the SPA's wrapper): valid agent identity headers alone
// must reach the 404 body, and a request with neither admin auth nor agent
// headers must be rejected 401, exactly like a registered CLI procedure.
func TestRouter_UnregisteredCLIServicePrefix_AuthMatchesCLIGroup(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	agentReq := httptest.NewRequest(http.MethodGet, "/loam.v1.NoSuchService/DoThing", nil)
	withCompleteAgentHeaders(agentReq)
	agentRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(agentRec, agentReq)
	assert.Equal(t, http.StatusNotFound, agentRec.Code, "agent identity headers alone must be enough to reach the CLI group's 404")
	noAuthReq := httptest.NewRequest(http.MethodGet, "/loam.v1.NoSuchService/DoThing", nil)
	noAuthRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(noAuthRec, noAuthReq)
	assert.Equal(t, http.StatusUnauthorized, noAuthRec.Code, "no credential at all must still be rejected under CLI semantics")
}

// TestRouter_UnregisteredAdminServicePrefix_RejectsAgentHeaders proves the
// admin group's fallback keeps AdminOnly semantics: agent identity headers,
// sufficient on the CLI group, are never enough here.
func TestRouter_UnregisteredAdminServicePrefix_RejectsAgentHeaders(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/loam.admin.v1.NoSuchService/DoThing", nil)
	withCompleteAgentHeaders(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestRouter_GitUnknownPath_ReturnsGitStyle404 proves loam-cjq's /git/
// claim: an unrecognized path under /git/* (no mirror handler matches it)
// gets an ordinary git HTTP 404, never text/html, when the caller presents
// valid agent identity.
func TestRouter_GitUnknownPath_ReturnsGitStyle404(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/git/no/such/repo.git/info/refs", nil)
	withCompleteAgentHeaders(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.NotContains(t, rec.Body.String(), "SPA-INDEX")
}

// TestRouter_GitUnknownPath_StillEnforcesGitIdentity proves the /git/
// fallback keeps GitIdentity's auth regime: missing agent identity is
// rejected 403 (not 401), and admin basic auth confers nothing, matching
// docs/git-spec.md -- "Admin basic auth is not accepted on /git/*".
func TestRouter_GitUnknownPath_StillEnforcesGitIdentity(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	noIdentityReq := httptest.NewRequest(http.MethodGet, "/git/no/such/repo.git/info/refs", nil)
	noIdentityRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(noIdentityRec, noIdentityReq)
	assert.Equal(t, http.StatusForbidden, noIdentityRec.Code)
	adminOnlyReq := httptest.NewRequest(http.MethodGet, "/git/no/such/repo.git/info/refs", nil)
	withValidAdminAuth(adminOnlyReq)
	adminOnlyRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(adminOnlyRec, adminOnlyReq)
	assert.Equal(t, http.StatusForbidden, adminOnlyRec.Code, "admin basic auth must not substitute for agent identity on /git/*")
}

// TestRouter_RegisteredServicePrefix_UnaffectedByGroupFallback re-proves,
// after wiring the group fallback into Handler, that a request matching an
// ACTUALLY REGISTERED service still reaches that service's own handler --
// the fallback must never shadow a real registration. This is the
// regression this bead explicitly must not cause.
func TestRouter_RegisteredServicePrefix_UnaffectedByGroupFallback(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, cliPingPath, nil)
	withValidAdminAuth(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "cli-ok", rec.Body.String())
}

// TestRouter_SPACatchAll_UnaffectedByGroupFallback proves a genuine web
// route (no group prefix at all) still reaches the SPA, unaffected by the
// new fallback dispatch added to Handler.
func TestRouter_SPACatchAll_UnaffectedByGroupFallback(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/some/client/route", nil)
	withValidAdminAuth(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "SPA-INDEX")
}

// TestRouter_UnregisteredServicePrefix_NoSPARegistered proves the group
// fallback works even when RegisterSPA was never called -- it does not
// piggyback on the SPA's own "/" registration, so a Router used only for
// its RPC groups (no static UI mounted at all) still gets a proper Connect
// 404 rather than Go's bare "404 page not found" text handler.
func TestRouter_UnregisteredServicePrefix_NoSPARegistered(t *testing.T) {
	t.Parallel()
	auth := httpauth.New(testAdminUser, testAdminPassword)
	router := server.New(auth)
	router.RegisterCLI(cliPrefix, pingStub(cliPingPath, "cli-ok"))
	req := httptest.NewRequest(http.MethodGet, "/loam.v1.NoSuchService/DoThing", nil)
	withValidAdminAuth(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Header().Get("Content-Type"), "text/html")
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "not_found", body["code"])
}
