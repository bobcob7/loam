package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// testAuthor is the agent replying in these tests — deliberately NOT
// testReviewer, since `reply` is the author's half of the round trip.
const testAuthor = "grace-hopper-3-author"

// --- scaffolding ---

// replyServer stands in for the server behind `work reply`. It records the
// request ReplyToThread received, and fails loudly on any OTHER rpc: reply
// is a single immediate call, and a second round trip here would be a
// pre-flight check the server already performs.
type replyServer struct {
	client   *WorkBranchClientMock
	requests []*loamv1.ReplyToThreadRequest
	other    []string
}

// newReplyServer builds a server that accepts a reply and echoes it back as
// the server would: attributed to the replying agent, stamped with a round.
func newReplyServer() *replyServer {
	s := &replyServer{}
	s.client = &WorkBranchClientMock{
		ReplyToThreadFunc: func(_ context.Context, req *connect.Request[loamv1.ReplyToThreadRequest]) (*connect.Response[loamv1.ReplyToThreadResponse], error) {
			s.requests = append(s.requests, req.Msg)
			return connect.NewResponse(&loamv1.ReplyToThreadResponse{
				Comment: &loamv1.Comment{Author: testAuthor, Body: req.Msg.GetBody(), Round: 2},
			}), nil
		},
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			s.other = append(s.other, "GetWorkBranch")
			return nil, errors.New("reply must not pre-check the work branch")
		},
		ListCommentsFunc: func(context.Context, *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error) {
			s.other = append(s.other, "ListComments")
			return nil, errors.New("reply must not pre-check the thread")
		},
		SubmitVerdictFunc: func(context.Context, *connect.Request[loamv1.SubmitVerdictRequest]) (*connect.Response[loamv1.SubmitVerdictResponse], error) {
			s.other = append(s.other, "SubmitVerdict")
			return nil, errors.New("reply must not cast a verdict")
		},
	}
	return s
}

// rejectingReplyServer builds a server whose ReplyToThread fails with the
// given Connect status — the shape a missing work branch, a missing thread,
// or a terminal-state rejection arrives in.
func rejectingReplyServer(code connect.Code, message string) *replyServer {
	s := newReplyServer()
	s.client.ReplyToThreadFunc = func(_ context.Context, req *connect.Request[loamv1.ReplyToThreadRequest]) (*connect.Response[loamv1.ReplyToThreadResponse], error) {
		s.requests = append(s.requests, req.Msg)
		return nil, connect.NewError(code, errors.New(message))
	}
	return s
}

// replyWorkspace is a workspace resolver that infers the shared test
// identity and RECORDS any attempt to open a staging area, so a test can
// prove `reply` never stages.
func replyWorkspace() *WorkspaceResolverMock {
	return &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return testRepo, nil },
		ResolveWorkBranchFunc: func() (string, error) { return testWorkBranch, nil },
		OpenStagingFunc: func(string, string) (StagingArea, error) {
			return nil, errors.New("reply must not open a staging area")
		},
	}
}

// replyDeps wires a Deps for `work reply` around srv and ws.
func replyDeps(srv *replyServer, ws WorkspaceResolver, stdin string, encoded *any) *Deps {
	cfg := &ConfigMock{IdentifierFunc: func() string { return testAuthor }}
	connectClient := &ConnectClientMock{WorkBranchFunc: func() WorkBranchClient { return srv.client }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), cfg, encoder, newErrorMapper(), ws, connectClient, nil, strings.NewReader(stdin))
}

// runReply runs one `work reply` invocation as a fresh process would.
func runReply(t *testing.T, srv *replyServer, ws WorkspaceResolver, stdin string, args ...string) (any, error) {
	t.Helper()
	var encoded any
	err := runWorkReply(t.Context(), replyDeps(srv, ws, stdin, &encoded), args)
	return encoded, err
}

// replyArgs is the positional pair plus --thread every test below passes
// unless it is specifically exercising inference or a missing flag.
func replyArgs(thread string, rest ...string) []string {
	return append([]string{testRepo, testWorkBranch, "--thread", thread}, rest...)
}

// --- immediate posting ---

// TestRunWorkReply_PostsImmediatelyAndReportsThePostedComment is
// replies.feature's "An author replies to a thread immediately": one rpc,
// carrying the whole reply, and the posted comment reported back.
func TestRunWorkReply_PostsImmediatelyAndReportsThePostedComment(t *testing.T) {
	t.Parallel()
	srv := newReplyServer()
	encoded, err := runReply(t, srv, replyWorkspace(), "fixed in the next push\n", replyArgs("t1")...)
	require.NoError(t, err)
	require.Len(t, srv.requests, 1, "reply must reach the server in exactly one call")
	assert.Equal(t, testRepo, srv.requests[0].GetRepo())
	assert.Equal(t, testWorkBranch, srv.requests[0].GetWorkBranch())
	assert.Equal(t, "t1", srv.requests[0].GetThreadId())
	assert.Equal(t, "fixed in the next push", srv.requests[0].GetBody())
	assert.Equal(t, replyOutput{Author: testAuthor, Body: "fixed in the next push"}, encoded)
}

// TestRunWorkReply_IsNeverStaged is replies.feature's "it was not staged".
// The command has no staging area at all: OpenStaging is never called, so a
// reply cannot be sitting locally waiting for a verdict to publish it.
func TestRunWorkReply_IsNeverStaged(t *testing.T) {
	t.Parallel()
	srv := newReplyServer()
	ws := replyWorkspace()
	_, err := runReply(t, srv, ws, "a reply", replyArgs("t1")...)
	require.NoError(t, err)
	assert.Empty(t, ws.OpenStagingCalls(), "reply must never touch the staging area")
	assert.Empty(t, srv.other, "reply must make no rpc other than ReplyToThread")
}

// TestRunWorkReply_OutputShape pins the documented JSON (docs/cli-spec.md ->
// reply): author and body only. The server stamps a round on the comment and
// it must not leak into the output shape the spec fixes.
func TestRunWorkReply_OutputShape(t *testing.T) {
	t.Parallel()
	encoded, err := runReply(t, newReplyServer(), replyWorkspace(), "a reply", replyArgs("t1")...)
	require.NoError(t, err)
	raw, err := json.Marshal(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, `{"author":"grace-hopper-3-author","body":"a reply"}`, string(raw))
}

// TestRunWorkReply_InfersIdentityFromWorkspaceWhenPositionalsOmitted proves
// the [repo] [work-branch] convention holds for reply too.
func TestRunWorkReply_InfersIdentityFromWorkspaceWhenPositionalsOmitted(t *testing.T) {
	t.Parallel()
	srv := newReplyServer()
	_, err := runReply(t, srv, replyWorkspace(), "an inferred reply", "--thread", "t1")
	require.NoError(t, err)
	require.Len(t, srv.requests, 1)
	assert.Equal(t, testRepo, srv.requests[0].GetRepo())
	assert.Equal(t, testWorkBranch, srv.requests[0].GetWorkBranch())
}

// --- usage errors (exit 2), decided before any rpc ---

func TestRunWorkReply_UsageErrors_ExitTwoWithoutCallingServer(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		stdin string
		args  []string
	}{
		"no --thread":                   {"a reply", []string{testRepo, testWorkBranch}},
		"empty --thread":                {"a reply", replyArgs("")},
		"no body on stdin":              {"", replyArgs("t1")},
		"blank body on stdin":           {"\n  \n", replyArgs("t1")},
		"too many positional arguments": {"a reply", []string{testRepo, testWorkBranch, "extra", "--thread", "t1"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newReplyServer()
			encoded, err := runReply(t, srv, replyWorkspace(), tt.stdin, tt.args...)
			require.Error(t, err)
			assert.Equal(t, 2, newErrorMapper().ExitCode(err))
			assert.Nil(t, encoded, "a rejected invocation must encode nothing")
			assert.Empty(t, srv.requests, "a usage error must be decided before any rpc")
		})
	}
}

// --- server-side rejections ---

// TestRunWorkReply_MissingThread_ExitsThree is replies.feature's "Replying
// to a missing thread is rejected". The server owns that lookup; the CLI's
// job is to carry its NotFound through unreclassified.
func TestRunWorkReply_MissingThread_ExitsThree(t *testing.T) {
	t.Parallel()
	srv := rejectingReplyServer(connect.CodeNotFound, "thread t404 does not exist")
	encoded, err := runReply(t, srv, replyWorkspace(), "a reply", replyArgs("t404")...)
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codeNotFound, ce.code)
	assert.Nil(t, encoded)
}

// TestRunWorkReply_TerminalWorkBranch_ExitsTwoAsPreconditionFailed is
// replies.feature's "Replying on a completed work branch is rejected". It
// must be exit 2 / precondition_failed, NOT the not_found an "identifier
// could not be resolved" reading would produce (docs/cli-spec.md -> State
// gates: reply is rejected in complete/closed).
func TestRunWorkReply_TerminalWorkBranch_ExitsTwoAsPreconditionFailed(t *testing.T) {
	t.Parallel()
	srv := rejectingReplyServer(connect.CodeFailedPrecondition, "work branch is complete, a terminal state")
	encoded, err := runReply(t, srv, replyWorkspace(), "a reply", replyArgs("t1")...)
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codePreconditionFailed, ce.code)
	assert.Nil(t, encoded)
}

// --- dispatch ---

// TestRouterDispatch_WorkReply_ReachesRealHandler proves `loam work reply`
// reaches this bead's handler rather than the errNotImplemented stub it
// replaced.
func TestRouterDispatch_WorkReply_ReachesRealHandler(t *testing.T) {
	t.Parallel()
	var encoded any
	deps := replyDeps(newReplyServer(), replyWorkspace(), "a routed reply", &encoded)
	err := NewRouter(deps).Dispatch(t.Context(), []string{"work", "reply", testRepo, testWorkBranch, "--thread", "t1"})
	require.NoError(t, err)
	assert.Equal(t, replyOutput{Author: testAuthor, Body: "a routed reply"}, encoded)
}
