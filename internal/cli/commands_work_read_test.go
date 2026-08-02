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

// jsonOf marshals whatever a handler encoded, so a test can pin the
// documented WIRE shape (docs/cli-spec.md) rather than only the Go struct --
// the difference that catches a missing `round`, a phantom `"file": ""`
// anchor, or a `null` where an empty array is promised.
func jsonOf(t *testing.T, encoded any) string {
	t.Helper()
	raw, err := json.Marshal(encoded)
	require.NoError(t, err)
	return string(raw)
}

// notFoundError is the connect error the server returns for a work branch
// that does not exist -- the exit 3 case of every read command.
func notFoundError() error {
	return connect.NewError(connect.CodeNotFound, errors.New("work branch bobcob7/doc-server/wb-9c2f1a not found"))
}

// anchoredThread builds a published thread anchored at file/line, raised in
// round, whose single comment was posted in commentRound (which may be
// later than round -- a reply).
func anchoredThread(id, file string, line uint32, round, commentRound uint32) *loamv1.Thread {
	return &loamv1.Thread{
		Id:       id,
		Resolved: false,
		Anchor:   &loamv1.FileLine{File: file, Line: &line},
		Round:    round,
		Comments: []*loamv1.Comment{{Author: testReviewer, Body: "this leaks a token", Round: commentRound}},
	}
}

// --- work list ---

// TestRunWorkList_NoFlags_SendsReviewableDefaultAndEncodesEnvelope pins both
// halves of `work list` with no flags: the request defaults to reviewable
// with no other filter set, and the response is the {truncated, results}
// envelope, NOT a bare array.
func TestRunWorkList_NoFlags_SendsReviewableDefaultAndEncodesEnvelope(t *testing.T) {
	t.Parallel()
	var captured *loamv1.ListWorkBranchesRequest
	client := &WorkBranchClientMock{
		ListWorkBranchesFunc: func(_ context.Context, req *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
			captured = req.Msg
			return connect.NewResponse(&loamv1.ListWorkBranchesResponse{WorkBranches: []*loamv1.WorkBranch{{
				Repo: testRepo, Name: testWorkBranch, Target: "main", Title: "Add login",
				Author: "grace-hopper-3-author", State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE,
			}}}), nil
		},
	}
	var encoded any
	err := runWorkList(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), nil)
	require.NoError(t, err)

	require.NotNil(t, captured)
	require.NotNil(t, captured.State)
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE, captured.GetState(), "--state defaults to reviewable")
	assert.Nil(t, captured.Repo, "an omitted --repo must not be sent as an empty filter")
	assert.Nil(t, captured.Author, "an omitted --author must not be sent as an empty filter")
	assert.Nil(t, captured.Target, "an omitted --target must not be sent as an empty filter")
	assert.False(t, captured.GetAwaitingReview())
	assert.Equal(t, uint32(100), captured.GetPage().GetLimit(), "--limit defaults to 100")

	assert.JSONEq(t, `{"truncated":false,"results":[
		{"repo":"bobcob7/doc-server","name":"wb-9c2f1a","target":"main","title":"Add login",
		 "author":"grace-hopper-3-author","state":"reviewable"}]}`, jsonOf(t, encoded))
}

// TestRunWorkList_EveryFilterForwarded proves each flag reaches the request
// as its own field -- a filter silently dropped here would list work
// branches the caller did not ask for.
func TestRunWorkList_EveryFilterForwarded(t *testing.T) {
	t.Parallel()
	var captured *loamv1.ListWorkBranchesRequest
	client := &WorkBranchClientMock{
		ListWorkBranchesFunc: func(_ context.Context, req *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
			captured = req.Msg
			return connect.NewResponse(&loamv1.ListWorkBranchesResponse{}), nil
		},
	}
	var encoded any
	args := []string{"--repo", testRepo, "--author", "grace-hopper-3-author", "--target", "main", "--awaiting-review", "--state", "reviewed", "--limit", "5"}
	require.NoError(t, runWorkList(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), args))

	require.NotNil(t, captured)
	assert.Equal(t, testRepo, captured.GetRepo())
	assert.Equal(t, "grace-hopper-3-author", captured.GetAuthor())
	assert.Equal(t, "main", captured.GetTarget())
	assert.True(t, captured.GetAwaitingReview())
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED, captured.GetState())
	assert.Equal(t, uint32(5), captured.GetPage().GetLimit())
}

// TestRunWorkList_EveryStateValueMapsToItsEnum walks the five documented
// --state values, so a renamed or mis-mapped state fails here rather than
// silently listing a different lifecycle state.
func TestRunWorkList_EveryStateValueMapsToItsEnum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  loamv1.WorkBranchState
	}{
		{"draft", loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT},
		{"reviewable", loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE},
		{"reviewed", loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED},
		{"complete", loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE},
		{"closed", loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()
			var captured *loamv1.ListWorkBranchesRequest
			client := &WorkBranchClientMock{
				ListWorkBranchesFunc: func(_ context.Context, req *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
					captured = req.Msg
					return connect.NewResponse(&loamv1.ListWorkBranchesResponse{}), nil
				},
			}
			var encoded any
			require.NoError(t, runWorkList(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{"--state", tt.value}))
			require.NotNil(t, captured)
			assert.Equal(t, tt.want, captured.GetState())
		})
	}
}

// TestRunWorkList_BadFilterValue_ExitsTwoWithoutCallingServer covers
// docs/cli-spec.md -> work list -> Errors ("exit 2 on a bad filter value"),
// including `--state=` -- reachable only by passing it explicitly, and
// refused rather than quietly treated as the default.
func TestRunWorkList_BadFilterValue_ExitsTwoWithoutCallingServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"unknown state", []string{"--state", "bogus"}},
		{"empty state", []string{"--state", ""}},
		{"negative limit", []string{"--limit", "-1"}},
		{"positional argument", []string{"acme/repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			client := &WorkBranchClientMock{
				ListWorkBranchesFunc: func(context.Context, *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
					called = true
					return nil, errors.New("must not be called")
				},
			}
			var encoded any
			err := runWorkList(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), tt.args)
			require.Error(t, err)
			assert.Equal(t, 2, newErrorMapper().ExitCode(err))
			assert.False(t, called, "a bad filter value must be rejected before any RPC")
			assert.Nil(t, encoded)
		})
	}
}

// TestRunWorkList_EmptyResult_ExitsZeroAsAnEmptyArray covers "An empty
// result is a normal exit 0" -- and that `results` is `[]`, not `null`,
// which an agent parsing the response would have to special-case.
func TestRunWorkList_EmptyResult_ExitsZeroAsAnEmptyArray(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		ListWorkBranchesFunc: func(context.Context, *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
			return connect.NewResponse(&loamv1.ListWorkBranchesResponse{}), nil
		},
	}
	var encoded any
	err := runWorkList(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, newErrorMapper().ExitCode(err))
	assert.JSONEq(t, `{"truncated":false,"results":[]}`, jsonOf(t, encoded))
}

// TestRunWorkList_TruncatedIsSurfaced proves the server's truncated flag
// reaches the caller: without it a capped list is indistinguishable from a
// complete one.
func TestRunWorkList_TruncatedIsSurfaced(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		ListWorkBranchesFunc: func(context.Context, *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
			return connect.NewResponse(&loamv1.ListWorkBranchesResponse{Truncated: true}), nil
		},
	}
	var encoded any
	require.NoError(t, runWorkList(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{"--limit", "1"}))
	out, ok := encoded.(workListOutput)
	require.True(t, ok, "work list must encode a workListOutput")
	assert.True(t, out.Truncated)
}

func TestRunWorkList_UnenrolledRepo_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		ListWorkBranchesFunc: func(context.Context, *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("repo acme/nope is not enrolled"))
		},
	}
	var encoded any
	err := runWorkList(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{"--repo", "acme/nope"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- work show ---

// noVerdictsFunc stubs ListVerdicts with an empty response. runWorkShow
// always calls ListVerdicts now, to populate latest_verdict (loam-o718), so
// every WorkBranchClientMock a `work show` test exercises must stub this
// method or the mock panics on the unconfigured call.
func noVerdictsFunc(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
	return connect.NewResponse(&loamv1.ListVerdictsResponse{}), nil
}

func TestRunWorkShow_Success_EncodesFullMetadata(t *testing.T) {
	t.Parallel()
	var captured *loamv1.GetWorkBranchRequest
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(_ context.Context, req *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			captured = req.Msg
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: testRepo, Name: testWorkBranch, Target: "main", Title: "Add login",
				Description: "adds a login form", Author: "grace-hopper-3-author",
				State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE,
			}}), nil
		},
		ListVerdictsFunc: noVerdictsFunc,
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Equal(t, testRepo, captured.GetRepo())
	assert.Equal(t, testWorkBranch, captured.GetWorkBranch())
	assert.JSONEq(t, `{"repo":"bobcob7/doc-server","name":"wb-9c2f1a","target":"main","title":"Add login",
		"description":"adds a login form","state":"reviewable","author":"grace-hopper-3-author"}`, jsonOf(t, encoded))
	assert.NotContains(t, jsonOf(t, encoded), `"round"`, "no round on the response means the JSON must omit the key entirely, not render a zeroed round")
}

// TestRunWorkShow_WithReviewRound_IncludesRound proves the opposite side of
// the absent/present distinction: once GetWorkBranchResponse carries a
// round, `work show` surfaces its number and requested_by exactly
// (docs/cli-spec.md -> show's `"round": { "number": 2, "requested_by": "..." }`
// example), matching Thread.round and Comment.round's naming (loam-ofg.9).
func TestRunWorkShow_WithReviewRound_IncludesRound(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{
				WorkBranch: &loamv1.WorkBranch{
					Repo: testRepo, Name: testWorkBranch, Target: "main", Title: "Add login",
					Description: "adds a login form", Author: "grace-hopper-3-author",
					State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE,
				},
				Round: &loamv1.GetWorkBranchResponse_Round{Number: 2, RequestedBy: "grace-hopper-3-author"},
			}), nil
		},
		ListVerdictsFunc: noVerdictsFunc,
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	assert.JSONEq(t, `{"repo":"bobcob7/doc-server","name":"wb-9c2f1a","target":"main","title":"Add login",
		"description":"adds a login form","state":"reviewable","author":"grace-hopper-3-author",
		"round":{"number":2,"requested_by":"grace-hopper-3-author"}}`, jsonOf(t, encoded))
}

// TestRunWorkShow_AcceptedProposal_ReportsItsUpstreamPRURL proves an agent
// whose proposal has been accepted can reach its own pull request from the
// CLI. Before loam-ls7u the URL was on the wire (the handler populates
// WorkBranch.upstream_pr_url) but workShowOutput had no field for it, so
// the only route to it was the admin ProposalService queue -- which agents
// cannot call at all.
func TestRunWorkShow_AcceptedProposal_ReportsItsUpstreamPRURL(t *testing.T) {
	t.Parallel()
	prURL := "https://forge.example/loam-demo/doc-server/pulls/1"
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(_ context.Context, _ *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: testRepo, Name: testWorkBranch, Target: "main", Title: "Add login",
				Description: "adds a login form", Author: "grace-hopper-3-author",
				State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED, UpstreamPrUrl: &prURL,
			}}), nil
		},
		ListVerdictsFunc: noVerdictsFunc,
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	assert.JSONEq(t, `{"repo":"bobcob7/doc-server","name":"wb-9c2f1a","target":"main","title":"Add login",
		"description":"adds a login form","state":"reviewed","author":"grace-hopper-3-author",
		"upstream_pr_url":"https://forge.example/loam-demo/doc-server/pulls/1"}`, jsonOf(t, encoded))
}

// TestRunWorkShow_ServerSendsAnEmptyPRURL_IsNotTreatedAsAbsent is why the
// field is a *string rather than a string with omitempty. An unaccepted
// branch and one whose server reported an EMPTY url must stay
// distinguishable: the first is the normal case, the second is a server
// bug, and a plain omitempty would render them identically and hide it.
//
// Verified by mutation -- changing the field to `string` with omitempty
// leaves TestRunWorkShow_AcceptedProposal_ReportsItsUpstreamPRURL and
// TestRunWorkShow_Success_EncodesFullMetadata green and turns only this
// one red.
func TestRunWorkShow_ServerSendsAnEmptyPRURL_IsNotTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	empty := ""
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(_ context.Context, _ *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: testRepo, Name: testWorkBranch, Target: "main", Title: "Add login",
				Description: "adds a login form", Author: "grace-hopper-3-author",
				State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED, UpstreamPrUrl: &empty,
			}}), nil
		},
		ListVerdictsFunc: noVerdictsFunc,
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	assert.Contains(t, jsonOf(t, encoded), `"upstream_pr_url":""`, "a present-but-empty url must be reported, not silently dropped like an absent one")
}

// TestRunWorkShow_OmittedPositionals_InferFromWorkspace proves the
// [repo] [work-branch] convention: with neither given, both come from the
// enclosing clone.
func TestRunWorkShow_OmittedPositionals_InferFromWorkspace(t *testing.T) {
	t.Parallel()
	var captured *loamv1.GetWorkBranchRequest
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(_ context.Context, req *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			captured = req.Msg
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{Repo: testRepo, Name: testWorkBranch}}), nil
		},
		ListVerdictsFunc: noVerdictsFunc,
	}
	ws := &WorkspaceResolverMock{
		ResolveRepoFunc:       func() (string, error) { return testRepo, nil },
		ResolveWorkBranchFunc: func() (string, error) { return testWorkBranch, nil },
	}
	var encoded any
	require.NoError(t, runWorkShow(t.Context(), workTestDeps(client, ws, "", &encoded), nil))
	require.NotNil(t, captured)
	assert.Equal(t, testRepo, captured.GetRepo())
	assert.Equal(t, testWorkBranch, captured.GetWorkBranch())
}

func TestRunWorkShow_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return nil, notFoundError()
		},
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// TestRunWorkShow_DisapproveVerdict_LatestVerdictCarriesOutcomeReviewerRoundAndStale
// is loam-o718's headline case: after a DISAPPROVE, `state` alone reads
// "reviewed" -- the same value an APPROVE would leave -- because
// internal/reviewpublish/publish.go's publishInTx flips reviewable ->
// reviewed on any outcome. latest_verdict is what lets an agent polling
// `show` alone tell the two apart, so this pins both halves together: state
// stays "reviewed" (unchanged, per the bead's constraint) AND latest_verdict
// reports the disapprove with all four fields.
func TestRunWorkShow_DisapproveVerdict_LatestVerdictCarriesOutcomeReviewerRoundAndStale(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: testRepo, Name: testWorkBranch, Target: "main", Title: "Add login",
				Description: "adds a login form", Author: "grace-hopper-3-author",
				State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED,
			}}), nil
		},
		ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			return connect.NewResponse(&loamv1.ListVerdictsResponse{Verdicts: []*loamv1.VerdictSummary{
				{Reviewer: testReviewer, Outcome: loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE, Round: 3, Stale: false},
			}}), nil
		},
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	assert.JSONEq(t, `{"repo":"bobcob7/doc-server","name":"wb-9c2f1a","target":"main","title":"Add login",
		"description":"adds a login form","state":"reviewed","author":"grace-hopper-3-author",
		"latest_verdict":{"outcome":"disapprove","reviewer":"ada-lovelace-7-reviewer","round":3,"stale":false}}`,
		jsonOf(t, encoded))
}

// TestRunWorkShow_NoVerdicts_OmitsLatestVerdictKeyEntirely proves
// latest_verdict is a pointer omitted via omitempty, not a zeroed object,
// when the branch has no verdicts yet -- matching Round/UpstreamPRURL's
// presence/absence convention (loam-0pj.10: a zeroed object would be a
// fabrication under a different name).
func TestRunWorkShow_NoVerdicts_OmitsLatestVerdictKeyEntirely(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: testRepo, Name: testWorkBranch, State: loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT,
			}}), nil
		},
		ListVerdictsFunc: noVerdictsFunc,
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	assert.NotContains(t, jsonOf(t, encoded), `"latest_verdict"`, "no verdicts means the key must be absent, not present-and-zero")
	out, ok := encoded.(workShowOutput)
	require.True(t, ok, "work show must encode a workShowOutput")
	assert.Nil(t, out.LatestVerdict)
}

// TestRunWorkShow_LatestVerdictIsMostRecentOverall_EvenWhenStale is
// "latest" as the bead defines it: the most recent verdict overall,
// including a stale one -- not the most recent NON-stale verdict (that is
// the approval-bar rule the server owns) and not a per-reviewer roll-up.
// Both rows here are already stale (a later round has been requested with
// no vote yet), and the higher-round one -- an approve -- must still win
// and still report stale:true, since a caller reading latest_verdict:
// approve on a stale verdict would be misled worse than by today's honest
// "reviewed".
func TestRunWorkShow_LatestVerdictIsMostRecentOverall_EvenWhenStale(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: testRepo, Name: testWorkBranch, State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE,
			}}), nil
		},
		ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			return connect.NewResponse(&loamv1.ListVerdictsResponse{Verdicts: []*loamv1.VerdictSummary{
				{Reviewer: "alan-turing-4-reviewer", Outcome: loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE, Round: 2, Stale: true},
				{Reviewer: testReviewer, Outcome: loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL, Round: 1, Stale: true},
			}}), nil
		},
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	out, ok := encoded.(workShowOutput)
	require.True(t, ok, "work show must encode a workShowOutput")
	require.NotNil(t, out.LatestVerdict)
	assert.Equal(t, &workShowVerdictOutput{
		Outcome: "approve", Reviewer: "alan-turing-4-reviewer", Round: 2, Stale: true,
	}, out.LatestVerdict, "the higher-round verdict wins even though it is stale, and reports stale:true rather than being dropped or laundered")
}

// TestRunWorkShow_ListVerdictsErrors_SurfacesAsAnError proves a ListVerdicts
// failure is a real error, not swallowed into an omitted latest_verdict.
// This is deliberately NOT a graceful-degradation path: ListVerdicts and
// GetWorkBranch are gated by the same CapabilityWorkRead
// (internal/handler/workbranch/review.go:74, workbranch.go:331), so no role
// that reaches this call can have GetWorkBranch succeed while ListVerdicts
// fails on permissions -- an error here is always a genuine failure.
func TestRunWorkShow_ListVerdictsErrors_SurfacesAsAnError(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{Repo: testRepo, Name: testWorkBranch}}), nil
		},
		ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("verdict store unreachable"))
		},
	}
	var encoded any
	err := runWorkShow(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.Error(t, err)
	assert.Nil(t, encoded, "a ListVerdicts failure must not encode a partial work show response")
}

// TestRunWorkReadCommands_UnresolvableIdentifier_ExitTwoWithoutCallingServer
// covers "exit 2 if the identifier cannot be resolved (not in a clone and
// arguments omitted)" for every read command that takes the positional
// pair, and proves none of them reaches the network first.
func TestRunWorkReadCommands_UnresolvableIdentifier_ExitTwoWithoutCallingServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  handlerFunc
		args []string
	}{
		{"work show", runWorkShow, nil},
		{"work diff", runWorkDiff, nil},
		{"work comments", runWorkComments, nil},
		{"work comments staged", runWorkComments, []string{"--staged"}},
		{"work verdicts", runWorkVerdicts, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			fail := func() { called = true }
			client := &WorkBranchClientMock{
				GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
					fail()
					return nil, errors.New("must not be called")
				},
				GetWorkBranchDiffFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchDiffRequest]) (*connect.Response[loamv1.GetWorkBranchDiffResponse], error) {
					fail()
					return nil, errors.New("must not be called")
				},
				ListCommentsFunc: func(context.Context, *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error) {
					fail()
					return nil, errors.New("must not be called")
				},
				ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
					fail()
					return nil, errors.New("must not be called")
				},
			}
			var encoded any
			err := tt.run(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), tt.args)
			require.Error(t, err)
			assert.Equal(t, 2, newErrorMapper().ExitCode(err))
			assert.False(t, called, "an unresolvable identifier must be rejected before any RPC")
			assert.Nil(t, encoded)
		})
	}
}

// TestRunWorkReadCommands_TooManyPositionals_ExitUsage pins the shared
// "[repo] [work-branch], at most two" convention for every read command
// that takes it.
func TestRunWorkReadCommands_TooManyPositionals_ExitUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  handlerFunc
	}{
		{"work show", runWorkShow},
		{"work diff", runWorkDiff},
		{"work comments", runWorkComments},
		{"work verdicts", runWorkVerdicts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var encoded any
			deps := workTestDeps(&WorkBranchClientMock{}, noResolveWorkspace(), "", &encoded)
			err := tt.run(t.Context(), deps, []string{testRepo, testWorkBranch, "extra"})
			require.Error(t, err)
			var ue *usageError
			assert.ErrorAs(t, err, &ue)
			assert.Equal(t, 2, newErrorMapper().ExitCode(err))
		})
	}
}

// --- work diff ---

func TestRunWorkDiff_Success_EncodesTheDiffAsAField(t *testing.T) {
	t.Parallel()
	var captured *loamv1.GetWorkBranchDiffRequest
	client := &WorkBranchClientMock{
		GetWorkBranchDiffFunc: func(_ context.Context, req *connect.Request[loamv1.GetWorkBranchDiffRequest]) (*connect.Response[loamv1.GetWorkBranchDiffResponse], error) {
			captured = req.Msg
			return connect.NewResponse(&loamv1.GetWorkBranchDiffResponse{Diff: "--- a/auth.go\n+++ b/auth.go\n"}), nil
		},
	}
	var encoded any
	err := runWorkDiff(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Equal(t, testWorkBranch, captured.GetWorkBranch())
	assert.JSONEq(t, `{"diff":"--- a/auth.go\n+++ b/auth.go\n"}`, jsonOf(t, encoded))
}

func TestRunWorkDiff_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		GetWorkBranchDiffFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchDiffRequest]) (*connect.Response[loamv1.GetWorkBranchDiffResponse], error) {
			return nil, notFoundError()
		},
	}
	var encoded any
	err := runWorkDiff(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// TestRunWorkDiff_MissingMirrorRef_ExitsTwoAsPreconditionFailed pins how
// this command reports a server-side FailedPrecondition: exit 2 /
// precondition_failed carrying the server's own message -- NOT a fabricated
// empty diff, which an agent would read as "no changes yet". Since loam-5iu
// created the ref at `work start` time this is an edge case (a mirror out
// of step with the registry, or two histories with no merge base) rather
// than the common path it used to be, but the reporting contract is
// unchanged and is what this pins.
func TestRunWorkDiff_MissingMirrorRef_ExitsTwoAsPreconditionFailed(t *testing.T) {
	t.Parallel()
	const message = "computing diff for work branch bobcob7/doc-server/wb-9c2f1a: ref missing"
	client := &WorkBranchClientMock{
		GetWorkBranchDiffFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchDiffRequest]) (*connect.Response[loamv1.GetWorkBranchDiffResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(message))
		},
	}
	var encoded any
	err := runWorkDiff(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codePreconditionFailed, ce.code)
	assert.Equal(t, message, ce.Error(), "the server's own explanation must reach the caller verbatim")
	assert.Nil(t, encoded, "a failed diff must encode nothing, least of all an empty diff")
}

// --- work comments (published) ---

// TestRunWorkComments_PublishedThreads_PinTheDocumentedShape covers the
// exact JSON docs/cli-spec.md -> comments (get) documents, including the
// `round` on the thread AND on each comment.
func TestRunWorkComments_PublishedThreads_PinTheDocumentedShape(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(anchoredThread("t1", "auth.go", 42, 1, 1))
	var encoded any
	err := runWorkComments(t.Context(), workTestDeps(srv.client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.NoError(t, err)
	assert.JSONEq(t, `[{"id":"t1","resolved":false,"file":"auth.go","line":42,"round":1,
		"comments":[{"author":"ada-lovelace-7-reviewer","body":"this leaks a token","round":1}]}]`, jsonOf(t, encoded))
}

// TestRunWorkComments_ReplyKeepsItsOwnLaterRound proves a comment's round
// is the comment's own, never inherited from the thread it lives in: a
// thread raised in round 1 can carry a reply posted in round 2.
func TestRunWorkComments_ReplyKeepsItsOwnLaterRound(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(anchoredThread("t1", "auth.go", 42, 1, 2))
	var encoded any
	require.NoError(t, runWorkComments(t.Context(), workTestDeps(srv.client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch}))
	rows, ok := encoded.([]threadOutput)
	require.True(t, ok, "work comments must encode []threadOutput")
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Comments, 1)
	assert.Equal(t, uint32(1), rows[0].Round, "the thread reports the round it was RAISED in")
	assert.Equal(t, uint32(2), rows[0].Comments[0].Round, "a reply reports the round it was POSTED in")
}

// TestRunWorkComments_AnchorlessAndWholeFileThreads_OmitEmptyAnchorFields
// keeps a top-level thread from reporting a phantom `"file": ""` and a
// whole-file anchor from reporting a `"line": 0` an agent would parse as
// line zero.
func TestRunWorkComments_AnchorlessAndWholeFileThreads_OmitEmptyAnchorFields(t *testing.T) {
	t.Parallel()
	topLevel := &loamv1.Thread{Id: "t1", Round: 3, Comments: []*loamv1.Comment{{Author: testReviewer, Body: "a general remark", Round: 3}}}
	wholeFile := &loamv1.Thread{Id: "t2", Resolved: true, Anchor: &loamv1.FileLine{File: "README.md"}, Round: 3,
		Comments: []*loamv1.Comment{{Author: testReviewer, Body: "stale docs", Round: 3}}}
	srv := newCommentServer(topLevel, wholeFile)
	var encoded any
	require.NoError(t, runWorkComments(t.Context(), workTestDeps(srv.client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch}))
	assert.JSONEq(t, `[
		{"id":"t1","resolved":false,"round":3,"comments":[{"author":"ada-lovelace-7-reviewer","body":"a general remark","round":3}]},
		{"id":"t2","resolved":true,"file":"README.md","round":3,"comments":[{"author":"ada-lovelace-7-reviewer","body":"stale docs","round":3}]}]`,
		jsonOf(t, encoded))
}

// TestRunWorkComments_FollowsPagination proves `comments` returns the
// threads on the work branch, not just the server's first page: with a page
// size of one and three threads, all three come back.
func TestRunWorkComments_FollowsPagination(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(
		anchoredThread("t1", "a.go", 1, 1, 1),
		anchoredThread("t2", "b.go", 2, 1, 1),
		anchoredThread("t3", "c.go", 3, 2, 2),
	)
	srv.pageSize = 1
	var encoded any
	require.NoError(t, runWorkComments(t.Context(), workTestDeps(srv.client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch}))
	rows, ok := encoded.([]threadOutput)
	require.True(t, ok)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	assert.Equal(t, []string{"t1", "t2", "t3"}, ids, "every page's threads must be returned, not just the first")
	assert.Equal(t, 3, srv.listCalls, "each page must be fetched")
}

func TestRunWorkComments_NoThreads_EncodesEmptyArray(t *testing.T) {
	t.Parallel()
	srv := newCommentServer()
	var encoded any
	require.NoError(t, runWorkComments(t.Context(), workTestDeps(srv.client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch}))
	assert.JSONEq(t, `[]`, jsonOf(t, encoded))
}

// TestRunWorkComments_NotFound_ExitsThree covers the published path's exit
// 3. ListComments resolves the work branch itself, so GetWorkBranch is wired
// to a plain (unclassifiable, exit 1) error rather than left nil: published
// mode must not spend a redundant existence check, and if it ever did, this
// test reports the wrong exit code instead of panicking on an unconfigured
// mock.
func TestRunWorkComments_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	getCalls := 0
	client := &WorkBranchClientMock{
		ListCommentsFunc: func(context.Context, *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error) {
			return nil, notFoundError()
		},
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			getCalls++
			return nil, errors.New("published comments must not check existence separately")
		},
	}
	var encoded any
	err := runWorkComments(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Equal(t, 0, getCalls, "ListComments resolves the work branch itself")
	assert.Nil(t, encoded)
}

// --- work comments: the staged / published boundary ---

// stageTwoItems stages an anchored comment and a resolve-only item in a
// real staging area under workspaceRoot, returning nothing: the point is
// the on-disk state the two `comments` modes then disagree about.
func stageTwoItems(t *testing.T, workspaceRoot string) {
	t.Helper()
	store := openTestStore(t, workspaceRoot, testReviewer)
	_, err := store.add(stagedItem{File: "auth.go", Line: 42, Body: "unpublished thought"})
	require.NoError(t, err)
	_, err = store.add(stagedItem{Resolve: "t1"})
	require.NoError(t, err)
}

// TestRunWorkComments_Published_ExcludeStagedItems is the headline
// behaviour of docs/cli-spec.md -> comments (get): "Published threads only;
// the caller's staged comments are excluded until submitted." With two
// items staged on disk and one thread published, plain `comments` returns
// exactly the published thread and nothing else.
func TestRunWorkComments_Published_ExcludeStagedItems(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageTwoItems(t, workspaceRoot)
	srv := newCommentServer(anchoredThread("t1", "auth.go", 42, 1, 1))
	var encoded any
	deps := workTestDeps(srv.client, stagingWorkspace(workspaceRoot, testReviewer), "", &encoded)
	require.NoError(t, runWorkComments(t.Context(), deps, []string{testRepo, testWorkBranch}))

	assert.NotContains(t, jsonOf(t, encoded), "unpublished thought", "a staged body must never appear in the published listing")
	assert.NotContains(t, jsonOf(t, encoded), `"s1"`, "a staged item's local id must never appear in the published listing")
	rows, ok := encoded.([]threadOutput)
	require.True(t, ok, "work comments without --staged must encode published threads, not staged items")
	require.Len(t, rows, 1, "only the published thread may appear")
	assert.Equal(t, "t1", rows[0].ID)
}

// TestRunWorkComments_Staged_ReturnsTheLocalItemsAndNeverListsPublished is
// the other side of that boundary: `--staged` reports the caller's own
// unpublished items, in staging order, in the shape `comment` produced --
// and asks the server for no comments at all, since staged items are not
// there to ask for.
func TestRunWorkComments_Staged_ReturnsTheLocalItemsAndNeverListsPublished(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageTwoItems(t, workspaceRoot)
	srv := newCommentServer(anchoredThread("t1", "auth.go", 42, 1, 1))
	var encoded any
	deps := workTestDeps(srv.client, stagingWorkspace(workspaceRoot, testReviewer), "", &encoded)
	require.NoError(t, runWorkComments(t.Context(), deps, []string{testRepo, testWorkBranch, "--staged"}))

	assert.JSONEq(t, `[
		{"staged":true,"id":"s1","file":"auth.go","line":42,"body":"unpublished thought"},
		{"staged":true,"id":"s2","resolve":"t1"}]`, jsonOf(t, encoded))
	assert.Equal(t, 0, srv.listCalls, "--staged must not fetch published threads")
	assert.NotContains(t, jsonOf(t, encoded), "this leaks a token", "a published comment must never appear in the staged listing")
}

// TestRunWorkComments_Staged_OnlyTheCallersOwnItems proves the staging area
// is per-agent: another reviewer sharing the workspace sees none of these.
func TestRunWorkComments_Staged_OnlyTheCallersOwnItems(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	stageTwoItems(t, workspaceRoot)
	srv := newCommentServer()
	var encoded any
	deps := workTestDeps(srv.client, stagingWorkspace(workspaceRoot, "alan-turing-4-reviewer"), "", &encoded)
	require.NoError(t, runWorkComments(t.Context(), deps, []string{testRepo, testWorkBranch, "--staged"}))
	assert.JSONEq(t, `[]`, jsonOf(t, encoded))
}

func TestRunWorkComments_Staged_NothingStaged_EncodesEmptyArray(t *testing.T) {
	t.Parallel()
	srv := newCommentServer()
	var encoded any
	deps := workTestDeps(srv.client, stagingWorkspace(realTempDir(t), testReviewer), "", &encoded)
	require.NoError(t, runWorkComments(t.Context(), deps, []string{testRepo, testWorkBranch, "--staged"}))
	assert.JSONEq(t, `[]`, jsonOf(t, encoded))
}

// TestRunWorkComments_Staged_UnknownWorkBranch_ExitsThree proves --staged
// still checks the work branch exists. Without it, a mistyped work branch
// would name an empty staging directory and report "nothing staged" --
// indistinguishable from a correct branch with nothing staged.
func TestRunWorkComments_Staged_UnknownWorkBranch_ExitsThree(t *testing.T) {
	t.Parallel()
	srv := missingWorkBranchServer()
	var encoded any
	deps := workTestDeps(srv.client, stagingWorkspace(realTempDir(t), testReviewer), "", &encoded)
	err := runWorkComments(t.Context(), deps, []string{testRepo, testWorkBranch, "--staged"})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- work verdicts ---

// TestRunWorkVerdicts_PinTheDocumentedShape covers the exact JSON
// docs/cli-spec.md -> verdicts documents, including `round` and `stale`.
func TestRunWorkVerdicts_PinTheDocumentedShape(t *testing.T) {
	t.Parallel()
	var captured *loamv1.ListVerdictsRequest
	client := &WorkBranchClientMock{
		ListVerdictsFunc: func(_ context.Context, req *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			captured = req.Msg
			return connect.NewResponse(&loamv1.ListVerdictsResponse{Verdicts: []*loamv1.VerdictSummary{
				{Reviewer: testReviewer, Outcome: loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE, Round: 2, Stale: false},
			}}), nil
		},
	}
	var encoded any
	require.NoError(t, runWorkVerdicts(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch}))
	require.NotNil(t, captured)
	assert.Equal(t, testWorkBranch, captured.GetWorkBranch())
	assert.JSONEq(t, `[{"reviewer":"ada-lovelace-7-reviewer","outcome":"approve","round":2,"stale":false}]`, jsonOf(t, encoded))
}

// TestRunWorkVerdicts_StaleIsCopiedNotDerived feeds a response whose stale
// flags deliberately contradict what round numbers alone would suggest --
// the LOWER round is current, the HIGHER one is stale. Only a CLI that
// copies the server's flag through can reproduce it; any local
// re-derivation ("stale unless this is the highest round") flips both rows.
// Staleness is the server's to compute (internal/handler/workbranch/
// review.go -> ListVerdicts: "Staleness is DERIVED, never stored").
func TestRunWorkVerdicts_StaleIsCopiedNotDerived(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			return connect.NewResponse(&loamv1.ListVerdictsResponse{Verdicts: []*loamv1.VerdictSummary{
				{Reviewer: "alan-turing-4-reviewer", Outcome: loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE, Round: 7, Stale: true},
				{Reviewer: testReviewer, Outcome: loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL, Round: 3, Stale: false},
			}}), nil
		},
	}
	var encoded any
	require.NoError(t, runWorkVerdicts(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch}))
	rows, ok := encoded.([]workVerdictOutput)
	require.True(t, ok, "work verdicts must encode []workVerdictOutput")
	assert.Equal(t, []workVerdictOutput{
		{Reviewer: "alan-turing-4-reviewer", Outcome: "disapprove", Round: 7, Stale: true},
		{Reviewer: testReviewer, Outcome: "neutral", Round: 3, Stale: false},
	}, rows, "rows must be reported exactly as the server returned them, stale ones included")
}

// TestRunWorkVerdicts_EveryOutcomeRendersItsSpecString pins the three
// documented outcome strings against the proto enum.
func TestRunWorkVerdicts_EveryOutcomeRendersItsSpecString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		outcome loamv1.VerdictOutcome
		want    string
	}{
		{loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE, "approve"},
		{loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE, "disapprove"},
		{loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL, "neutral"},
		{loamv1.VerdictOutcome_VERDICT_OUTCOME_UNSPECIFIED, "unspecified"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, verdictOutcomeString(tt.outcome))
		})
	}
}

func TestRunWorkVerdicts_NoVerdicts_EncodesEmptyArray(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			return connect.NewResponse(&loamv1.ListVerdictsResponse{}), nil
		},
	}
	var encoded any
	require.NoError(t, runWorkVerdicts(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch}))
	assert.JSONEq(t, `[]`, jsonOf(t, encoded))
}

func TestRunWorkVerdicts_NotFound_ExitsThree(t *testing.T) {
	t.Parallel()
	client := &WorkBranchClientMock{
		ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			return nil, notFoundError()
		},
	}
	var encoded any
	err := runWorkVerdicts(t.Context(), workTestDeps(client, noResolveWorkspace(), "", &encoded), []string{testRepo, testWorkBranch})
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- router dispatch reachability ---

// TestRouterDispatch_WorkReadCommands_ReachRealHandlers proves the router
// reaches the real handlers for all five commands this bead implements (not
// the errNotImplemented stub, and not a routing usageError).
func TestRouterDispatch_WorkReadCommands_ReachRealHandlers(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	client := &WorkBranchClientMock{
		ListWorkBranchesFunc: func(context.Context, *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
			return connect.NewResponse(&loamv1.ListWorkBranchesResponse{}), nil
		},
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{Repo: testRepo, Name: testWorkBranch}}), nil
		},
		GetWorkBranchDiffFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchDiffRequest]) (*connect.Response[loamv1.GetWorkBranchDiffResponse], error) {
			return connect.NewResponse(&loamv1.GetWorkBranchDiffResponse{Diff: ""}), nil
		},
		ListCommentsFunc: func(context.Context, *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error) {
			return connect.NewResponse(&loamv1.ListCommentsResponse{}), nil
		},
		ListVerdictsFunc: func(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
			return connect.NewResponse(&loamv1.ListVerdictsResponse{}), nil
		},
	}
	var encoded any
	router := NewRouter(workTestDeps(client, stagingWorkspace(workspaceRoot, testReviewer), "", &encoded))
	for _, args := range [][]string{
		{"work", "list"},
		{"work", "list", "--repo", testRepo, "--awaiting-review", "--limit", "5"},
		{"work", "show", testRepo, testWorkBranch},
		{"work", "diff", testRepo, testWorkBranch},
		{"work", "comments", testRepo, testWorkBranch},
		{"work", "comments", testRepo, testWorkBranch, "--staged"},
		{"work", "verdicts", testRepo, testWorkBranch},
	} {
		require.NoError(t, router.Dispatch(t.Context(), args), "args %v", args)
	}
}
