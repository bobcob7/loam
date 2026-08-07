package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// --- shared test scaffolding ---

// graphTestDeps wires a Deps for a graph-subquery command test: client
// governs the GraphService responses, ws resolves the inferred repo when
// neither --repo nor --all is given, and encoded captures whatever the
// handler encodes on success.
func graphTestDeps(client GraphClient, ws WorkspaceResolver, encoded *any) *Deps {
	connectClient := &ConnectClientMock{GraphFunc: func() GraphClient { return client }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), &ConfigMock{}, encoder, newErrorMapper(), ws, connectClient, nil, nil, nil)
}

// resolvingWorkspace is a WorkspaceResolverMock whose ResolveRepo succeeds
// with repo -- the shape a graph command inside a clone with an omitted
// --repo/--all sees.
func resolvingWorkspace(repo string) *WorkspaceResolverMock {
	return &WorkspaceResolverMock{ResolveRepoFunc: func() (string, error) { return repo, nil }}
}

// --- graph def ---

func TestRunGraphDef_Success_EmitsDefShapeWithKindAndAmbiguityInfo(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.QueryRequest
	client := &GraphClientMock{
		QueryFunc: func(_ context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.QueryResponse{
				Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{Locations: []*loamv1.Location{
					{
						Repo:     "acme/repo",
						FileLine: &loamv1.FileLine{File: "auth.go", Line: uint32Ptr(42)},
						Symbol:   strPtr("Login"),
						Kind:     "function",
						Of:       &loamv1.MatchInfo{Symbol: "Login", File: "auth.go", Kind: "function"},
					},
				}}},
				Ingested:  []*loamv1.Ingested{{Repo: "acme/repo", Target: "main", Ref: "a1b2c3d", At: "2026-07-25T12:00:00Z"}},
				Truncated: false,
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo"})
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Equal(t, []string{"acme/repo"}, capturedReq.GetScope().GetRepos())
	def := capturedReq.GetDefinition()
	require.NotNil(t, def)
	assert.Equal(t, "Login", def.GetSymbol())

	out, ok := encoded.(graphQueryOutput)
	require.True(t, ok, "graph def must encode a graphQueryOutput")
	assert.False(t, out.Truncated)
	assert.Equal(t, []graphIngestedOutput{{Repo: "acme/repo", Target: "main", Ref: "a1b2c3d", At: "2026-07-25T12:00:00Z"}}, out.Ingested)
	rows, ok := out.Results.([]graphDefRow)
	require.True(t, ok, "graph def results must be []graphDefRow")
	require.Len(t, rows, 1)
	assert.Equal(t, "acme/repo", rows[0].Repo)
	assert.Equal(t, "auth.go", rows[0].File)
	assert.Equal(t, uint32(42), rows[0].Line)
	assert.Equal(t, "Login", rows[0].Symbol)
	assert.Equal(t, "function", rows[0].Kind)
	require.NotNil(t, rows[0].Of, "an ambiguous match must carry `of`")
	assert.Equal(t, graphMatchInfoOutput{Symbol: "Login", File: "auth.go", Kind: "function"}, *rows[0].Of)
}

func TestRunGraphDef_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("symbol Bogus is not defined"))
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Bogus", "--repo", "acme/repo"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

func TestRunGraphDef_FailedPrecondition_ExitsTwo(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("index not yet built"))
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- graph refs ---

// TestRunGraphRefs_EmptyResult_IsSuccessNotNotFound proves the subtlety
// this bead's prompt calls out explicitly: a real, defined symbol can have
// zero references, so an empty LocationList from a successful response
// must exit 0 with `results: []`, never be reinterpreted as not-found.
func TestRunGraphRefs_EmptyResult_IsSuccessNotNotFound(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return connect.NewResponse(&loamv1.QueryResponse{
				Result:    &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}},
				Truncated: false,
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphRefs(t.Context(), deps, []string{"Unused", "--repo", "acme/repo"})
	require.NoError(t, err)
	assert.Equal(t, 0, newErrorMapper().ExitCode(err))

	out, ok := encoded.(graphQueryOutput)
	require.True(t, ok)
	rows, ok := out.Results.([]graphRefRow)
	require.True(t, ok)
	assert.Empty(t, rows)
	assert.NotNil(t, rows, "results must encode as [] not null")
}

func TestRunGraphRefs_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("symbol Bogus is not defined"))
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphRefs(t.Context(), deps, []string{"Bogus", "--repo", "acme/repo"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// TestRunGraphRefs_RowShapeOmitsKind proves refs' row shape is distinct
// from def's: docs/cli-spec.md pins refs to `{ repo, file, line, symbol }`
// with no `kind`, even though the underlying proto Location always carries
// one. Marshaling and checking for the literal "kind" key (rather than just
// asserting on the Go struct) catches a mutation that reintroduces it via a
// shared/generic row type.
func TestRunGraphRefs_RowShapeOmitsKind(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return connect.NewResponse(&loamv1.QueryResponse{
				Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{Locations: []*loamv1.Location{
					{Repo: "acme/repo", FileLine: &loamv1.FileLine{File: "auth.go", Line: uint32Ptr(10)}, Symbol: strPtr("Login"), Kind: "function"},
				}}},
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphRefs(t.Context(), deps, []string{"Login", "--repo", "acme/repo"})
	require.NoError(t, err)
	out, ok := encoded.(graphQueryOutput)
	require.True(t, ok)
	rows, ok := out.Results.([]graphRefRow)
	require.True(t, ok)
	require.Len(t, rows, 1)
	encodedJSON := jsonEncodeForTest(t, rows[0])
	assert.NotContains(t, encodedJSON, `"kind"`, "refs' row shape must not carry a kind field")
}

// --- graph deps / dependents: to/from endpoint selection ---

func TestRunGraphDeps_UsesToEndpoint(t *testing.T) {
	t.Parallel()
	edge := &loamv1.DependencyEdge{
		From: &loamv1.Location{Repo: "acme/repo", Symbol: strPtr("Caller"), FileLine: &loamv1.FileLine{File: "caller.go", Line: uint32Ptr(5)}, Kind: "function"},
		To:   &loamv1.Location{Repo: "acme/repo", Symbol: strPtr("Callee"), FileLine: &loamv1.FileLine{File: "callee.go", Line: uint32Ptr(9)}, Kind: "function"},
	}
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return connect.NewResponse(&loamv1.QueryResponse{
				Result: &loamv1.QueryResponse_Dependencies{Dependencies: &loamv1.DependencyList{Edges: []*loamv1.DependencyEdge{edge}}},
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDeps(t.Context(), deps, []string{"Caller", "--repo", "acme/repo"})
	require.NoError(t, err)
	out, ok := encoded.(graphQueryOutput)
	require.True(t, ok)
	rows, ok := out.Results.([]graphDependencyRow)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, "Callee", rows[0].Symbol, "deps must report the depended-upon (\"to\") endpoint, not the dependent (\"from\") one")
	assert.Equal(t, "callee.go", rows[0].File)
}

func TestRunGraphDependents_UsesFromEndpoint(t *testing.T) {
	t.Parallel()
	edge := &loamv1.DependencyEdge{
		From: &loamv1.Location{Repo: "acme/repo", Symbol: strPtr("Caller"), FileLine: &loamv1.FileLine{File: "caller.go", Line: uint32Ptr(5)}, Kind: "function"},
		To:   &loamv1.Location{Repo: "acme/repo", Symbol: strPtr("Callee"), FileLine: &loamv1.FileLine{File: "callee.go", Line: uint32Ptr(9)}, Kind: "function"},
	}
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return connect.NewResponse(&loamv1.QueryResponse{
				Result: &loamv1.QueryResponse_Dependencies{Dependencies: &loamv1.DependencyList{Edges: []*loamv1.DependencyEdge{edge}}},
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDependents(t.Context(), deps, []string{"Callee", "--repo", "acme/repo"})
	require.NoError(t, err)
	out, ok := encoded.(graphQueryOutput)
	require.True(t, ok)
	rows, ok := out.Results.([]graphDependencyRow)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, "Caller", rows[0].Symbol, "dependents must report the dependent (\"from\") endpoint, not the depended-upon (\"to\") one")
	assert.Equal(t, "caller.go", rows[0].File)
}

// --- graph history ---

func TestRunGraphHistory_Success_EchoesQuerySymbol(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return connect.NewResponse(&loamv1.QueryResponse{
				Result: &loamv1.QueryResponse_History{History: &loamv1.HistoryList{Entries: []*loamv1.HistoryEntry{
					{Repo: "acme/repo", Commit: "a1b2c3d", Ref: "main", Message: "add login"},
				}}},
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphHistory(t.Context(), deps, []string{"Login", "--repo", "acme/repo"})
	require.NoError(t, err)
	out, ok := encoded.(graphQueryOutput)
	require.True(t, ok)
	rows, ok := out.Results.([]graphHistoryRow)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, "acme/repo", rows[0].Repo)
	assert.Equal(t, "Login", rows[0].Symbol)
	assert.Equal(t, "a1b2c3d", rows[0].Commit)
	assert.Equal(t, "main", rows[0].Ref)
	assert.Equal(t, "add login", rows[0].Message)
}

// --- shared: truncation, limit, file, scope ---

func TestRunGraphDef_Truncated_PropagatesTruncatedFlag(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return connect.NewResponse(&loamv1.QueryResponse{
				Result:    &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}},
				Truncated: true,
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo", "--limit", "1"})
	require.NoError(t, err)
	out, ok := encoded.(graphQueryOutput)
	require.True(t, ok)
	assert.True(t, out.Truncated, "a capped response must set truncated: true")
}

func TestRunGraphDef_Limit_PropagatesToPage(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.QueryRequest
	client := &GraphClientMock{
		QueryFunc: func(_ context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.QueryResponse{Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}}}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo", "--limit", "7"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, uint32(7), capturedReq.GetPage().GetLimit())
}

func TestRunGraphDef_DefaultLimit_Is50(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.QueryRequest
	client := &GraphClientMock{
		QueryFunc: func(_ context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.QueryResponse{Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}}}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, uint32(50), capturedReq.GetPage().GetLimit())
}

func TestRunGraphDef_NegativeLimit_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	called := false
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo", "--limit", "-1"})
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.False(t, called)
}

func TestRunGraphDef_FileFlag_NarrowsRequest(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.QueryRequest
	client := &GraphClientMock{
		QueryFunc: func(_ context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.QueryResponse{Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}}}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo", "--file", "auth.go"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	require.NotNil(t, capturedReq.File)
	assert.Equal(t, "auth.go", capturedReq.GetFile())
}

func TestRunGraphDef_NoFileFlag_LeavesFileUnset(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.QueryRequest
	client := &GraphClientMock{
		QueryFunc: func(_ context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.QueryResponse{Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}}}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Nil(t, capturedReq.File)
}

// --- scope resolution ---

func TestRunGraphDef_RepoAndAll_MutuallyExclusive_ExitsUsage(t *testing.T) {
	t.Parallel()
	called := false
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--repo", "acme/repo", "--all"})
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.False(t, called)
}

func TestRunGraphDef_All_SendsEmptyScope(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.QueryRequest
	client := &GraphClientMock{
		QueryFunc: func(_ context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.QueryResponse{Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}}}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login", "--all"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Empty(t, capturedReq.GetScope().GetRepos(), "--all must fan out via an empty scope, per docs/cli-spec.md")
}

func TestRunGraphDef_NoRepoNoAll_InfersFromWorkspace(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.QueryRequest
	client := &GraphClientMock{
		QueryFunc: func(_ context.Context, req *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.QueryResponse{Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}}}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, resolvingWorkspace("inferred/repo"), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, []string{"inferred/repo"}, capturedReq.GetScope().GetRepos())
}

func TestRunGraphDef_NoRepoNoAll_UnresolvableScope_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	called := false
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

	err := runGraphDef(t.Context(), deps, []string{"Login"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.False(t, called, "an unresolvable scope must not reach the server")
}

// --- argument validation ---

func TestRunGraphDef_WrongArgCount_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"no args", []string{"--repo", "acme/repo"}},
		{"two targets", []string{"Login", "Logout", "--repo", "acme/repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			client := &GraphClientMock{
				QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
					called = true
					return nil, errors.New("must not be called")
				},
			}
			var encoded any
			deps := graphTestDeps(client, noResolveWorkspace(), &encoded)

			err := runGraphDef(t.Context(), deps, tt.args)
			require.Error(t, err)
			var ue *usageError
			assert.ErrorAs(t, err, &ue)
			assert.False(t, called)
		})
	}
}

// --- router dispatch reachability ---

// TestRouterDispatch_GraphSubqueries_ReachRealHandlers proves the router
// reaches the real handlers for all five graph subqueries this bead
// implements (not the errNotImplemented stub, and not a routing
// usageError) -- the same shape
// TestRouterDispatch_WorkStartSetRequestReview_ReachRealHandlers proves for
// work start/set/request-review.
func TestRouterDispatch_GraphSubqueries_ReachRealHandlers(t *testing.T) {
	t.Parallel()
	client := &GraphClientMock{
		QueryFunc: func(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error) {
			return connect.NewResponse(&loamv1.QueryResponse{
				Result: &loamv1.QueryResponse_Locations{Locations: &loamv1.LocationList{}},
			}), nil
		},
	}
	var encoded any
	deps := graphTestDeps(client, noResolveWorkspace(), &encoded)
	router := NewRouter(deps)

	for _, args := range [][]string{
		{"graph", "def", "Login", "--repo", "acme/repo"},
		{"graph", "refs", "Login", "--repo", "acme/repo"},
		{"graph", "deps", "file.go", "--repo", "acme/repo"},
		{"graph", "dependents", "file.go", "--repo", "acme/repo"},
		{"graph", "history", "Login", "--repo", "acme/repo"},
	} {
		err := router.Dispatch(t.Context(), args)
		require.NoError(t, err, "args %v", args)
	}
}

// --- test helpers ---

func strPtr(s string) *string    { return &s }
func uint32Ptr(u uint32) *uint32 { return &u }

func jsonEncodeForTest(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := &jsonEncoder{w: &buf}
	require.NoError(t, enc.Encode(v))
	return buf.String()
}
