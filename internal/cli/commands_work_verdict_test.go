package cli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// --- scaffolding ---

// verdictServer stands in for the server behind `work verdict`. It records
// every SubmitVerdict request, and fails loudly on any OTHER rpc: publishing
// is one atomic call, so a pre-flight existence or state check here would be
// both duplicated and raceable against the transaction that decides.
type verdictServer struct {
	client   *WorkBranchClientMock
	requests []*loamv1.SubmitVerdictRequest
	other    []string
}

// newVerdictServer builds a server that accepts a verdict and echoes back
// the outcome it was sent plus a published count equal to the number of
// comments in the batch — what the real handler reports.
func newVerdictServer() *verdictServer {
	s := &verdictServer{}
	s.client = &WorkBranchClientMock{
		SubmitVerdictFunc: func(_ context.Context, req *connect.Request[loamv1.SubmitVerdictRequest]) (*connect.Response[loamv1.SubmitVerdictResponse], error) {
			s.requests = append(s.requests, req.Msg)
			return connect.NewResponse(&loamv1.SubmitVerdictResponse{
				Outcome:   req.Msg.GetOutcome(),
				Published: uint32(len(req.Msg.GetComments())),
			}), nil
		},
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			s.other = append(s.other, "GetWorkBranch")
			return nil, errors.New("verdict must not pre-check the work branch")
		},
		ListCommentsFunc: func(context.Context, *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error) {
			s.other = append(s.other, "ListComments")
			return nil, errors.New("verdict must not pre-check threads")
		},
		ReplyToThreadFunc: func(context.Context, *connect.Request[loamv1.ReplyToThreadRequest]) (*connect.Response[loamv1.ReplyToThreadResponse], error) {
			s.other = append(s.other, "ReplyToThread")
			return nil, errors.New("verdict must not post replies")
		},
	}
	return s
}

// rejectingVerdictServer builds a server whose SubmitVerdict fails with the
// given Connect status — the shape a state-gate refusal, a missing work
// branch, or an author-only resolve refusal arrives in.
func rejectingVerdictServer(code connect.Code, message string) *verdictServer {
	s := newVerdictServer()
	s.client.SubmitVerdictFunc = func(_ context.Context, req *connect.Request[loamv1.SubmitVerdictRequest]) (*connect.Response[loamv1.SubmitVerdictResponse], error) {
		s.requests = append(s.requests, req.Msg)
		return nil, connect.NewError(code, errors.New(message))
	}
	return s
}

// verdictDeps wires a Deps for `work verdict` against a REAL workspace
// rooted at workspaceRoot — the staging area and its clearing are the thing
// under test, so it is a real contained directory rather than a mock.
func verdictDeps(workspaceRoot string, srv *verdictServer, encoded *any) *Deps {
	return verdictDepsWithWorkspace(stagingWorkspace(workspaceRoot, testReviewer), srv, encoded)
}

// verdictDepsWithWorkspace is verdictDeps with the workspace resolver
// substituted, for the staging-failure cases.
func verdictDepsWithWorkspace(ws WorkspaceResolver, srv *verdictServer, encoded *any) *Deps {
	cfg := &ConfigMock{IdentifierFunc: func() string { return testReviewer }}
	connectClient := &ConnectClientMock{WorkBranchFunc: func() WorkBranchClient { return srv.client }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), cfg, encoder, newErrorMapper(), ws, connectClient, nil, nil)
}

// runVerdict runs one `work verdict` invocation as a fresh process would:
// new deps, new workspace resolver, new staging store.
func runVerdict(t *testing.T, workspaceRoot string, srv *verdictServer, args ...string) (any, error) {
	t.Helper()
	var encoded any
	err := runWorkVerdict(t.Context(), verdictDeps(workspaceRoot, srv, &encoded), args)
	return encoded, err
}

// verdictArgs is the positional pair plus --outcome every test below passes
// unless it is specifically exercising inference or a bad outcome.
func verdictArgs(outcome string) []string {
	return []string{testRepo, testWorkBranch, "--outcome", outcome}
}

// stageItems puts items into the reviewer's real staging area for the shared
// test repo/work branch, exactly as repeated `work comment` invocations
// would, and requires that they really landed — a test that asserts the
// staging area is EMPTY afterwards proves nothing unless it was non-empty
// first.
func stageItems(t *testing.T, workspaceRoot string, items ...stagedItem) {
	t.Helper()
	store := openTestStore(t, workspaceRoot, testReviewer)
	for _, item := range items {
		_, err := store.add(item)
		require.NoError(t, err)
	}
	staged, err := store.list()
	require.NoError(t, err)
	require.Len(t, staged, len(items), "precondition: the batch must really be staged before publishing it")
}

// remainingStaged reads the reviewer's staging area the way a later CLI
// invocation would.
func remainingStaged(t *testing.T, workspaceRoot string) []stagedItem {
	t.Helper()
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	return items
}

// --- publishing the batch ---

// TestRunWorkVerdict_PublishesTheStagedBatchAtomicallyAndClearsStaging is
// reviewing.feature's "Submitting a verdict publishes staged comments
// atomically with an outcome". The whole batch travels in ONE call — that
// call is where the server's transaction lives, so two calls would be two
// transactions — and the staging area is empty afterwards.
func TestRunWorkVerdict_PublishesTheStagedBatchAtomicallyAndClearsStaging(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot,
		stagedItem{File: "auth.go", Line: 42, Body: "this leaks a token"},
		stagedItem{Body: "a general remark"},
	)
	srv := newVerdictServer()
	encoded, err := runVerdict(t, workspaceRoot, srv, verdictArgs("disapprove")...)
	require.NoError(t, err)
	require.Len(t, srv.requests, 1, "the whole batch must publish in exactly one call")
	req := srv.requests[0]
	assert.Equal(t, testRepo, req.GetRepo())
	assert.Equal(t, testWorkBranch, req.GetWorkBranch())
	assert.Equal(t, loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE, req.GetOutcome())
	require.Len(t, req.GetComments(), 2, "both staged comments must travel in the batch")
	assert.Equal(t, "this leaks a token", req.GetComments()[0].GetBody())
	assert.Equal(t, "auth.go", req.GetComments()[0].GetAnchor().GetFile())
	assert.Equal(t, uint32(42), req.GetComments()[0].GetAnchor().GetLine())
	assert.Equal(t, "a general remark", req.GetComments()[1].GetBody())
	assert.Nil(t, req.GetComments()[1].GetAnchor(), "a top-level comment must carry no anchor")
	assert.Empty(t, srv.other, "verdict must make no rpc other than SubmitVerdict")
	assert.Equal(t, verdictOutput{Repo: testRepo, WorkBranch: testWorkBranch, Outcome: "disapprove", Published: 2}, encoded)
	assert.Empty(t, remainingStaged(t, workspaceRoot), "a published batch must no longer be staged")
}

// TestRunWorkVerdict_OutputShape pins the documented JSON (docs/cli-spec.md
// -> verdict), including published as a number rather than a string.
func TestRunWorkVerdict_OutputShape(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "a note"}, stagedItem{Body: "another"}, stagedItem{Body: "a third"})
	encoded, err := runVerdict(t, workspaceRoot, newVerdictServer(), verdictArgs("approve")...)
	require.NoError(t, err)
	raw, err := json.Marshal(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, `{"repo":"bobcob7/doc-server","work_branch":"wb-9c2f1a","outcome":"approve","published":3}`, string(raw))
}

// TestRunWorkVerdict_ResolveOnlyItem_TravelsAsAResolutionNotAComment: a
// staged `--resolve` with no body has nothing to publish as a comment, and
// the server rejects a bodyless comment outright — so it must travel only in
// resolve_thread_ids.
func TestRunWorkVerdict_ResolveOnlyItem_TravelsAsAResolutionNotAComment(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Resolve: "t1"})
	srv := newVerdictServer()
	encoded, err := runVerdict(t, workspaceRoot, srv, verdictArgs("approve")...)
	require.NoError(t, err)
	require.Len(t, srv.requests, 1)
	assert.Empty(t, srv.requests[0].GetComments(), "a resolve-only item must not be sent as a comment")
	assert.Equal(t, []string{"t1"}, srv.requests[0].GetResolveThreadIds())
	assert.Equal(t, uint32(0), encoded.(verdictOutput).Published)
}

// TestRunWorkVerdict_ItemWithBodyAndResolve_TravelsAsBoth covers
// docs/cli-spec.md's "--resolve may accompany a new comment": one staged
// item, two effects, published in the same transaction.
func TestRunWorkVerdict_ItemWithBodyAndResolve_TravelsAsBoth(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "fixed, thanks", Resolve: "t1"})
	srv := newVerdictServer()
	_, err := runVerdict(t, workspaceRoot, srv, verdictArgs("approve")...)
	require.NoError(t, err)
	require.Len(t, srv.requests, 1)
	require.Len(t, srv.requests[0].GetComments(), 1)
	assert.Equal(t, "fixed, thanks", srv.requests[0].GetComments()[0].GetBody())
	assert.Equal(t, []string{"t1"}, srv.requests[0].GetResolveThreadIds())
}

// TestRunWorkVerdict_WholeFileAnchor_SendsNoLine pins the one place the
// staged format and the wire format disagree: staging stores 0 for "no line
// within this file", and FileLine.line is optional, so 0 must be sent as an
// ABSENT line, never as line zero.
func TestRunWorkVerdict_WholeFileAnchor_SendsNoLine(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{File: "auth.go", Body: "this whole file"})
	srv := newVerdictServer()
	_, err := runVerdict(t, workspaceRoot, srv, verdictArgs("neutral")...)
	require.NoError(t, err)
	require.Len(t, srv.requests, 1)
	require.Len(t, srv.requests[0].GetComments(), 1, "precondition: the anchored comment must reach the batch")
	anchor := srv.requests[0].GetComments()[0].GetAnchor()
	require.NotNil(t, anchor)
	assert.Equal(t, "auth.go", anchor.GetFile())
	assert.Nil(t, anchor.Line, "a whole-file anchor must send no line, not line 0")
}

// TestRunWorkVerdict_OutcomeOnly_IsAllowed is reviewing.feature's "An
// outcome-only verdict is allowed": nothing staged is not an error, it is a
// verdict with no comments.
func TestRunWorkVerdict_OutcomeOnly_IsAllowed(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newVerdictServer()
	encoded, err := runVerdict(t, workspaceRoot, srv, verdictArgs("neutral")...)
	require.NoError(t, err)
	require.Len(t, srv.requests, 1)
	assert.Empty(t, srv.requests[0].GetComments())
	assert.Empty(t, srv.requests[0].GetResolveThreadIds())
	assert.Equal(t, loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL, srv.requests[0].GetOutcome())
	assert.Equal(t, verdictOutput{Repo: testRepo, WorkBranch: testWorkBranch, Outcome: "neutral", Published: 0}, encoded)
}

// TestRunWorkVerdict_ReSubmitting_DoesNotRepublishTheClearedBatch is the
// CLI's half of reviewing.feature's "Re-submitting replaces my verdict for
// the round". The server replaces the verdict row; this side must not send
// the already-published comments a second time, which is exactly what the
// clear buys.
func TestRunWorkVerdict_ReSubmitting_DoesNotRepublishTheClearedBatch(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "a note"}, stagedItem{Resolve: "t1"})
	srv := newVerdictServer()
	_, err := runVerdict(t, workspaceRoot, srv, verdictArgs("disapprove")...)
	require.NoError(t, err)
	encoded, err := runVerdict(t, workspaceRoot, srv, verdictArgs("approve")...)
	require.NoError(t, err)
	require.Len(t, srv.requests, 2)
	require.Len(t, srv.requests[0].GetComments(), 1, "precondition: the first submission really carried the batch")
	assert.Empty(t, srv.requests[1].GetComments(), "the second submission must not republish the first batch")
	assert.Empty(t, srv.requests[1].GetResolveThreadIds(), "the second submission must not re-resolve the first batch's threads")
	assert.Equal(t, uint32(0), encoded.(verdictOutput).Published)
}

// TestRunWorkVerdict_ClearingDoesNotRewindStagedIDs: a comment staged after
// a publish must not reuse an id that already named a published one, for the
// same reason discard does not free its id.
func TestRunWorkVerdict_ClearingDoesNotRewindStagedIDs(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "s1's body"}, stagedItem{Body: "s2's body"})
	_, err := runVerdict(t, workspaceRoot, newVerdictServer(), verdictArgs("approve")...)
	require.NoError(t, err)
	item, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: "staged after publishing"})
	require.NoError(t, err)
	assert.Equal(t, "s3", item.ID, "clearing must not rewind next_id onto an id that already named a published comment")
}

// --- usage errors (exit 2), decided before any rpc or staging access ---

func TestRunWorkVerdict_InvalidOutcome_ExitsTwoWithoutPublishing(t *testing.T) {
	t.Parallel()
	tests := map[string]([]string){
		"missing --outcome":             {testRepo, testWorkBranch},
		"empty --outcome":               verdictArgs(""),
		"unrecognized outcome":          verdictArgs("yes"),
		"wrong case":                    verdictArgs("Approve"),
		"proto enum name":               verdictArgs("VERDICT_OUTCOME_APPROVE"),
		"too many positional arguments": {testRepo, testWorkBranch, "extra", "--outcome", "approve"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspaceRoot := realTempDir(t)
			stageItems(t, workspaceRoot, stagedItem{Body: "a note"})
			srv := newVerdictServer()
			encoded, err := runVerdict(t, workspaceRoot, srv, args...)
			require.Error(t, err)
			assert.Equal(t, 2, newErrorMapper().ExitCode(err))
			assert.Nil(t, encoded, "a rejected invocation must encode nothing")
			assert.Empty(t, srv.requests, "a usage error must be decided before any rpc")
			assert.Len(t, remainingStaged(t, workspaceRoot), 1, "a rejected invocation must not clear the staging area")
		})
	}
}

// --- server-side rejections keep the batch ---

// TestRunWorkVerdict_NotOpenForReview_ExitsTwoAndKeepsTheBatch is
// reviewing.feature's "A verdict cannot be submitted before review is
// requested", plus docs/cli-spec.md's "Against a terminal branch, verdict
// fails with the precondition error and the staged items remain until
// --discarded — no automatic cleanup". Losing the batch here would be
// unrecoverable, which is why the clear happens only after a success.
func TestRunWorkVerdict_NotOpenForReview_ExitsTwoAndKeepsTheBatch(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "a note"}, stagedItem{Body: "another note"})
	srv := rejectingVerdictServer(connect.CodeFailedPrecondition, "work branch is draft; no round exists yet")
	encoded, err := runVerdict(t, workspaceRoot, srv, verdictArgs("approve")...)
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codePreconditionFailed, ce.code)
	assert.Nil(t, encoded)
	assert.Len(t, remainingStaged(t, workspaceRoot), 2, "a refused verdict must leave the batch staged")
}

// TestRunWorkVerdict_UnknownWorkBranch_ExitsThree: the server owns that
// lookup, and its NotFound must reach the exit code unreclassified.
func TestRunWorkVerdict_UnknownWorkBranch_ExitsThree(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "a note"})
	srv := rejectingVerdictServer(connect.CodeNotFound, "work branch bobcob7/doc-server/wb-9c2f1a not found")
	encoded, err := runVerdict(t, workspaceRoot, srv, verdictArgs("approve")...)
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codeNotFound, ce.code)
	assert.Nil(t, encoded)
	assert.Len(t, remainingStaged(t, workspaceRoot), 1)
}

// TestRunWorkVerdict_ResolvingAnotherAgentsThread_ExitsTwoAsUnauthorized
// keeps the server's author-only refusal on the SAME exit-code class as
// `work comment`'s local pre-check (exit 2, unauthorized): the two guard the
// same rule at different moments and must not disagree about what it costs.
func TestRunWorkVerdict_ResolvingAnotherAgentsThread_ExitsTwoAsUnauthorized(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Resolve: "t1"})
	srv := rejectingVerdictServer(connect.CodePermissionDenied, "only a thread's author may resolve it")
	encoded, err := runVerdict(t, workspaceRoot, srv, verdictArgs("approve")...)
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codeUnauthorized, ce.code)
	assert.Nil(t, encoded)
	assert.Len(t, remainingStaged(t, workspaceRoot), 1, "a refused verdict must leave the batch staged")
}

// --- the publish/clear failure ordering ---

// TestRunWorkVerdict_ClearFailsAfterPublish_ReportsThePublishedState is the
// one asymmetric failure this command can produce. Publishing succeeded, so
// the comments ARE visible; the staging area could not be emptied, so a
// re-run would publish them again. That must be reported loudly and
// non-zero, never encoded as a success — a silent success here is exactly
// how an agent double-publishes its review.
func TestRunWorkVerdict_ClearFailsAfterPublish_ReportsThePublishedState(t *testing.T) {
	t.Parallel()
	staged := []byte(`{"version":1,"next_id":2,"items":[{"id":"s1","body":"a note"}]}`)
	area := &StagingAreaMock{
		ReadFileFunc:  func(string) ([]byte, error) { return staged, nil },
		WriteFileFunc: func(string, []byte) error { return errStagingArea },
		CloseFunc:     func() error { return nil },
	}
	ws := &WorkspaceResolverMock{OpenStagingFunc: func(string, string) (StagingArea, error) { return area, nil }}
	srv := newVerdictServer()
	var encoded any
	err := runWorkVerdict(t.Context(), verdictDepsWithWorkspace(ws, srv, &encoded), verdictArgs("approve"))
	require.Error(t, err)
	require.Len(t, srv.requests, 1, "precondition: the batch really was published before the clear failed")
	require.Len(t, srv.requests[0].GetComments(), 1)
	assert.ErrorIs(t, err, errStagingArea)
	assert.Contains(t, err.Error(), "the verdict was published")
	assert.Contains(t, err.Error(), "a second time")
	assert.Nil(t, encoded, "a failed clear must not be reported as a successful verdict")
	assert.Equal(t, 1, newErrorMapper().ExitCode(err), "an unclassified local failure exits 1, not 0")
}

// TestRunWorkVerdict_UnopenableStagingArea_PublishesNothing proves the
// command does not fall back to publishing an empty batch when the staging
// area cannot be opened: an outcome-only verdict the reviewer never asked
// for would silently drop their whole review.
func TestRunWorkVerdict_UnopenableStagingArea_PublishesNothing(t *testing.T) {
	t.Parallel()
	ws := &WorkspaceResolverMock{OpenStagingFunc: func(string, string) (StagingArea, error) { return nil, errStagingArea }}
	srv := newVerdictServer()
	var encoded any
	err := runWorkVerdict(t.Context(), verdictDepsWithWorkspace(ws, srv, &encoded), verdictArgs("approve"))
	require.ErrorIs(t, err, errStagingArea)
	assert.Empty(t, srv.requests, "an unreadable staging area must not publish an empty batch")
	assert.Nil(t, encoded)
}

// --- identity resolution and dispatch ---

// TestRunWorkVerdict_InfersIdentityFromWorkspaceWhenPositionalsOmitted
// proves the [repo] [work-branch] convention holds here too, and that the
// batch published is the one staged under the SAME inferred key.
func TestRunWorkVerdict_InfersIdentityFromWorkspaceWhenPositionalsOmitted(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "an inferred batch"})
	srv := newVerdictServer()
	_, err := runVerdict(t, workspaceRoot, srv, "--outcome", "approve")
	require.NoError(t, err)
	require.Len(t, srv.requests, 1)
	assert.Equal(t, testRepo, srv.requests[0].GetRepo())
	assert.Equal(t, testWorkBranch, srv.requests[0].GetWorkBranch())
	require.Len(t, srv.requests[0].GetComments(), 1)
	assert.Equal(t, "an inferred batch", srv.requests[0].GetComments()[0].GetBody())
}

// TestRouterDispatch_WorkVerdict_ReachesRealHandler proves `loam work
// verdict` reaches this bead's handler rather than the errNotImplemented
// stub it replaced.
func TestRouterDispatch_WorkVerdict_ReachesRealHandler(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageItems(t, workspaceRoot, stagedItem{Body: "a routed comment"})
	var encoded any
	deps := verdictDeps(workspaceRoot, newVerdictServer(), &encoded)
	err := NewRouter(deps).Dispatch(t.Context(), []string{"work", "verdict", testRepo, testWorkBranch, "--outcome", "approve"})
	require.NoError(t, err)
	assert.Equal(t, verdictOutput{Repo: testRepo, WorkBranch: testWorkBranch, Outcome: "approve", Published: 1}, encoded)
	assert.Empty(t, remainingStaged(t, workspaceRoot))
}

// --- the store operation verdict owns ---

// TestStagingStoreClear_EmptiesItemsWithoutRewindingNextID exercises clear
// directly, including the case a whole-command test cannot reach cheaply:
// clearing an already-empty area is a no-op, not an error.
func TestStagingStoreClear_EmptiesItemsWithoutRewindingNextID(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	store := openTestStore(t, workspaceRoot, testReviewer)
	_, err := store.add(stagedItem{Body: "first"})
	require.NoError(t, err)
	_, err = store.add(stagedItem{Body: "second"})
	require.NoError(t, err)
	require.NoError(t, store.clear())
	items, err := store.list()
	require.NoError(t, err)
	require.Empty(t, items)
	require.NoError(t, store.clear(), "clearing an empty staging area must be a no-op")
	added, err := store.add(stagedItem{Body: "third"})
	require.NoError(t, err)
	assert.Equal(t, "s3", added.ID)
	assert.Equal(t, []string{"s3"}, stagedIDs(mustList(t, store)))
}

// mustList is a list() that fails the test rather than returning an error.
func mustList(t *testing.T, store *stagingStore) []stagedItem {
	t.Helper()
	items, err := store.list()
	require.NoError(t, err)
	return items
}
