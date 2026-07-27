package cli

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// searchTestDeps wires a Deps for a search command test: client governs the
// SearchService responses, ws resolves the inferred repo when neither
// --repo nor --all is given, and encoded captures whatever the handler
// encodes on success.
func searchTestDeps(client SearchClient, ws WorkspaceResolver, encoded *any) *Deps {
	connectClient := &ConnectClientMock{SearchFunc: func() SearchClient { return client }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), &ConfigMock{}, encoder, newErrorMapper(), ws, connectClient, nil, nil)
}

// --- success shape ---

func TestRunSearch_Success_EmitsSearchRowShapeWithIngestedAndTruncated(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.SearchRequest
	client := &SearchClientMock{
		SearchFunc: func(_ context.Context, req *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.SearchResponse{
				Results: []*loamv1.SearchResult{
					{Repo: "acme/repo", File: "auth.go", StartLine: 40, EndLine: 58, Score: 0.82, Snippet: "func Login() {"},
				},
				Ingested:  []*loamv1.Ingested{{Repo: "acme/repo", Target: "main", Ref: "a1b2c3d", At: "2026-07-25T12:00:00Z"}},
				Truncated: false,
			}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"how does auth work", "--repo", "acme/repo"})
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Equal(t, "how does auth work", capturedReq.GetQuery())
	assert.Equal(t, []string{"acme/repo"}, capturedReq.GetScope().GetRepos())

	out, ok := encoded.(searchOutput)
	require.True(t, ok, "search must encode a searchOutput")
	assert.False(t, out.Truncated)
	assert.Equal(t, []graphIngestedOutput{{Repo: "acme/repo", Target: "main", Ref: "a1b2c3d", At: "2026-07-25T12:00:00Z"}}, out.Ingested)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "acme/repo", out.Results[0].Repo)
	assert.Equal(t, "auth.go", out.Results[0].File)
	assert.Equal(t, []uint32{40, 58}, out.Results[0].Lines)
	assert.InDelta(t, float32(0.82), out.Results[0].Score, 0.0001)
	assert.Equal(t, "func Login() {", out.Results[0].Snippet)
}

// TestRunSearch_EmptyResult_IsSuccessNotNotFound mirrors
// TestRunGraphRefs_EmptyResult_IsSuccessNotNotFound: a successful response
// with no matching chunks must exit 0 with `results: []`, never be
// reinterpreted as failure.
func TestRunSearch_EmptyResult_IsSuccessNotNotFound(t *testing.T) {
	t.Parallel()
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			return connect.NewResponse(&loamv1.SearchResponse{}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"nothing matches this", "--repo", "acme/repo"})
	require.NoError(t, err)
	assert.Equal(t, 0, newErrorMapper().ExitCode(err))

	out, ok := encoded.(searchOutput)
	require.True(t, ok)
	assert.Empty(t, out.Results)
	assert.NotNil(t, out.Results, "results must encode as [] not null")
}

// --- limit ---

func TestRunSearch_Limit_PropagatesToPage(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.SearchRequest
	client := &SearchClientMock{
		SearchFunc: func(_ context.Context, req *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.SearchResponse{}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--repo", "acme/repo", "--limit", "3"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, uint32(3), capturedReq.GetPage().GetLimit())
}

func TestRunSearch_DefaultLimit_Is10(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.SearchRequest
	client := &SearchClientMock{
		SearchFunc: func(_ context.Context, req *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.SearchResponse{}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--repo", "acme/repo"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, uint32(10), capturedReq.GetPage().GetLimit())
}

func TestRunSearch_NegativeLimit_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	called := false
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--repo", "acme/repo", "--limit", "-1"})
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.False(t, called)
}

func TestRunSearch_Truncated_PropagatesTruncatedFlag(t *testing.T) {
	t.Parallel()
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			return connect.NewResponse(&loamv1.SearchResponse{Truncated: true}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--repo", "acme/repo", "--limit", "1"})
	require.NoError(t, err)
	out, ok := encoded.(searchOutput)
	require.True(t, ok)
	assert.True(t, out.Truncated, "a capped response must set truncated: true")
}

// --- scope resolution ---

func TestRunSearch_RepoAndAll_MutuallyExclusive_ExitsUsage(t *testing.T) {
	t.Parallel()
	called := false
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--repo", "acme/repo", "--all"})
	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.False(t, called)
}

func TestRunSearch_All_SendsEmptyScope(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.SearchRequest
	client := &SearchClientMock{
		SearchFunc: func(_ context.Context, req *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.SearchResponse{}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--all"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Empty(t, capturedReq.GetScope().GetRepos(), "--all must fan out via an empty scope, per docs/cli-spec.md")
}

func TestRunSearch_NoRepoNoAll_InfersFromWorkspace(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.SearchRequest
	client := &SearchClientMock{
		SearchFunc: func(_ context.Context, req *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.SearchResponse{}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, resolvingWorkspace("inferred/repo"), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, []string{"inferred/repo"}, capturedReq.GetScope().GetRepos())
}

func TestRunSearch_NoRepoNoAll_UnresolvableScope_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	called := false
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.False(t, called, "an unresolvable scope must not reach the server")
}

// --- argument validation ---

func TestRunSearch_WrongArgCount_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"no args", []string{"--repo", "acme/repo"}},
		{"two queries", []string{"auth", "login", "--repo", "acme/repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			client := &SearchClientMock{
				SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
					called = true
					return nil, errors.New("must not be called")
				},
			}
			var encoded any
			deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

			err := runSearch(t.Context(), deps, tt.args)
			require.Error(t, err)
			var ue *usageError
			assert.ErrorAs(t, err, &ue)
			assert.False(t, called)
		})
	}
}

// --- error mapping ---

func TestRunSearch_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("repo acme/bogus is not enrolled"))
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--repo", "acme/repo"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// TestRunSearch_InvalidArgument_ExitsTwo proves an unenrolled-repo scope --
// the server-side CodeInvalidArgument case this bead's brief calls out
// explicitly, distinct from CodeNotFound -- maps to exit 2, not exit 3.
func TestRunSearch_InvalidArgument_ExitsTwo(t *testing.T) {
	t.Parallel()
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("empty query"))
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)

	err := runSearch(t.Context(), deps, []string{"auth", "--repo", "acme/repo"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- router dispatch reachability ---

// TestRouterDispatch_Search_ReachesRealHandler proves the router reaches
// the real search handler (not the errNotImplemented stub, and not a
// routing usageError) -- the same shape
// TestRouterDispatch_GraphSubqueries_ReachRealHandlers proves for the graph
// subqueries. Named in stillStubbedExemptions (router_test.go) as the test
// that proves search's coverage.
func TestRouterDispatch_Search_ReachesRealHandler(t *testing.T) {
	t.Parallel()
	client := &SearchClientMock{
		SearchFunc: func(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error) {
			return connect.NewResponse(&loamv1.SearchResponse{}), nil
		},
	}
	var encoded any
	deps := searchTestDeps(client, noResolveWorkspace(), &encoded)
	router := NewRouter(deps)

	err := router.Dispatch(t.Context(), []string{"search", "how does auth work", "--repo", "acme/repo"})
	require.NoError(t, err)
}
