package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/httpauth"
)

// metaHandlerFunc adapts a plain function to loamv1connect.MetaServiceHandler
// (a single-method interface), so each test below can supply exactly the
// server behavior it needs without a named type per test.
type metaHandlerFunc func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error)

func (f metaHandlerFunc) GetInstructions(ctx context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
	return f(ctx, req)
}

// validConnectCfg builds a Config whose identity fields are all set and
// whose ServerURL points at serverURL — the shape every test below needs,
// varying only the handler behind serverURL.
func validConnectCfg(serverURL string) *ConfigMock {
	return &ConfigMock{
		ServerURLFunc:  func() string { return serverURL },
		AgentNameFunc:  func() string { return "grace-hopper" },
		AgentIDFunc:    func() string { return "3" },
		AgentRoleFunc:  func() string { return "author" },
		IdentifierFunc: func() string { return "grace-hopper-3-author" },
	}
}

// TestIdentityInterceptor_SetsAllThreeHeadersVerbatim proves the
// loam-0pj.6 acceptance criterion directly against the wire: every
// outbound request carries the three Loam-Agent-* headers, with the exact
// values from Config. It captures the request headers via a real Connect
// handler over httptest rather than mocking the transport, so this is a
// genuine end-to-end header-propagation check.
func TestIdentityInterceptor_SetsAllThreeHeadersVerbatim(t *testing.T) {
	t.Parallel()
	var got http.Header
	path, handler := loamv1connect.NewMetaServiceHandler(metaHandlerFunc(
		func(ctx context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			got = req.Header()
			return connect.NewResponse(&loamv1.GetInstructionsResponse{}), nil
		}))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := validConnectCfg(srv.URL)
	client, err := newConnectClient(cfg, srv.Client())
	require.NoError(t, err)
	_, err = client.Meta().GetInstructions(t.Context(), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err)
	require.NotNil(t, got, "the handler must have observed a request")
	assert.Equal(t, "grace-hopper", got.Get("Loam-Agent-Name"))
	assert.Equal(t, "3", got.Get("Loam-Agent-Id"))
	assert.Equal(t, "author", got.Get("Loam-Agent-Role"))
}

// TestIdentityInterceptor_HeaderNamesMatchHTTPAuthMiddleware is the
// integration proof the brief calls for by name: it routes a real Connect
// call through internal/httpauth's actual CLI() wrapper — not a
// hand-rolled header check — so a header-name mismatch between this
// package and internal/httpauth/middleware.go's agentIdentityFromHeaders
// (Loam-Agent-Name / -Id / -Role) fails here exactly as it would in
// production, instead of surfacing only at integration with a real server.
func TestIdentityInterceptor_HeaderNamesMatchHTTPAuthMiddleware(t *testing.T) {
	t.Parallel()
	path, handler := loamv1connect.NewMetaServiceHandler(metaHandlerFunc(
		func(ctx context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			identity, ok := httpauth.IdentityFromContext(ctx)
			if !ok {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no agent identity in context"))
			}
			return connect.NewResponse(&loamv1.GetInstructionsResponse{Usage: identity.Identifier()}), nil
		}))
	auth := httpauth.New("admin", "admin-password")
	mux := http.NewServeMux()
	mux.Handle(path, auth.CLI(handler))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := validConnectCfg(srv.URL)
	client, err := newConnectClient(cfg, srv.Client())
	require.NoError(t, err)
	resp, err := client.Meta().GetInstructions(t.Context(), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err)
	assert.Equal(t, "grace-hopper-3-author", resp.Msg.Usage)
}

// TestNewConnectClient_IncompleteAgentIdentity_FailsAtConstruction proves
// the brief's fail-closed requirement: a missing/empty LOAM_AGENT_* value
// must fail when the ConnectClient is built, as a clear usage error (exit
// 2), rather than silently producing a client whose requests carry a
// partial header set and get a confusing 401 from the server's fail-closed
// rule (loam-gcg).
func TestNewConnectClient_IncompleteAgentIdentity_FailsAtConstruction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *ConfigMock
	}{
		{"empty name", &ConfigMock{ServerURLFunc: func() string { return "https://loam.example" }, AgentNameFunc: func() string { return "" }, AgentIDFunc: func() string { return "3" }, AgentRoleFunc: func() string { return "author" }}},
		{"empty id", &ConfigMock{ServerURLFunc: func() string { return "https://loam.example" }, AgentNameFunc: func() string { return "grace-hopper" }, AgentIDFunc: func() string { return "" }, AgentRoleFunc: func() string { return "author" }}},
		{"empty role", &ConfigMock{ServerURLFunc: func() string { return "https://loam.example" }, AgentNameFunc: func() string { return "grace-hopper" }, AgentIDFunc: func() string { return "3" }, AgentRoleFunc: func() string { return "" }}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, err := newConnectClient(tt.cfg, http.DefaultClient)
			require.Error(t, err)
			assert.Nil(t, client)
			assert.ErrorIs(t, err, errUsage)
			assert.ErrorIs(t, err, errMissingEnv)
			mapper := newErrorMapper()
			assert.Equal(t, 2, mapper.ExitCode(err))
		})
	}
}

// TestConnectError_NotFound_MapsToExitThreeAndEncodesCleanly proves the
// other brief requirement: once the CLI is exercising real *connect.Error
// values (rather than the mocked ConnectClient used elsewhere in this
// package's tests), a command handler that forgets to map its RPC error
// still ends up correctly classified — CodeNotFound to exit 3 — with the
// error's own message encoded exactly once.
//
// This routes the raw *connect.Error through the real Run() (not a
// hand-built errorPayload): an earlier version of this test constructed
// errorPayload{Message: ce.Error()} itself and then asserted a property of
// the payload it had just built — which stayed green even when run.go
// independently used the wrong message (its own err.Error(), which for a
// *connect.Error prepends the code: "not_found: <message>", doubling the
// code already carried in errorDetail.Code). Driving the real Run() is
// what actually exercises the encoding path an agent's stdout depends on.
func TestConnectError_NotFound_MapsToExitThreeAndEncodesCleanly(t *testing.T) {
	t.Parallel()
	const notFoundMessage = "work branch wb-9c2f1a not found in repo acme/web"
	path, handler := loamv1connect.NewMetaServiceHandler(metaHandlerFunc(
		func(ctx context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New(notFoundMessage))
		}))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := validConnectCfg(srv.URL)
	client, err := newConnectClient(cfg, srv.Client())
	require.NoError(t, err)

	var buf bytes.Buffer
	encoder := newEncoder("json", &buf)
	deps := NewDeps(testLogger(), cfg, encoder, newErrorMapper(), &WorkspaceResolverMock{}, client)
	router := &Router{deps: deps, commands: map[string]*command{
		// This handler intentionally returns the raw RPC error unmapped —
		// exactly the "a handler forgot to map it" case mapCommandError
		// exists for (see errors.go's doc comment on mapCommandError).
		"boom": {run: func(ctx context.Context, d *Deps, args []string) error {
			_, callErr := d.connect.Meta().GetInstructions(ctx, connect.NewRequest(&loamv1.GetInstructionsRequest{}))
			return callErr
		}},
	}}

	exitCode := Run(t.Context(), router, []string{"boom"})
	assert.Equal(t, 3, exitCode, "CodeNotFound must map to exit 3")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	errObj, ok := decoded["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "not_found", errObj["code"])
	msg, _ := errObj["message"].(string)
	assert.Equal(t, notFoundMessage, msg, "message must be exactly the connect.Error's own message — not doubled with its own code prefix (connect.Error.Error() == \"not_found: ...\")")
	assert.Equal(t, 1, strings.Count(buf.String(), `"error"`), "the error object must be encoded exactly once, never doubled")
}
