package workbranch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/reviewpublish"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// reviewCaps is the capability set a reviewer agent carries in these tests:
// everything the review half of the service gates on, so a test that is not
// about authorization never trips it accidentally.
var reviewCaps = []handler.Capability{handler.CapabilityWorkRead, handler.CapabilityWorkVerdict, handler.CapabilityWorkReply}

// --- ListComments ---

// TestListComments_AgentLackingWorkRead_Denied proves the capability gate
// runs BEFORE the thread store is consulted. allMocks' ThreadStore answers a
// benign published thread, so removing the gate falls through to an
// observable 200 with one thread -- this fails on the CodePermissionDenied
// assertion, never on a nil-func panic.
func TestListComments_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkVerdict}, &buf)
	_, err := h.ListComments(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListCommentsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, threads.ListCalls(), "the thread store must not be consulted when the capability gate denies the caller")
}

// TestListComments_AdminSuperuser_Allowed pins docs/web-spec.md ->
// ProposalService, which lists ListComments among the operations the admin
// reaches as superuser: an admin carries no role and no capabilities at all,
// so this only passes because RequireCapability's admin bypass runs.
func TestListComments_AdminSuperuser_Allowed(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, nil, &buf)
	resp, err := h.ListComments(adminCtx(t), connect.NewRequest(&loamv1.ListCommentsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Len(t, resp.Msg.GetThreads(), 1)
}

// TestListComments_ThreadAndCommentReportTheirOwnRounds is
// reviewing.feature's "Verdicts and comments record their review round" and
// replies.feature's "the thread still shows it was raised in the first
// round", at the RPC surface: the fixture thread was raised in round 1 and
// carries a comment posted in round 2, and the response must report both
// numbers independently. A handler that copied the thread's round onto its
// comments -- the obvious wrong implementation -- fails the comment
// assertion here.
func TestListComments_ThreadAndCommentReportTheirOwnRounds(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.ListComments(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListCommentsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetThreads(), 1)
	thread := resp.Msg.GetThreads()[0]
	assert.Equal(t, uint32(1), thread.GetRound(), "the thread reports the round it was RAISED in")
	require.Len(t, thread.GetComments(), 1)
	assert.Equal(t, uint32(2), thread.GetComments()[0].GetRound(), "a comment reports its OWN round, which may be later than its thread's")
}

// TestListComments_AnchorAndPageInfo proves the anchored-thread mapping and
// PageInfo.total: the fixture thread is anchored at auth.go:42 and the store
// reports a total of 1.
func TestListComments_AnchorAndPageInfo(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.ListComments(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListCommentsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetThreads(), 1)
	anchor := resp.Msg.GetThreads()[0].GetAnchor()
	require.NotNil(t, anchor)
	assert.Equal(t, "auth.go", anchor.GetFile())
	assert.Equal(t, uint32(42), anchor.GetLine())
	assert.Equal(t, uint32(1), resp.Msg.GetPageInfo().GetTotal())
}

// TestListComments_TopLevelThread_NoAnchor proves an unanchored thread maps
// to a nil anchor rather than a FileLine with an empty path -- a top-level
// thread stores SQL NULL in threads.file, and rendering it as `""` would
// make every client show a file-anchored comment on a file named "".
func TestListComments_TopLevelThread_NoAnchor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	threads.ListFunc = func(_ context.Context, _ uuid.UUID, _, _ int32) ([]reviewstore.ThreadWithComments, int64, error) {
		thread := sampleThread()
		thread.File, thread.Line = nil, nil
		return []reviewstore.ThreadWithComments{thread}, 1, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.ListComments(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListCommentsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetThreads(), 1)
	assert.Nil(t, resp.Msg.GetThreads()[0].GetAnchor())
}

// TestListComments_PageDefaultsAndOverrides proves Page reaches the store:
// an unset Page uses the documented default limit of 100 (docs/cli-spec.md
// -> "list"), and an explicit Page is forwarded verbatim.
func TestListComments_PageDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		page          *loamv1.Page
		limit, offset int32
	}{
		{name: "unset page uses the server default", page: nil, limit: 100, offset: 0},
		{name: "explicit page is forwarded", page: &loamv1.Page{Limit: 5, Offset: 10}, limit: 5, offset: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
			_, err := h.ListComments(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListCommentsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a", Page: tc.page}))
			require.NoError(t, err)
			require.Len(t, threads.ListCalls(), 1)
			assert.Equal(t, tc.limit, threads.ListCalls()[0].Limit)
			assert.Equal(t, tc.offset, threads.ListCalls()[0].Offset)
		})
	}
}

// --- ListVerdicts ---

// TestListVerdicts_AgentLackingWorkRead_Denied proves the gate runs before
// the verdict store is consulted; allMocks' VerdictStore would otherwise
// answer one current approve verdict.
func TestListVerdicts_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkVerdict}, &buf)
	_, err := h.ListVerdicts(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListVerdictsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, verdicts.ListCalls(), "the verdict store must not be consulted when the capability gate denies the caller")
}

// TestListVerdicts_CurrentRoundVerdictIsNotStale is reviewing.feature's
// "Listing verdicts shows each reviewer once, with stale flags", happy half:
// two reviewers, both in the current round, both reported once and neither
// stale.
func TestListVerdicts_CurrentRoundVerdictIsNotStale(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	verdicts.ListFunc = func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
		return []reviewstore.VerdictRecord{
			verdictRecord("ada-lovelace-7-reviewer", reviewstore.OutcomeApprove, 1, true),
			verdictRecord("alan-turing-4-reviewer", reviewstore.OutcomeApprove, 1, true),
		}, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.ListVerdicts(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListVerdictsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetVerdicts(), 2)
	for _, verdict := range resp.Msg.GetVerdicts() {
		assert.False(t, verdict.GetStale(), "a current-round verdict is never stale")
		assert.Equal(t, uint32(1), verdict.GetRound())
	}
}

// TestListVerdicts_PriorRoundVerdictIsStale is the other half: a verdict
// cast in round 1 while the branch's current round is 2 reads as stale, and
// reports the round it was cast in. Staleness is the store's derived
// Current flag negated -- this handler owns no second mechanism for it, so
// a mutation that hardcodes Stale: false fails here.
func TestListVerdicts_PriorRoundVerdictIsStale(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	verdicts.ListFunc = func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
		return []reviewstore.VerdictRecord{verdictRecord("alan-turing-4-reviewer", reviewstore.OutcomeApprove, 1, false)}, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.ListVerdicts(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListVerdictsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetVerdicts(), 1)
	assert.True(t, resp.Msg.GetVerdicts()[0].GetStale(), "a verdict from a round before the current one is stale")
	assert.Equal(t, uint32(1), resp.Msg.GetVerdicts()[0].GetRound())
}

// TestListVerdicts_ReviewerWhoVotedTwice_ReportedOnceWithLatest pins
// reviewing.feature's "each reviewer appears once with their latest
// outcome" (and docs/cli-spec.md -> "verdicts": "each reviewer's recorded
// verdict (unique agent + outcome)"). ada voted disapprove in round 1 and
// approve in round 2; only the round-2 approve is reported, and it is not
// stale. alan voted only in round 1 and is still reported, stale.
func TestListVerdicts_ReviewerWhoVotedTwice_ReportedOnceWithLatest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	verdicts.ListFunc = func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
		// Newest round first -- the order ListVerdictsForWorkBranch's own
		// ORDER BY r.number DESC guarantees and dedupeLatestPerReviewer relies on.
		return []reviewstore.VerdictRecord{
			verdictRecord("ada-lovelace-7-reviewer", reviewstore.OutcomeApprove, 2, true),
			verdictRecord("ada-lovelace-7-reviewer", reviewstore.OutcomeDisapprove, 1, false),
			verdictRecord("alan-turing-4-reviewer", reviewstore.OutcomeDisapprove, 1, false),
		}, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.ListVerdicts(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListVerdictsRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetVerdicts(), 2, "each reviewer appears exactly once")
	assert.Equal(t, "ada-lovelace-7-reviewer", resp.Msg.GetVerdicts()[0].GetReviewer())
	assert.Equal(t, loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE, resp.Msg.GetVerdicts()[0].GetOutcome(), "ada's LATEST outcome, not her round-1 disapprove")
	assert.Equal(t, uint32(2), resp.Msg.GetVerdicts()[0].GetRound())
	assert.False(t, resp.Msg.GetVerdicts()[0].GetStale())
	assert.Equal(t, "alan-turing-4-reviewer", resp.Msg.GetVerdicts()[1].GetReviewer())
	assert.True(t, resp.Msg.GetVerdicts()[1].GetStale(), "alan never voted in the current round")
}

// --- SubmitVerdict ---

// TestSubmitVerdict_AgentLackingWorkVerdict_Denied proves the gate runs
// before anything is published; allMocks' publisher would otherwise answer a
// benign success.
func TestSubmitVerdict_AgentLackingWorkVerdict_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.SubmitVerdict(agentCtx(t, "author"), connect.NewRequest(submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE)))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, publisher.PublishCalls(), "nothing may be published when the capability gate denies the caller")
}

// TestSubmitVerdict_PublishesCommentsAtomicallyAsOneCall is the core of
// reviewing.feature's "Submitting a verdict publishes staged comments
// atomically with an outcome" at the handler boundary: the handler makes
// EXACTLY ONE call into the publisher carrying the whole batch -- outcome,
// both comments (anchors included), and the resolve list -- rather than
// sequencing several writes of its own, which is what makes the underlying
// single transaction possible at all. published echoes the batch size.
func TestSubmitVerdict_PublishesCommentsAtomicallyAsOneCall(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	threadID := uuid.New()
	line := uint32(42)
	req := submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE)
	req.Comments = []*loamv1.VerdictComment{
		{Anchor: &loamv1.FileLine{File: "auth.go", Line: &line}, Body: "needs a guard"},
		{Body: "overall this reads well"},
	}
	req.ResolveThreadIds = []string{threadID.String()}
	resp, err := h.SubmitVerdict(agentCtx(t, "reviewer"), connect.NewRequest(req))
	require.NoError(t, err)
	require.Len(t, publisher.PublishCalls(), 1, "the whole batch must cross the seam in ONE call -- several calls could not be one transaction")
	published := publisher.PublishCalls()[0].Req
	assert.Equal(t, reviewstore.OutcomeDisapprove, published.Outcome)
	assert.Equal(t, "grace-hopper-3-reviewer", published.Reviewer)
	require.Len(t, published.Comments, 2)
	require.NotNil(t, published.Comments[0].File)
	assert.Equal(t, "auth.go", *published.Comments[0].File)
	require.NotNil(t, published.Comments[0].Line)
	assert.Equal(t, int32(42), *published.Comments[0].Line)
	assert.Nil(t, published.Comments[1].File, "an unanchored comment opens a top-level thread, not one anchored to an empty path")
	assert.Equal(t, []uuid.UUID{threadID}, published.ResolveThreadIDs)
	assert.Equal(t, uint32(2), resp.Msg.GetPublished())
	assert.Equal(t, loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE, resp.Msg.GetOutcome())
}

// TestSubmitVerdict_OutcomeOnly_Allowed pins reviewing.feature's "An
// outcome-only verdict is allowed": no comments is a valid submission and
// reports published = 0, not an error.
func TestSubmitVerdict_OutcomeOnly_Allowed(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.SubmitVerdict(agentCtx(t, "reviewer"), connect.NewRequest(submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL)))
	require.NoError(t, err)
	assert.Equal(t, uint32(0), resp.Msg.GetPublished())
	assert.Equal(t, loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL, resp.Msg.GetOutcome())
}

// TestSubmitVerdict_RejectedBeforePublishing is the "nothing is published
// unless the whole request is valid" table: every one of these is caught
// before the publisher is reached, so a rejected verdict cannot leave a
// partial batch behind even in principle. Each row asserts the Connect code
// AND that Publish was never called -- the second assertion is the one that
// would catch a handler that validated after publishing.
func TestSubmitVerdict_RejectedBeforePublishing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mutate  func(*loamv1.SubmitVerdictRequest)
		admin   bool
		wantErr connect.Code
	}{
		{
			name:    "unspecified outcome",
			mutate:  func(r *loamv1.SubmitVerdictRequest) { r.Outcome = loamv1.VerdictOutcome_VERDICT_OUTCOME_UNSPECIFIED },
			wantErr: connect.CodeInvalidArgument,
		},
		{
			name:    "malformed thread id in the resolve list",
			mutate:  func(r *loamv1.SubmitVerdictRequest) { r.ResolveThreadIds = []string{uuid.NewString(), "not-a-uuid"} },
			wantErr: connect.CodeInvalidArgument,
		},
		{
			name:    "comment with an empty body",
			mutate:  func(r *loamv1.SubmitVerdictRequest) { r.Comments = []*loamv1.VerdictComment{{Body: ""}} },
			wantErr: connect.CodeInvalidArgument,
		},
		{
			name: "anchor with no file path",
			mutate: func(r *loamv1.SubmitVerdictRequest) {
				r.Comments = []*loamv1.VerdictComment{{Anchor: &loamv1.FileLine{}, Body: "hm"}}
			},
			wantErr: connect.CodeInvalidArgument,
		},
		{
			name:    "admin superuser has no agent identity to record as reviewer",
			mutate:  func(*loamv1.SubmitVerdictRequest) {},
			admin:   true,
			wantErr: connect.CodeInvalidArgument,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
			req := submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE)
			tc.mutate(req)
			ctx := agentCtx(t, "reviewer")
			if tc.admin {
				ctx = adminCtx(t)
			}
			_, err := h.SubmitVerdict(ctx, connect.NewRequest(req))
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, connectCode(t, err))
			assert.Empty(t, publisher.PublishCalls(), "a rejected verdict must never reach the publisher")
		})
	}
}

// TestSubmitVerdict_PublisherErrors_MappedToConnectCodes pins the mapping of
// every caller-fixable publish failure. reviewpublish.ErrNotOpenForReview is
// reviewing.feature's "A verdict cannot be submitted before review is
// requested" (a draft has no round) and cli-spec's terminal-state rejection;
// reviewstore.ErrNotThreadAuthor is "Only the thread's author may resolve
// it". Each must reach the caller as its own code, not collapse into
// CodeInternal.
func TestSubmitVerdict_PublisherErrors_MappedToConnectCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		publish  error
		wantCode connect.Code
	}{
		{name: "work branch not open for review", publish: fmt.Errorf("work branch is draft: %w", reviewpublish.ErrNotOpenForReview), wantCode: connect.CodeFailedPrecondition},
		{name: "no review round yet", publish: fmt.Errorf("wrapped: %w", reviewstore.ErrNoCurrentRound), wantCode: connect.CodeFailedPrecondition},
		{name: "resolving another agent's thread", publish: fmt.Errorf("wrapped: %w", reviewstore.ErrNotThreadAuthor), wantCode: connect.CodePermissionDenied},
		{name: "resolving a thread that does not exist", publish: fmt.Errorf("wrapped: %w", reviewstore.ErrThreadNotFound), wantCode: connect.CodeNotFound},
		{name: "work branch vanished mid-publish", publish: fmt.Errorf("wrapped: %w", workbranchstore.ErrNotFound), wantCode: connect.CodeNotFound},
		{name: "anything else is internal", publish: errors.New("connection reset"), wantCode: connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			publisher.PublishFunc = func(context.Context, reviewpublish.Request) (reviewpublish.Result, error) {
				return reviewpublish.Result{}, tc.publish
			}
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
			_, err := h.SubmitVerdict(agentCtx(t, "reviewer"), connect.NewRequest(submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE)))
			require.Error(t, err)
			assert.Equal(t, tc.wantCode, connectCode(t, err))
		})
	}
}

// TestSubmitVerdict_NotOpenForReview_MessageNamesActualState is loam-jv8f's
// acceptance test for mapPublishErr's ErrNotOpenForReview/ErrNoCurrentRound
// case: it used to wrap only handler.ErrFailedPrecondition and discard err
// entirely, so the message an agent or operator saw ended in the generic
// "handler: failed precondition" with the actual state
// (reviewpublish.ErrNotOpenForReview's own "work branch is %s: %w"
// wrapping) nowhere to be found.
func TestSubmitVerdict_NotOpenForReview_MessageNamesActualState(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	publisher.PublishFunc = func(context.Context, reviewpublish.Request) (reviewpublish.Result, error) {
		return reviewpublish.Result{}, fmt.Errorf("work branch is %s: %w", workbranchstore.StateDraft, reviewpublish.ErrNotOpenForReview)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	_, err := h.SubmitVerdict(agentCtx(t, "reviewer"), connect.NewRequest(submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE)))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), "work branch is draft", "the actual state must survive -- it used to be discarded in favor of only the generic failed-precondition text")
	assert.ErrorIs(t, err, reviewpublish.ErrNotOpenForReview, "the sentinel must survive the mapping, not just the Connect code")
	assert.ErrorIs(t, err, handler.ErrFailedPrecondition)
}

// TestSubmitVerdict_RejectedResolve_PublishesNothing is the handler-level
// half of the atomicity claim in reviewing.feature's "Only the thread's
// author may resolve it": when the resolve step fails, the caller gets the
// rejection and NO response reporting published comments. The store-level
// half -- that the comments are not merely unreported but never committed --
// is proved against a real Postgres in internal/reviewpublish's integration
// test, because no mock can establish it.
func TestSubmitVerdict_RejectedResolve_PublishesNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	publisher.PublishFunc = func(context.Context, reviewpublish.Request) (reviewpublish.Result, error) {
		return reviewpublish.Result{}, fmt.Errorf("resolving: %w", reviewstore.ErrNotThreadAuthor)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	req := submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE)
	req.Comments = []*loamv1.VerdictComment{{Body: "needs a guard"}}
	req.ResolveThreadIds = []string{uuid.NewString()}
	resp, err := h.SubmitVerdict(agentCtx(t, "reviewer"), connect.NewRequest(req))
	require.Error(t, err)
	assert.Nil(t, resp, "a rejected verdict returns no response at all -- never one reporting published comments")
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
}

// TestSubmitVerdict_NotThreadAuthor_MessageNamesAuthorAndActor is loam-jhyo's
// acceptance test for mapPublishErr's ErrNotThreadAuthor case: it used to
// wrap only handler.ErrPermissionDenied and discard err, dropping the actual
// author and actor names (reviewstore's own "thread %s was opened by %s,
// not %s: %w" wrapping) -- precisely the diagnostic a caller wants, and the
// same defect TestReplyToThread_NotThreadAuthor_MessageNamesAuthorAndActor
// covers for mapThreadStoreErr. author and actor are fresh random uuids
// rather than any value already present in the request or the handler's own
// context string, so the message assertions cannot be satisfied textually
// by that prefix alone -- they only pass if err itself survives the wrap.
func TestSubmitVerdict_NotThreadAuthor_MessageNamesAuthorAndActor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	threadID := uuid.New()
	author := uuid.NewString()
	actor := uuid.NewString()
	publisher.PublishFunc = func(context.Context, reviewpublish.Request) (reviewpublish.Result, error) {
		return reviewpublish.Result{}, fmt.Errorf("thread %s was opened by %s, not %s: %w", threadID, author, actor, reviewstore.ErrNotThreadAuthor)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	req := submitRequest(loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE)
	req.ResolveThreadIds = []string{threadID.String()}
	_, err := h.SubmitVerdict(agentCtx(t, "reviewer"), connect.NewRequest(req))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), author, "the actual author must survive -- it used to be discarded in favor of only the generic permission-denied text")
	assert.Contains(t, connectErr.Message(), actor, "the actual actor must survive too")
	assert.ErrorIs(t, err, reviewstore.ErrNotThreadAuthor, "the sentinel must survive the mapping, not just the Connect code")
	assert.ErrorIs(t, err, handler.ErrPermissionDenied)
}

// --- ReplyToThread ---

// TestReplyToThread_AgentLackingWorkReply_Denied proves the gate runs before
// anything is written; allMocks' ThreadStore would otherwise answer a
// successful reply.
func TestReplyToThread_AgentLackingWorkReply_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.ReplyToThread(agentCtx(t, "author"), connect.NewRequest(replyRequest(uuid.NewString())))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, threads.ReplyCalls(), "nothing may be written when the capability gate denies the caller")
}

// TestReplyToThread_StampsTheCurrentRound_NotTheThreadsRound is
// replies.feature's "A reply records the round it was made in": the thread
// was raised in round 1 (allMocks' ThreadStore.Get answers sampleRoundID),
// the branch is now on round 2, and the reply must carry round 2. A handler
// that reused the thread's round -- the plausible wrong reading of "the
// thread still shows it was raised in the first round" -- fails here on both
// the RoundID and the reported round number.
func TestReplyToThread_StampsTheCurrentRound_NotTheThreadsRound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	secondRoundID := uuid.New()
	rounds.CurrentRoundFunc = func(_ context.Context, workBranchID uuid.UUID) (reviewstore.Round, error) {
		return reviewstore.Round{ID: secondRoundID, WorkBranchID: workBranchID, Number: 2}, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	resp, err := h.ReplyToThread(agentCtx(t, "author"), connect.NewRequest(replyRequest(uuid.NewString())))
	require.NoError(t, err)
	require.Len(t, threads.ReplyCalls(), 1)
	assert.Equal(t, secondRoundID, threads.ReplyCalls()[0].RoundID, "the reply is stamped with the branch's CURRENT round")
	assert.NotEqual(t, sampleRoundID, threads.ReplyCalls()[0].RoundID, "and specifically not the round the thread was raised in")
	assert.Equal(t, int32(2), threads.ReplyCalls()[0].RoundNumber)
	assert.Equal(t, uint32(2), resp.Msg.GetComment().GetRound())
	assert.Equal(t, "grace-hopper-3-author", resp.Msg.GetComment().GetAuthor())
	assert.Equal(t, "thanks, fixed", resp.Msg.GetComment().GetBody())
}

// TestReplyToThread_ChangesNoStateAndCastsNoVerdict pins replies.feature's
// "Replying does not change the work branch state" and "Replying does not
// affect verdicts": the reply path must not touch the work-branch state
// machine or the verdict publisher at all. Asserting on the CALL RECORDS
// rather than on a re-read state value is deliberate -- a re-read would tell
// us only what the mock was configured to answer, which is unfalsifiable.
func TestReplyToThread_ChangesNoStateAndCastsNoVerdict(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	_, err := h.ReplyToThread(agentCtx(t, "author"), connect.NewRequest(replyRequest(uuid.NewString())))
	require.NoError(t, err)
	require.Len(t, threads.ReplyCalls(), 1, "the reply itself must have happened, or the assertions below prove nothing")
	assert.Empty(t, workBranches.UpdateStateCalls(), "a reply never transitions the work branch")
	assert.Empty(t, publisher.PublishCalls(), "a reply is not a verdict")
	assert.Empty(t, rounds.OpenRoundCalls(), "a reply never opens a round, so it cannot make a prior verdict stale")
}

// TestReplyToThread_TerminalWorkBranch_Rejected is replies.feature's
// "Replying on a completed work branch is rejected", plus the closed state
// docs/cli-spec.md's State gates table rejects alongside it.
func TestReplyToThread_TerminalWorkBranch_Rejected(t *testing.T) {
	t.Parallel()
	for _, state := range []workbranchstore.State{workbranchstore.StateComplete, workbranchstore.StateClosed} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			workBranches.GetByNameFunc = func(context.Context, uuid.UUID, string) (workbranchstore.WorkBranch, error) {
				return sampleTitledWorkBranch(state), nil
			}
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
			_, err := h.ReplyToThread(agentCtx(t, "author"), connect.NewRequest(replyRequest(uuid.NewString())))
			require.Error(t, err)
			assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
			assert.Empty(t, threads.ReplyCalls(), "a terminal work branch accepts no reply")
		})
	}
}

// TestReplyToThread_NonTerminalStates_Allowed is the positive half of the
// State gates table: reply is allowed in draft, reviewable AND reviewed, so
// the terminal-state guard above cannot have been written as an
// allow-list that quietly rejects a legitimate state too.
func TestReplyToThread_NonTerminalStates_Allowed(t *testing.T) {
	t.Parallel()
	for _, state := range []workbranchstore.State{workbranchstore.StateDraft, workbranchstore.StateReviewable, workbranchstore.StateReviewed} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			workBranches.GetByNameFunc = func(context.Context, uuid.UUID, string) (workbranchstore.WorkBranch, error) {
				return sampleTitledWorkBranch(state), nil
			}
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
			_, err := h.ReplyToThread(agentCtx(t, "author"), connect.NewRequest(replyRequest(uuid.NewString())))
			require.NoError(t, err)
			assert.Len(t, threads.ReplyCalls(), 1)
		})
	}
}

// TestReplyToThread_RejectedBeforeWriting covers the request-shape and
// lookup failures, each asserting the reply was never written.
// reviewstore.ErrThreadNotFound covers both "no such thread" and "a thread
// on some OTHER work branch" -- the store deliberately reports them
// identically so a thread id cannot be probed across work branches.
func TestReplyToThread_RejectedBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		threadID string
		body     string
		getErr   error
		admin    bool
		wantCode connect.Code
	}{
		{name: "empty body", threadID: uuid.NewString(), body: "", wantCode: connect.CodeInvalidArgument},
		{name: "empty thread id", threadID: "", body: "thanks, fixed", wantCode: connect.CodeInvalidArgument},
		{name: "malformed thread id", threadID: "not-a-uuid", body: "thanks, fixed", wantCode: connect.CodeInvalidArgument},
		{name: "unknown thread", threadID: uuid.NewString(), body: "thanks, fixed", getErr: fmt.Errorf("wrapped: %w", reviewstore.ErrThreadNotFound), wantCode: connect.CodeNotFound},
		{name: "admin superuser has no agent identity to attribute the reply to", threadID: uuid.NewString(), body: "thanks, fixed", admin: true, wantCode: connect.CodeInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			if tc.getErr != nil {
				threads.GetFunc = func(context.Context, uuid.UUID, uuid.UUID) (reviewstore.Thread, error) {
					return reviewstore.Thread{}, tc.getErr
				}
			}
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
			req := replyRequest(tc.threadID)
			req.Body = tc.body
			ctx := agentCtx(t, "author")
			if tc.admin {
				ctx = adminCtx(t)
			}
			_, err := h.ReplyToThread(ctx, connect.NewRequest(req))
			require.Error(t, err)
			assert.Equal(t, tc.wantCode, connectCode(t, err))
			assert.Empty(t, threads.ReplyCalls(), "no reply may be written when the request is rejected")
		})
	}
}

// TestReplyToThread_NoReviewRound_FailedPrecondition proves a branch that
// was never opened for review answers a precondition failure rather than
// collapsing to CodeInternal -- ErrNoCurrentRound is the caller's problem
// to fix (request review first), not an operational fault.
func TestReplyToThread_NoReviewRound_FailedPrecondition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	rounds.CurrentRoundFunc = func(_ context.Context, workBranchID uuid.UUID) (reviewstore.Round, error) {
		return reviewstore.Round{}, fmt.Errorf("getting current round for work branch %s: %w", workBranchID, reviewstore.ErrNoCurrentRound)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	_, err := h.ReplyToThread(agentCtx(t, "author"), connect.NewRequest(replyRequest(uuid.NewString())))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, threads.ReplyCalls())
}

// TestReplyToThread_NotThreadAuthor_MessageNamesAuthorAndActor is loam-jv8f's
// acceptance test for mapThreadStoreErr's ErrNotThreadAuthor case: it used
// to wrap only handler.ErrPermissionDenied and discard err, dropping the
// actual author and actor names (reviewstore's own "thread %s was opened by
// %s, not %s: %w" wrapping) -- precisely the diagnostic a caller wants.
func TestReplyToThread_NotThreadAuthor_MessageNamesAuthorAndActor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	threadID := uuid.New()
	threads.GetFunc = func(context.Context, uuid.UUID, uuid.UUID) (reviewstore.Thread, error) {
		return reviewstore.Thread{}, fmt.Errorf("thread %s was opened by %s, not %s: %w", threadID, "grace-hopper-3-author", "ada-lovelace-7-reviewer", reviewstore.ErrNotThreadAuthor)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, reviewCaps, &buf)
	_, err := h.ReplyToThread(agentCtx(t, "author"), connect.NewRequest(replyRequest(threadID.String())))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), "grace-hopper-3-author", "the actual author must survive -- it used to be discarded in favor of only the generic permission-denied text")
	assert.Contains(t, connectErr.Message(), "ada-lovelace-7-reviewer", "the actual actor must survive too")
	assert.ErrorIs(t, err, reviewstore.ErrNotThreadAuthor, "the sentinel must survive the mapping, not just the Connect code")
	assert.ErrorIs(t, err, handler.ErrPermissionDenied)
}

// verdictRecord builds one reviewstore.VerdictRecord fixture. current is the
// store's DERIVED "is this the branch's current round" flag, which the
// handler reports negated as `stale`.
func verdictRecord(reviewer string, outcome reviewstore.Outcome, round int32, current bool) reviewstore.VerdictRecord {
	return reviewstore.VerdictRecord{
		Verdict:     reviewstore.Verdict{ID: uuid.New(), RoundID: uuid.New(), Reviewer: reviewer, Outcome: outcome},
		RoundNumber: round,
		Current:     current,
	}
}

// submitRequest builds a minimal, valid SubmitVerdictRequest with the given
// outcome and no comments.
func submitRequest(outcome loamv1.VerdictOutcome) *loamv1.SubmitVerdictRequest {
	return &loamv1.SubmitVerdictRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a", Outcome: outcome}
}

// replyRequest builds a minimal, valid ReplyToThreadRequest against threadID.
func replyRequest(threadID string) *loamv1.ReplyToThreadRequest {
	return &loamv1.ReplyToThreadRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a", ThreadId: threadID, Body: "thanks, fixed"}
}
