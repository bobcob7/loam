package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys mirrored from internal/server's unexported constants.
// Duplicated rather than exported because they are a WIRE contract with a
// trace backend, not a Go API: a query written against loam.agent.role must
// keep working, and a test that reads the constant it is checking cannot
// notice a rename.
const (
	attrCallerKind      = attribute.Key("loam.caller.kind")
	attrAgentName       = attribute.Key("loam.agent.name")
	attrAgentID         = attribute.Key("loam.agent.id")
	attrAgentRole       = attribute.Key("loam.agent.role")
	attrAgentIdentifier = attribute.Key("loam.agent.identifier")
)

// metaStub and repoStub are two REAL generated connect services, chosen so
// the fixture can vary the procedure across cases. A fixture with one
// service could not distinguish "the span name is read from the request"
// from "the span name is a constant" -- the same class of defect that let
// loam-p56y's sampler tests pass against a hardcoded AlwaysSample.
type metaStub struct {
	loamv1connect.UnimplementedMetaServiceHandler
}

func (metaStub) GetInstructions(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
	return connect.NewResponse(&loamv1.GetInstructionsResponse{}), nil
}

type repoStub struct {
	loamv1connect.UnimplementedRepoServiceHandler
}

func (repoStub) GetRepo(context.Context, *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error) {
	return connect.NewResponse(&loamv1.GetRepoResponse{}), nil
}

type roleStub struct {
	adminv1connect.UnimplementedRoleServiceHandler
}

func (roleStub) ListRoles(context.Context, *connect.Request[adminv1.ListRolesRequest]) (*connect.Response[adminv1.ListRolesResponse], error) {
	return connect.NewResponse(&adminv1.ListRolesResponse{}), nil
}

// newTracedRouter builds the REAL Router -- real internal/httpauth
// wrappers, real generated connect handlers, real otelconnect interceptor
// -- over an in-memory span recorder. Nothing here is a mock: a mocked
// interceptor would prove that a mock can be called, not that the wiring
// carries a span from the composition root to a handler.
func newTracedRouter(t *testing.T) (*server.Router, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	// context.WithoutCancel: t.Context() is already cancelled by the time
	// cleanups run, and a cancelled context makes Shutdown return
	// "context canceled" for every test in this file.
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.WithoutCancel(t.Context()))) })
	router := server.New(httpauth.New(testAdminUser, testAdminPassword), provider)
	router.RegisterCLI(loamv1connect.NewMetaServiceHandler(metaStub{}, router.RPCOptions()...))
	router.RegisterCLI(loamv1connect.NewRepoServiceHandler(repoStub{}, router.RPCOptions()...))
	router.RegisterAdmin(adminv1connect.NewRoleServiceHandler(roleStub{}, router.RPCOptions()...))
	router.RegisterUnauthenticated("/healthz", pingStub("/healthz", "healthz-ok"))
	router.RegisterUnauthenticated("/readyz", pingStub("/readyz", "readyz-ok"))
	return router, recorder
}

// attrs flattens one recorded span's attributes into a lookup, so a test
// can assert on ABSENCE as distinct from an empty value -- the whole point
// of the no-identity decision in tracing.go.
func attrs(span sdktrace.ReadOnlySpan) map[attribute.Key]string {
	out := make(map[attribute.Key]string, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		out[kv.Key] = kv.Value.Emit()
	}
	return out
}

func requireOneSpan(t *testing.T, recorder *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := recorder.Ended()
	require.Len(t, ended, 1, "expected exactly one RPC span")
	return ended[0]
}

// TestRPCSpan_NameIsProcedure_AttributesAreReadFromTheRequest is the
// assertion that the instrumentation is wired to the REQUEST rather than to
// constants. Every case varies BOTH the procedure and the identity, and
// asserts both, so a handler that hardcoded either would fail at least one
// case.
func TestRPCSpan_NameIsProcedure_AttributesAreReadFromTheRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		path          string
		agentName     string
		agentID       string
		agentRole     string
		wantSpanName  string
		wantService   string
		wantMethod    string
		wantIdentifie string
	}{
		{
			name:          "meta service, reviewer identity",
			path:          loamv1connect.MetaServiceGetInstructionsProcedure,
			agentName:     "ada",
			agentID:       "7",
			agentRole:     "reviewer",
			wantSpanName:  "loam.v1.MetaService/GetInstructions",
			wantService:   "loam.v1.MetaService",
			wantMethod:    "GetInstructions",
			wantIdentifie: "ada-7-reviewer",
		},
		{
			name:          "repo service, author identity",
			path:          loamv1connect.RepoServiceGetRepoProcedure,
			agentName:     "grace",
			agentID:       "12",
			agentRole:     "author",
			wantSpanName:  "loam.v1.RepoService/GetRepo",
			wantService:   "loam.v1.RepoService",
			wantMethod:    "GetRepo",
			wantIdentifie: "grace-12-author",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router, recorder := newTracedRouter(t)
			srv := httptest.NewServer(router.Handler())
			t.Cleanup(srv.Close)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+tt.path, jsonBody())
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(headerAgentName, tt.agentName)
			request.Header.Set(headerAgentID, tt.agentID)
			request.Header.Set(headerAgentRole, tt.agentRole)
			response, err := srv.Client().Do(request)
			require.NoError(t, err)
			t.Cleanup(func() { _ = response.Body.Close() })
			require.Equal(t, http.StatusOK, response.StatusCode)
			span := requireOneSpan(t, recorder)
			// The span NAME is the RPC procedure and nothing else --
			// identity lives in attributes so traces stay aggregatable.
			assert.Equal(t, tt.wantSpanName, span.Name())
			assert.Equal(t, trace.SpanKindServer, span.SpanKind())
			recorded := attrs(span)
			assert.Equal(t, tt.wantService, recorded["rpc.service"])
			assert.Equal(t, tt.wantMethod, recorded["rpc.method"])
			assert.Equal(t, "agent", recorded[attrCallerKind])
			assert.Equal(t, tt.agentName, recorded[attrAgentName])
			assert.Equal(t, tt.agentID, recorded[attrAgentID])
			assert.Equal(t, tt.agentRole, recorded[attrAgentRole])
			assert.Equal(t, tt.wantIdentifie, recorded[attrAgentIdentifier])
			// Nothing about the caller leaked into the span name.
			assert.NotContains(t, span.Name(), tt.agentName)
		})
	}
}

// TestRPCSpan_AdminCaller_RecordsKindWithoutAgentAttributes pins the other
// half of the no-identity decision: httpauth.AdminOnly sets an admin marker
// and NO agent identity, so the loam.agent.* keys must be ABSENT rather
// than present-and-empty, while loam.caller.kind still says positively
// which case this was.
func TestRPCSpan_AdminCaller_RecordsKindWithoutAgentAttributes(t *testing.T) {
	t.Parallel()
	router, recorder := newTracedRouter(t)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+adminv1connect.RoleServiceListRolesProcedure, jsonBody())
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(testAdminUser, testAdminPassword)
	response, err := srv.Client().Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusOK, response.StatusCode)
	span := requireOneSpan(t, recorder)
	assert.Equal(t, "loam.admin.v1.RoleService/ListRoles", span.Name())
	recorded := attrs(span)
	assert.Equal(t, "admin", recorded[attrCallerKind])
	assert.NotContains(t, recorded, attrAgentName)
	assert.NotContains(t, recorded, attrAgentID)
	assert.NotContains(t, recorded, attrAgentRole)
	assert.NotContains(t, recorded, attrAgentIdentifier)
}

// TestHealthEndpoints_ProduceNoSpan is the assertion this bead exists to
// make: /healthz and /readyz are polled on a liveness interval, and a span
// each would bury real traffic. The final leg is what keeps it from being
// vacuous -- it proves the same recorder, the same router and the same
// process DO record a span for a real RPC, so an empty recorder after the
// health probes means the exemption held, not that tracing was off.
func TestHealthEndpoints_ProduceNoSpan(t *testing.T) {
	t.Parallel()
	router, recorder := newTracedRouter(t)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	for _, path := range []string{"/healthz", "/readyz"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		response, err := srv.Client().Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, path)
	}
	require.Empty(t, recorder.Ended(), "health endpoints must produce no spans")
	rpc, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+loamv1connect.MetaServiceGetInstructionsProcedure, jsonBody())
	require.NoError(t, err)
	rpc.Header.Set("Content-Type", "application/json")
	rpc.Header.Set(headerAgentName, "ada")
	rpc.Header.Set(headerAgentID, "7")
	rpc.Header.Set(headerAgentRole, "reviewer")
	response, err := srv.Client().Do(rpc)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Len(t, recorder.Ended(), 1, "the same recorder does record a real RPC")
}

// TestRPCSpan_RejectedRequestProducesNoSpan records where the instrumented
// region begins. httpauth rejects a request with no complete identity
// BEFORE the connect handler, so the 401 never reaches an interceptor and
// leaves no span. That is a consequence of the interceptor living inside
// the auth wrapper, and it is worth pinning: if unauthenticated attempts
// ever need to be visible in traces, this test is the one that will fail
// and point at why.
func TestRPCSpan_RejectedRequestProducesNoSpan(t *testing.T) {
	t.Parallel()
	router, recorder := newTracedRouter(t)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+loamv1connect.MetaServiceGetInstructionsProcedure, jsonBody())
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(headerAgentName, "ada")
	response, err := srv.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	assert.Empty(t, recorder.Ended())
}

// TestRPCSpan_ErrorStatusIsRecorded proves the span carries the RPC's
// outcome and not just its name: a handler returning CodeUnimplemented (the
// generated Unimplemented* embed, reached here by calling a procedure the
// stub does not override) must end its span with an error status. An HTTP-
// level wrapper would have had to infer this from a status code; the
// connect interceptor reads the connect error directly.
func TestRPCSpan_ErrorStatusIsRecorded(t *testing.T) {
	t.Parallel()
	router, recorder := newTracedRouter(t)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+adminv1connect.RoleServiceGetRoleProcedure, jsonBody())
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(testAdminUser, testAdminPassword)
	response, err := srv.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	span := requireOneSpan(t, recorder)
	assert.Equal(t, "loam.admin.v1.RoleService/GetRole", span.Name())
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Equal(t, "admin", attrs(span)[attrCallerKind])
}

// TestRouter_NilTracerProvider_IsInert covers the degenerate wiring
// buildRouter's own tests use: a nil provider must not panic and must not
// record, so a test that only cares about routing need not thread telemetry
// through.
func TestRouter_NilTracerProvider_IsInert(t *testing.T) {
	t.Parallel()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword), nil)
	router.RegisterCLI(loamv1connect.NewMetaServiceHandler(metaStub{}, router.RPCOptions()...))
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+loamv1connect.MetaServiceGetInstructionsProcedure, jsonBody())
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(headerAgentName, "ada")
	request.Header.Set(headerAgentID, "7")
	request.Header.Set(headerAgentRole, "reviewer")
	response, err := srv.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

// TestRouter_RPCOptions_CallerCannotMutateTheRouter covers the defensive
// copy in RPCOptions: a caller that writes into the returned slice must not
// be changing what every handler registered afterwards is built with.
//
// Note the assertion is an ELEMENT OVERWRITE, not an append. An append
// cannot detect the missing copy here — the Router's slice has cap == len,
// so append reallocates whether or not RPCOptions cloned — and a test built
// on one would pass against a RPCOptions that returned rt.rpcOptions
// directly. Mutation-checked: replacing slices.Clone with a bare return
// fails this test.
func TestRouter_RPCOptions_CallerCannotMutateTheRouter(t *testing.T) {
	t.Parallel()
	router := server.New(httpauth.New(testAdminUser, testAdminPassword), nil)
	original := router.RPCOptions()
	require.NotEmpty(t, original)
	// The expected VALUE is copied out before the mutation. Holding the
	// slice and re-reading original[0] afterwards would read through the
	// same backing array the mutation wrote to, and the comparison would
	// hold either way.
	want := original[0]
	handed := router.RPCOptions()
	handed[0] = connect.WithCompressMinBytes(1)
	after := router.RPCOptions()
	require.Len(t, after, len(original))
	assert.Equal(t, want, after[0])
}

func jsonBody() *strings.Reader { return strings.NewReader("{}") }
