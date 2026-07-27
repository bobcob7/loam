package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// --- shared test scaffolding ---

// workTestDeps wires a Deps for a work-lifecycle command test: workBranch
// governs the WorkBranchService responses, ws resolves repo/work-branch
// identity when a command omits them, stdin feeds the optional description
// (`work set`), and encoded captures whatever the handler encodes on
// success.
func workTestDeps(workBranch WorkBranchClient, ws WorkspaceResolver, stdin string, encoded *any) *Deps {
	connectClient := &ConnectClientMock{WorkBranchFunc: func() WorkBranchClient { return workBranch }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), &ConfigMock{}, encoder, newErrorMapper(), ws, connectClient, nil, strings.NewReader(stdin))
}

// noResolveWorkspace is a WorkspaceResolverMock whose ResolveRepo/
// ResolveWorkBranch both fail -- the shape a command outside any clone with
// omitted positionals sees. Tests that always pass explicit positionals use
// this so an accidental fallback to inference would fail loudly instead of
// silently resolving to a zero value.
func noResolveWorkspace() *WorkspaceResolverMock {
	return &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "", errors.New("not inside a repo directory") },
		ResolveWorkBranchFunc: func() (string, error) { return "", errors.New("not inside a repo directory") },
	}
}

// --- work start ---

func TestRunWorkStart_Success(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.CreateWorkBranchRequest
	client := &WorkBranchClientMock{
		CreateWorkBranchFunc: func(_ context.Context, req *connect.Request[loamv1.CreateWorkBranchRequest]) (*connect.Response[loamv1.CreateWorkBranchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.CreateWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: "acme/repo", Name: "wb-9c2f1a", Target: "main", State: loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT,
			}}), nil
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkStart(t.Context(), deps, []string{"acme/repo", "main"})
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Equal(t, "acme/repo", capturedReq.GetRepo())
	assert.Equal(t, "main", capturedReq.GetFrom())

	out, ok := encoded.(workStartOutput)
	require.True(t, ok, "work start must encode a workStartOutput")
	assert.Equal(t, workStartOutput{Repo: "acme/repo", Name: "wb-9c2f1a", Target: "main", State: "draft"}, out)
}

func TestRunWorkStart_MissingArgs_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"one arg", []string{"acme/repo"}},
		{"three args", []string{"acme/repo", "main", "extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			client := &WorkBranchClientMock{
				CreateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.CreateWorkBranchRequest]) (*connect.Response[loamv1.CreateWorkBranchResponse], error) {
					called = true
					return nil, errors.New("must not be called")
				},
			}
			var encoded any
			deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

			err := runWorkStart(t.Context(), deps, tt.args)
			require.Error(t, err)
			var ue *usageError
			assert.ErrorAs(t, err, &ue)
			assert.False(t, called, "work start must validate argument count before calling the server")
		})
	}
}

func TestRunWorkStart_RepoNotEnrolled_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		CreateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.CreateWorkBranchRequest]) (*connect.Response[loamv1.CreateWorkBranchResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("repo acme/repo is not enrolled"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkStart(t.Context(), deps, []string{"acme/repo", "main"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

func TestRunWorkStart_InvalidFrom_ExitsTwo(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		CreateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.CreateWorkBranchRequest]) (*connect.Response[loamv1.CreateWorkBranchResponse], error) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("bogus is not a valid target branch for repo acme/repo"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkStart(t.Context(), deps, []string{"acme/repo", "bogus"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- work set ---

func TestRunWorkSet_TitleOnly_Success(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.UpdateWorkBranchRequest
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(_ context.Context, req *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.UpdateWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: "acme/repo", Name: "wb-1", Target: "main", Title: "Add login", State: loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT,
			}}), nil
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"acme/repo", "wb-1", "--title", "Add login"})
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Equal(t, "acme/repo", capturedReq.GetRepo())
	assert.Equal(t, "wb-1", capturedReq.GetWorkBranch())
	require.NotNil(t, capturedReq.Title)
	assert.Equal(t, "Add login", capturedReq.GetTitle())
	assert.Nil(t, capturedReq.Description, "description must stay unset when stdin is empty")

	out, ok := encoded.(workBranchOutput)
	require.True(t, ok, "work set must encode a workBranchOutput")
	assert.Equal(t, workBranchOutput{Repo: "acme/repo", Name: "wb-1", Target: "main", Title: "Add login", State: "draft"}, out)
}

func TestRunWorkSet_DescriptionOnly_Success(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.UpdateWorkBranchRequest
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(_ context.Context, req *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.UpdateWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: "acme/repo", Name: "wb-1", Target: "main", Title: "Existing title", State: loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT,
			}}), nil
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "a new description\n", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Nil(t, capturedReq.Title, "title must stay unset when --title is omitted")
	require.NotNil(t, capturedReq.Description)
	assert.Equal(t, "a new description", capturedReq.GetDescription(), "a trailing newline from stdin must be trimmed")
}

func TestRunWorkSet_NeitherTitleNorStdin_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	called := false
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.False(t, called, "work set must reject before calling UpdateWorkBranch when neither --title nor stdin is provided")
	assert.Nil(t, encoded)
}

// TestRunWorkSet_BlankStdinOnly_ExitsUsage proves a lone newline (what
// `echo` without `-n` pipes for an intentionally empty description) still
// counts as "no description provided", not as a non-empty one.
func TestRunWorkSet_BlankStdinOnly_ExitsUsage(t *testing.T) {
	t.Parallel()
	called := false
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "\n", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.False(t, called)
}

func TestRunWorkSet_ResolvesIdentityFromWorkspaceWhenPositionalsOmitted(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.UpdateWorkBranchRequest
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(_ context.Context, req *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.UpdateWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{Repo: "inferred/repo", Name: "wb-inferred"}}), nil
		},
	}
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "inferred/repo", nil },
		ResolveWorkBranchFunc: func() (string, error) { return "wb-inferred", nil },
	}
	var encoded any
	deps := workTestDeps(client, ws, "", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"--title", "T"})
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, "inferred/repo", capturedReq.GetRepo())
	assert.Equal(t, "wb-inferred", capturedReq.GetWorkBranch())
}

func TestRunWorkSet_CannotResolveIdentity_ExitsUsageWithoutCallingServer(t *testing.T) {
	t.Parallel()
	called := false
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			called = true
			return nil, errors.New("must not be called")
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"--title", "T"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.False(t, called)
}

// TestRunWorkSet_TerminalState_ExitsTwoPrecondition proves the state-gate
// table (docs/cli-spec.md -> State gates: `set` rejects `complete`,
// `closed`) surfaces as exit 2 precondition_failed -- the server enforces
// the gate (internal/workbranchstore.UpdateState / SetTitleDescription),
// this CLI command only needs to classify the resulting *connect.Error
// correctly.
func TestRunWorkSet_TerminalState_ExitsTwoPrecondition(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("work branch is closed, a terminal state"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"acme/repo", "wb-1", "--title", "T"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codePreconditionFailed, ce.code)
	assert.Nil(t, encoded)
}

func TestRunWorkSet_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		UpdateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("work branch acme/repo/wb-1 not found"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkSet(t.Context(), deps, []string{"acme/repo", "wb-1", "--title", "T"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- work request-review ---

func TestRunWorkRequestReview_Success(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.RequestReviewRequest
	client := &WorkBranchClientMock{
		RequestReviewFunc: func(_ context.Context, req *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.RequestReviewResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: "acme/repo", Name: "wb-1", Target: "main", Title: "Add login", State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE,
			}}), nil
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkRequestReview(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Equal(t, "acme/repo", capturedReq.GetRepo())
	assert.Equal(t, "wb-1", capturedReq.GetWorkBranch())

	out, ok := encoded.(workBranchOutput)
	require.True(t, ok, "work request-review must encode a workBranchOutput")
	assert.Equal(t, workBranchOutput{Repo: "acme/repo", Name: "wb-1", Target: "main", Title: "Add login", State: "reviewable"}, out)
}

func TestRunWorkRequestReview_ResolvesIdentityFromWorkspaceWhenPositionalsOmitted(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.RequestReviewRequest
	client := &WorkBranchClientMock{
		RequestReviewFunc: func(_ context.Context, req *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.RequestReviewResponse{WorkBranch: &loamv1.WorkBranch{Repo: "inferred/repo", Name: "wb-inferred"}}), nil
		},
	}
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return "inferred/repo", nil },
		ResolveWorkBranchFunc: func() (string, error) { return "wb-inferred", nil },
	}
	var encoded any
	deps := workTestDeps(client, ws, "", &encoded)

	err := runWorkRequestReview(t.Context(), deps, nil)
	require.NoError(t, err)
	require.NotNil(t, capturedReq)
	assert.Equal(t, "inferred/repo", capturedReq.GetRepo())
	assert.Equal(t, "wb-inferred", capturedReq.GetWorkBranch())
}

// TestRunWorkRequestReview_AlreadyReviewable_ExitsTwoPrecondition is this
// bead's specific Definition of Done requirement (see loam-0pj.11's DESIGN
// note): docs/cli-spec.md's State gates table lists `reviewable` itself as
// a rejected state for `request-review` -- re-requesting review on a
// branch that already has an open round is a precondition failure, not a
// silent success, even though the Errors prose alone only mentions
// "terminal state".
func TestRunWorkRequestReview_AlreadyReviewable_ExitsTwoPrecondition(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		RequestReviewFunc: func(context.Context, *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("work branch is already reviewable with an open review round"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkRequestReview(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codePreconditionFailed, ce.code)
	assert.Contains(t, ce.Error(), "already reviewable")
	assert.Nil(t, encoded)
}

func TestRunWorkRequestReview_MissingTitleOrDescription_ExitsTwoPrecondition(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		RequestReviewFunc: func(context.Context, *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("work branch has no title or description set"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkRequestReview(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

func TestRunWorkRequestReview_TerminalState_ExitsTwoPrecondition(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		RequestReviewFunc: func(context.Context, *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("work branch is closed, a terminal state"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkRequestReview(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

func TestRunWorkRequestReview_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		RequestReviewFunc: func(context.Context, *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("work branch acme/repo/wb-1 not found"))
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)

	err := runWorkRequestReview(t.Context(), deps, []string{"acme/repo", "wb-1"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- pure helpers ---

func TestWorkBranchStateString_MapsAllKnownStates(t *testing.T) {
	t.Parallel()
	cases := map[loamv1.WorkBranchState]string{
		loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT:       "draft",
		loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE:  "reviewable",
		loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED:    "reviewed",
		loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE:    "complete",
		loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED:      "closed",
		loamv1.WorkBranchState_WORK_BRANCH_STATE_UNSPECIFIED: "unspecified",
	}
	for state, want := range cases {
		assert.Equal(t, want, workBranchStateString(state), "state %v", state)
	}
}

func TestReadStdin_TrimsSurroundingWhitespace(t *testing.T) {
	t.Parallel()
	got, err := readStdin(strings.NewReader("  a description\n"))
	require.NoError(t, err)
	assert.Equal(t, "a description", got)
}

func TestReadStdin_EmptyReaderReturnsEmptyString(t *testing.T) {
	t.Parallel()
	got, err := readStdin(strings.NewReader(""))
	require.NoError(t, err)
	assert.Empty(t, got)
}

// --- router dispatch reachability ---

// TestRouterDispatch_WorkStartSetRequestReview_ReachRealHandlers proves the
// router reaches the real handlers for all three commands this bead
// implements (not the errNotImplemented stub, and not a routing
// usageError) -- the same shape TestRouterDispatch_Clone_ReachesRealHandler
// proves for clone.
func TestRouterDispatch_WorkStartSetRequestReview_ReachRealHandlers(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		CreateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.CreateWorkBranchRequest]) (*connect.Response[loamv1.CreateWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.CreateWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{Repo: "acme/repo", Name: "wb-1", Target: "main"}}), nil
		},
		UpdateWorkBranchFunc: func(context.Context, *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.UpdateWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{Repo: "acme/repo", Name: "wb-1"}}), nil
		},
		RequestReviewFunc: func(context.Context, *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
			return connect.NewResponse(&loamv1.RequestReviewResponse{WorkBranch: &loamv1.WorkBranch{Repo: "acme/repo", Name: "wb-1"}}), nil
		},
	}
	var encoded any
	deps := workTestDeps(client, noResolveWorkspace(), "", &encoded)
	router := NewRouter(deps)

	for _, args := range [][]string{
		{"work", "start", "acme/repo", "main"},
		{"work", "set", "acme/repo", "wb-1", "--title", "T"},
		{"work", "request-review", "acme/repo", "wb-1"},
	} {
		err := router.Dispatch(t.Context(), args)
		require.NoError(t, err, "args %v", args)
	}
}
