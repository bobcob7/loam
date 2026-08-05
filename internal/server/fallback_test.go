package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// authInjectingTransport wraps a RoundTripper, adding valid admin basic
// auth to every outgoing request. connect.NewClient has no per-call header
// hook, so a real client (used below to prove the wire format against
// connect-go itself, not just this package's own JSON assertions) needs
// its auth injected at the http.Client's Transport instead. base must be
// a private transport (newIsolatedTransport), never http.DefaultTransport
// -- this package's tests start and stop real httptest.Server instances,
// and http.DefaultTransport's process-global idle-connection pool lets a
// later test's request reuse a pooled connection to an already-closed
// server once the OS reissues its port, producing a bogus EOF or "http:
// server closed idle connection" failure unrelated to either test
// (loam-nk6).
type authInjectingTransport struct {
	base http.RoundTripper
}

func (t authInjectingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.SetBasicAuth(testAdminUser, testAdminPassword)
	return t.base.RoundTrip(r)
}

// newIsolatedTransport returns an *http.Transport cloned from
// http.DefaultTransport, with its own private idle-connection pool --
// see authInjectingTransport's doc comment for why this package's real,
// start/stop-a-server tests must never share http.DefaultTransport's.
func newIsolatedTransport(t *testing.T) *http.Transport {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

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
	router := server.New(auth, noop.NewTracerProvider())
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

// TestRouter_UnregisteredCLIServicePrefix_RealConnectClientDecodesNotFound
// closes the gap the other tests in this file leave open: they assert
// body["code"] == "not_found" on a bare map[string]any, which would also
// pass for an envelope shape connect-go itself rejects. This test drives
// a real connect.Client -- the same one a genuine CLI caller uses --
// against a live httptest.Server wrapping Router.Handler(), and checks
// that connect-go's own decoder recovers a *connect.Error with the
// expected code, proving the hand-built envelope in fallback.go is wire-
// compatible with connect-go v1.20.0, not merely JSON that looks similar.
func TestRouter_UnregisteredCLIServicePrefix_RealConnectClientDecodesNotFound(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: authInjectingTransport{base: newIsolatedTransport(t)}}
	client := connect.NewClient[loamv1.GetInstructionsRequest, loamv1.GetInstructionsResponse](
		hc, srv.URL+"/loam.v1.NoSuchService/DoThing")
	_, err := client.CallUnary(t.Context(), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

// wantConnectNotFoundMessage is the exact message connectNotFoundHandler
// builds for an unregistered /loam.v1.NoSuchService/DoThing request --
// shared by every case in TestConnectNotFoundHandler_AnswersInRequestProtocol
// below since all three protocols carry the identical underlying
// connect.Error, only encoded differently on the wire (loam-i0v).
const wantConnectNotFoundMessage = "no /loam.v1. service registered for /loam.v1.NoSuchService/DoThing"

// TestConnectNotFoundHandler_AnswersInRequestProtocol proves loam-i0v's
// fix: an unregistered service prefix now answers in whichever of the
// three protocols connect-go handlers serve by default -- Connect, gRPC,
// or gRPC-Web -- rather than always emitting a bare Connect-shaped 404,
// which is the wire format a real gRPC client used to misread as
// "unimplemented: HTTP status 404 Not Found", losing both the code and
// the message the bead reported.
//
// Every case's underlying error is CodeNotFound: on the Connect wire that
// is HTTP 404 (connectCodeToHTTP), and on gRPC/gRPC-Web the matching
// status is 5 (NOT_FOUND) -- chosen over Unimplemented (12) precisely
// because it is the code the Connect path already reported before this
// fix, and unifying on it keeps the reported code consistent across all
// three protocols instead of silently downgrading gRPC/gRPC-Web callers
// to the less specific Unimplemented.
func TestConnectNotFoundHandler_AnswersInRequestProtocol(t *testing.T) {
	t.Parallel()
	const unregisteredCLIPath = "/loam.v1.NoSuchService/DoThing"
	testCases := []struct {
		name        string
		method      string
		contentType string
		check       func(t *testing.T, rec *httptest.ResponseRecorder)
	}{
		{
			name:   "connect_unchanged_from_pre-fix_bytes",
			method: http.MethodGet,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusNotFound, rec.Code)
				assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
				wantBody := `{"code":"not_found","message":"` + wantConnectNotFoundMessage + `"}`
				assert.Equal(t, wantBody, rec.Body.String(), "the Connect wire envelope must stay byte-for-byte identical to the pre-loam-i0v hand-rolled body")
			},
		},
		{
			name:        "grpc",
			method:      http.MethodPost,
			contentType: "application/grpc",
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusOK, rec.Code, "gRPC errors are trailers-only: HTTP status must stay 200")
				assert.Equal(t, "5", rec.Header().Get("Grpc-Status"), "5 is NOT_FOUND, chosen to match the Connect path's CodeNotFound rather than downgrading to Unimplemented (12)")
				assert.Equal(t, wantConnectNotFoundMessage, rec.Header().Get("Grpc-Message"))
				assert.Empty(t, rec.Body.String(), "gRPC error responses carry no body, only trailers")
			},
		},
		{
			name:        "grpc_web",
			method:      http.MethodPost,
			contentType: "application/grpc-web",
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				assert.Equal(t, http.StatusOK, rec.Code, "gRPC-Web errors are trailers-only, same as gRPC")
				assert.Equal(t, "application/grpc-web", rec.Header().Get("Content-Type"), "gRPC-Web echoes the request's content type")
				assert.Equal(t, "5", rec.Header().Get("Grpc-Status"))
				assert.Equal(t, wantConnectNotFoundMessage, rec.Header().Get("Grpc-Message"))
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := newTestRouter(t)
			req := httptest.NewRequest(tc.method, unregisteredCLIPath, nil)
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			withValidAdminAuth(req)
			rec := httptest.NewRecorder()
			router.Handler().ServeHTTP(rec, req)
			tc.check(t, rec)
		})
	}
}
