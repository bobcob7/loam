package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAdminUser     = "admin"
	testAdminPassword = "s3cret-pass"

	headerAgentName = "Loam-Agent-Name"
	headerAgentID   = "Loam-Agent-Id"
	headerAgentRole = "Loam-Agent-Role"

	cliPingPath   = "/loam.v1.WorkBranchService/Ping"
	cliPrefix     = "/loam.v1.WorkBranchService/"
	adminPingPath = "/loam.admin.v1.RepoAdminService/Ping"
	adminPrefix   = "/loam.admin.v1.RepoAdminService/"
	gitPushPath   = "/git/acme/widgets.git/info/refs"
	gitPrefix     = "/git/"
)

// pingStub mimics a generated connect-go handler closely enough for this
// package's purposes: it answers exactly one known procedure under prefix
// with 200, and 404s everything else in its subtree, the same shape
// connect-go's own generated switch produces (see
// loamv1connect.NewWorkBranchServiceHandler) -- this is what lets
// TestRouter_APINotFound_NotSwallowedBySPA assert against a realistic
// "known service, unknown procedure" 404 rather than an invented one.
func pingStub(okPath, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != okPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

// newTestRouter builds a Router with one stub handler registered in each
// path group plus the health endpoints and an in-memory SPA filesystem, so
// every test in this file exercises the real dispatch and the real
// internal/httpauth wrappers -- never a fake standing in for either.
func newTestRouter(t *testing.T) *server.Router {
	t.Helper()
	auth := httpauth.New(testAdminUser, testAdminPassword)
	router := server.New(auth)
	router.RegisterCLI(cliPrefix, pingStub(cliPingPath, "cli-ok"))
	router.RegisterAdmin(adminPrefix, pingStub(adminPingPath, "admin-ok"))
	router.RegisterGit(gitPrefix, pingStub(gitPushPath, "git-ok"))
	router.RegisterUnauthenticated("/healthz", pingStub("/healthz", "healthz-ok"))
	router.RegisterUnauthenticated("/readyz", pingStub("/readyz", "readyz-ok"))
	router.RegisterSPA(fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>SPA-INDEX</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('app')")},
	})
	return router
}

func withValidAdminAuth(r *http.Request) {
	r.SetBasicAuth(testAdminUser, testAdminPassword)
}

func withWrongAdminAuth(r *http.Request) {
	r.SetBasicAuth(testAdminUser, "not-the-password")
}

func withCompleteAgentHeaders(r *http.Request) {
	r.Header.Set(headerAgentName, "ada")
	r.Header.Set(headerAgentID, "7")
	r.Header.Set(headerAgentRole, "reviewer")
}

func withIncompleteAgentHeaders(r *http.Request) {
	r.Header.Set(headerAgentName, "ada")
}

func withGarbageAuthorization(r *http.Request) {
	r.Header.Set("Authorization", "Bogus not-a-real-scheme")
}

// TestRouter_PathGroupCredentialMatrix drives the real mux (not
// internal/httpauth in isolation) across every path group x credential
// combination named in the wave-4 brief, including the negative cells
// that prove each wrapper is bound to the RIGHT path group: agent headers
// rejected on the admin/static group, admin basic auth inert on /git/*,
// and /loam.v1.* rejecting a request that carries neither credential.
func TestRouter_PathGroupCredentialMatrix(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	tests := []struct {
		name       string
		path       string
		setReq     func(r *http.Request)
		wantStatus int
	}{
		{"healthz_no_auth", "/healthz", nil, http.StatusOK},
		{"readyz_no_auth", "/readyz", nil, http.StatusOK},

		{"cli_admin_basic_auth_valid", cliPingPath, withValidAdminAuth, http.StatusOK},
		{"cli_agent_headers_complete", cliPingPath, withCompleteAgentHeaders, http.StatusOK},
		{"cli_agent_headers_incomplete", cliPingPath, withIncompleteAgentHeaders, http.StatusUnauthorized},
		{"cli_no_auth", cliPingPath, nil, http.StatusUnauthorized},
		{"cli_admin_basic_auth_wrong_password", cliPingPath, withWrongAdminAuth, http.StatusUnauthorized},

		{"admin_valid_basic_auth", adminPingPath, withValidAdminAuth, http.StatusOK},
		{"admin_agent_headers_rejected", adminPingPath, withCompleteAgentHeaders, http.StatusUnauthorized},
		{"admin_no_auth", adminPingPath, nil, http.StatusUnauthorized},

		{"static_root_agent_headers_rejected", "/", withCompleteAgentHeaders, http.StatusUnauthorized},
		{"static_root_valid_admin_auth", "/", withValidAdminAuth, http.StatusOK},

		{"git_agent_headers_complete", gitPushPath, withCompleteAgentHeaders, http.StatusOK},
		{"git_admin_basic_auth_inert", gitPushPath, withValidAdminAuth, http.StatusForbidden},
		{"git_agent_headers_incomplete", gitPushPath, withIncompleteAgentHeaders, http.StatusForbidden},
		{"git_no_auth", gitPushPath, nil, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.setReq != nil {
				tc.setReq(req)
			}
			rec := httptest.NewRecorder()
			router.Handler().ServeHTTP(rec, req)
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

// TestRouter_Healthz_ReachableRegardlessOfAuthorizationHeader is the
// wave-4 brief's item 1, made concrete: a request with no Authorization
// header at all and one carrying a garbage Authorization header must both
// reach the handler with the identical 200 response -- proving the
// exemption is unconditional routing, not a middleware check that could be
// tricked by header content.
func TestRouter_Healthz_ReachableRegardlessOfAuthorizationHeader(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	noAuthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	noAuthRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(noAuthRec, noAuthReq)

	garbageAuthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	withGarbageAuthorization(garbageAuthReq)
	garbageAuthRec := httptest.NewRecorder()
	router.Handler().ServeHTTP(garbageAuthRec, garbageAuthReq)

	require.Equal(t, http.StatusOK, noAuthRec.Code)
	require.Equal(t, http.StatusOK, garbageAuthRec.Code)
	assert.Equal(t, noAuthRec.Body.String(), garbageAuthRec.Body.String())
	assert.Equal(t, "healthz-ok", noAuthRec.Body.String())
}

// TestRouter_SPAFallback_UnknownPathServesIndex proves an unrecognized
// non-API GET path (client-side route) falls back to index.html.
func TestRouter_SPAFallback_UnknownPathServesIndex(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/repos/123/proposals", nil)
	withValidAdminAuth(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "SPA-INDEX")
}

// TestRouter_SPAFallback_AssetPathServesRealFile proves a path matching a
// real embedded file is served verbatim, not the index.html fallback.
func TestRouter_SPAFallback_AssetPathServesRealFile(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	withValidAdminAuth(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "console.log('app')", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "SPA-INDEX")
}

// TestRouter_APINotFound_NotSwallowedBySPA proves an unrecognized
// procedure under a REGISTERED service subtree still gets that service's
// own 404 -- not the SPA's index.html -- because the mux's longest-prefix
// match routes it to the CLI subtree handler before the "/" SPA catch-all
// is ever considered.
func TestRouter_APINotFound_NotSwallowedBySPA(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/loam.v1.WorkBranchService/NoSuchMethod", nil)
	withValidAdminAuth(req)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "SPA-INDEX")
}

// TestRouter_RegisterCLI_WrongPathPrefixPanics proves the composition-root
// guard rail: RegisterCLI refuses a path outside /loam.v1.* rather than
// silently mounting it, so a copy-paste mistake wiring a handler under the
// wrong group fails at startup, not in production traffic.
func TestRouter_RegisterCLI_WrongPathPrefixPanics(t *testing.T) {
	t.Parallel()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword))
	assert.Panics(t, func() {
		router.RegisterCLI("/loam.admin.v1.RepoAdminService/", pingStub("x", "y"))
	})
}

// TestRouter_RegisterAdmin_WrongPathPrefixPanics is RegisterCLI's guard
// test, mirrored for RegisterAdmin.
func TestRouter_RegisterAdmin_WrongPathPrefixPanics(t *testing.T) {
	t.Parallel()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword))
	assert.Panics(t, func() {
		router.RegisterAdmin(cliPrefix, pingStub("x", "y"))
	})
}

// TestRouter_RegisterGit_WrongPathPrefixPanics is RegisterCLI's guard
// test, mirrored for RegisterGit.
func TestRouter_RegisterGit_WrongPathPrefixPanics(t *testing.T) {
	t.Parallel()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword))
	assert.Panics(t, func() {
		router.RegisterGit(cliPrefix, pingStub("x", "y"))
	})
}

// TestRouter_RegisterUnauthenticated_RejectsNonHealthPattern proves the
// health-endpoint allow-list: docs/server-spec.md calls /healthz and
// /readyz "the only such exemption", so registering anything else
// unauthenticated must fail loudly at startup rather than quietly widen
// the exemption.
func TestRouter_RegisterUnauthenticated_RejectsNonHealthPattern(t *testing.T) {
	t.Parallel()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword))
	assert.Panics(t, func() {
		router.RegisterUnauthenticated("/loam.v1.WorkBranchService/", pingStub("x", "y"))
	})
}

// TestRouter_RegisterSPA_MissingIndexPanics proves RegisterSPA fails fast
// on a filesystem missing index.html instead of shipping a router that
// 404s the SPA fallback on every request.
func TestRouter_RegisterSPA_MissingIndexPanics(t *testing.T) {
	t.Parallel()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword))
	assert.Panics(t, func() {
		router.RegisterSPA(fstest.MapFS{"assets/app.js": &fstest.MapFile{Data: []byte("x")}})
	})
}
