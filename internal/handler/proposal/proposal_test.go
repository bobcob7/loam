package proposal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// testRepoID and testWorkBranchID are the fixed ids every default mock in
// newTestDeps agrees on, so a test asserting "the accepter was called for
// THIS branch" has a stable value to compare against.
var (
	testRepoID       = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testWorkBranchID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

// testDeps bundles every moq mock Handler needs, each pre-configured with a
// harmless, fully-specified default: an enrolled repo, one reviewed and
// unconflicted work branch with one current-round approve, an accepter and
// a PR closer that succeed. A test overriding nothing therefore exercises
// the happy path, and every failure a test DOES see comes from the one
// collaborator it deliberately changed -- never from a nil-func panic on a
// collaborator the scenario does not care about.
//
// That property is what makes the mutation testing on this package
// meaningful: breaking a load-bearing line has to turn a test red on an
// ASSERTION, and it cannot do that if an unconfigured mock panics first.
type testDeps struct {
	workBranches *workBranchStoreMock
	repos        *repoStoreMock
	verdicts     *verdictStoreMock
	accepter     *proposalAccepterMock
	prCloser     *upstreamPRCloserMock
	buf          bytes.Buffer
}

// reviewedBranch is the default work_branches row: reviewed, unconflicted,
// no PR recorded yet.
func reviewedBranch() workbranchstore.WorkBranch {
	title, description := "add a widget", "it adds a widget"
	return workbranchstore.WorkBranch{
		ID:          testWorkBranchID,
		RepoID:      testRepoID,
		Name:        "wb-9c2f1a",
		Target:      "main",
		Title:       &title,
		Description: &description,
		State:       workbranchstore.StateReviewed,
		// CreateWorkBranch writes httpauth.Identity.Identifier() here --
		// "<name>-<id>-<role>", not the bare agent name. Seeded the way the
		// production write path really writes it, deliberately: loam-ppb is
		// the open P0 that the pre-receive policy compares this column
		// against a bare LOAM_AGENT_NAME, and a fixture that quietly used
		// the bare name here would hide the shape that bug is about.
		Author:   "scout-7f3a-reviewer",
		Conflict: workbranchstore.ConflictNone,
	}
}

func newTestDeps() *testDeps {
	d := &testDeps{}
	branch := reviewedBranch()
	d.workBranches = &workBranchStoreMock{
		GetByNameFunc: func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
			wb := branch
			wb.Name = name
			return wb, nil
		},
		ListFunc: func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
			return []workbranchstore.WorkBranch{branch}, 1, nil
		},
		CloseFunc: func(_ context.Context, _ uuid.UUID, reason string) (workbranchstore.WorkBranch, error) {
			closed := branch
			closed.State = workbranchstore.StateClosed
			closed.CloseReason = &reason
			return closed, nil
		},
	}
	d.repos = &repoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: testRepoID, Name: name, ForgeHost: "forge.example.com"}, nil
		},
		GetRepoByIDFunc: func(_ context.Context, _ uuid.UUID) (reposstore.Repo, error) {
			return reposstore.Repo{ID: testRepoID, Name: "acme/widgets", ForgeHost: "forge.example.com"}, nil
		},
	}
	d.verdicts = &verdictStoreMock{
		CurrentRoundApproveCountFunc: func(_ context.Context, _ uuid.UUID) (int64, error) {
			return 1, nil
		},
		ListFunc: func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
			return []reviewstore.VerdictRecord{approve("scout-7f3a-reviewer", 2, true)}, nil
		},
	}
	d.accepter = &proposalAccepterMock{
		AcceptProposalFunc: func(_ context.Context, _ mirrorsync.RepoID, name string) (mirrorsync.AcceptResult, error) {
			return mirrorsync.AcceptResult{
				UpstreamBranch: "loam/" + name,
				PRURL:          "https://forge.example.com/acme/widgets/pulls/7",
				PRNumber:       7,
				CreatedPR:      true,
			}, nil
		},
	}
	d.prCloser = &upstreamPRCloserMock{
		ClosePRAndCleanupFunc: func(_ context.Context, _ mirrorsync.RepoID, _ string, _ int) error { return nil },
	}
	return d
}

func (d *testDeps) handler() *Handler {
	logger := testLogger(&d.buf)
	return New(d.workBranches, d.repos, d.verdicts, d.accepter, d.prCloser, handler.NewErrorMapper(logger), logger)
}

// verdict builds a VerdictRecord with the round decoration reviewstore's
// own query produces: RoundNumber plus Current, the derived
// "is this the branch's MAX(number) round" flag.
func verdict(reviewer string, outcome reviewstore.Outcome, round int32, current bool) reviewstore.VerdictRecord {
	return reviewstore.VerdictRecord{
		Verdict:     reviewstore.Verdict{ID: uuid.New(), RoundID: uuid.New(), Reviewer: reviewer, Outcome: outcome},
		RoundNumber: round,
		Current:     current,
	}
}

func approve(reviewer string, round int32, current bool) reviewstore.VerdictRecord {
	return verdict(reviewer, reviewstore.OutcomeApprove, round, current)
}

// adminCtx is the context every admin RPC in this package sees in
// production: httpauth.Auth.AdminOnly marks it before the request reaches
// a handler.
func adminCtx(t *testing.T) context.Context {
	t.Helper()
	return httpauth.WithAdmin(t.Context())
}

func requireConnectCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, want, connErr.Code(), "connect code for %v", err)
}

// ---------------------------------------------------------------------
// AcceptProposal
// ---------------------------------------------------------------------

// TestAcceptProposal_CurrentRoundApprove_DelegatesToAccepter is the happy
// path: the precondition passes, the handler performs no git or forge work
// of its own, and the accepter's answer -- not something the handler
// synthesized -- is what reaches the wire.
func TestAcceptProposal_CurrentRoundApprove_DelegatesToAccepter(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Equal(t, "https://forge.example.com/acme/widgets/pulls/7", resp.Msg.GetPrUrl())
	assert.Equal(t, "loam/wb-9c2f1a", resp.Msg.GetUpstreamBranch())
	calls := d.accepter.AcceptProposalCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, mirrorsync.RepoID("acme/widgets"), calls[0].Repo)
	assert.Equal(t, "wb-9c2f1a", calls[0].WorkBranchName)
}

// TestAcceptProposal_NoApproveInCurrentRound_FailedPrecondition is THE
// precondition this bead owns (docs/sync-spec.md -> Proposal Acceptance:
// ">= 1 non-stale approve verdict"), the one StoreProposalAccepter
// deliberately does not enforce. A branch with no current-round approve is
// refused BEFORE the accepter is reached, so nothing is pushed upstream.
func TestAcceptProposal_NoApproveInCurrentRound_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.verdicts.CurrentRoundApproveCountFunc = func(_ context.Context, _ uuid.UUID) (int64, error) { return 0, nil }
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodeFailedPrecondition)
	assert.Empty(t, d.accepter.AcceptProposalCalls(), "nothing may be pushed upstream for an unapproved branch")
}

// TestAcceptProposal_OnlyStaleApprove_FailedPrecondition pins the
// "non-stale" half of the rule at the seam it is actually decided:
// CurrentRoundApproveCount counts the CURRENT round only, so an approve
// left behind by an earlier round contributes 0 and the accept is refused.
// This is why the count is a store query and not something walked in Go --
// the staleness derivation stays in the one SQL expression that owns it.
func TestAcceptProposal_OnlyStaleApprove_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.verdicts.CurrentRoundApproveCountFunc = func(_ context.Context, _ uuid.UUID) (int64, error) { return 0, nil }
	d.verdicts.ListFunc = func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
		return []reviewstore.VerdictRecord{approve("scout-7f3a-reviewer", 1, false)}, nil
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodeFailedPrecondition)
	assert.Empty(t, d.accepter.AcceptProposalCalls())
}

// TestAcceptProposal_ApproveAndDisapprove_IsAccepted pins the judgment call
// this bead had to make and documents it as a TEST, not just a comment: a
// disapprove does NOT veto. No spec in this tree states a "no outstanding
// disapprove" rule, so the >= 1 approve rule is applied literally and the
// admin -- who can see both verdicts in the queue -- decides.
func TestAcceptProposal_ApproveAndDisapprove_IsAccepted(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.verdicts.CurrentRoundApproveCountFunc = func(_ context.Context, _ uuid.UUID) (int64, error) { return 1, nil }
	d.verdicts.ListFunc = func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
		return []reviewstore.VerdictRecord{
			approve("scout-7f3a-reviewer", 2, true),
			verdict("nib-0011-reviewer", reviewstore.OutcomeDisapprove, 2, true),
		}, nil
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Len(t, d.accepter.AcceptProposalCalls(), 1)
}

// TestAcceptProposal_NotReviewed_FailedPrecondition proves the state gate is
// enforced here and not merely delegated: the accepter's own refusal uses
// an unexported sentinel that cannot be classified at this boundary, so a
// draft branch reaching it would answer CodeInternal instead of
// CodeFailedPrecondition.
func TestAcceptProposal_NotReviewed_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		wb := reviewedBranch()
		wb.Name, wb.State = name, workbranchstore.StateDraft
		return wb, nil
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodeFailedPrecondition)
	assert.Empty(t, d.accepter.AcceptProposalCalls())
}

// TestAcceptProposal_Conflicted_FailedPrecondition covers
// features/admin-proposals.feature -> "A conflicted work branch cannot be
// accepted": approved and reviewed, but flagged against its target.
func TestAcceptProposal_Conflicted_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		wb := reviewedBranch()
		wb.Name, wb.Conflict = name, workbranchstore.ConflictFlagged
		return wb, nil
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodeFailedPrecondition)
	assert.Empty(t, d.accepter.AcceptProposalCalls())
}

// TestAcceptProposal_AlreadyRecordedPR_StillDelegates is the idempotency
// guard from this handler's side (features/admin-proposals.feature ->
// "Re-accepting a caught-up work branch updates the existing PR"). A
// recorded upstream_pr_number must NOT make the handler refuse: the accept
// has to reach the accepter so the branch is fast-forwarded, and the
// accepter's own null-check plus guarded UPDATE are what stop a second PR
// from being opened. A "already accepted" short-circuit here would defeat
// the documented flow, which is exactly what this test forbids.
func TestAcceptProposal_AlreadyRecordedPR_StillDelegates(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	prNumber, prURL := int32(7), "https://forge.example.com/acme/widgets/pulls/7"
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		wb := reviewedBranch()
		wb.Name, wb.UpstreamPRNumber, wb.UpstreamPRURL = name, &prNumber, &prURL
		return wb, nil
	}
	d.accepter.AcceptProposalFunc = func(_ context.Context, _ mirrorsync.RepoID, name string) (mirrorsync.AcceptResult, error) {
		return mirrorsync.AcceptResult{UpstreamBranch: "loam/" + name, PRURL: prURL, PRNumber: 7, CreatedPR: false}, nil
	}
	resp, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Equal(t, prURL, resp.Msg.GetPrUrl())
	require.Len(t, d.accepter.AcceptProposalCalls(), 1, "a re-accept must reach the accepter so the branch is fast-forwarded")
}

// TestAcceptProposal_ForgeRefusal_FailedPrecondition proves the "the forge
// REFUSED" half of the distinction mapAcceptErr exists to draw: an invalid
// token is something the admin can fix, and it is reported as a failed
// precondition rather than collapsed into CodeInternal.
func TestAcceptProposal_ForgeRefusal_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.accepter.AcceptProposalFunc = func(_ context.Context, _ mirrorsync.RepoID, _ string) (mirrorsync.AcceptResult, error) {
		return mirrorsync.AcceptResult{}, fmt.Errorf("opening upstream PR: %w", forge.ErrInvalidToken)
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodeFailedPrecondition)
}

// TestAcceptProposal_ForgeRefusal_ChainSurvivesMapping is loam-jv8f's
// acceptance test for mapAcceptErr: it used to format err with %v instead
// of %w, so the forge's own refusal text still rendered but
// errors.Is(mapped, forge.ErrInvalidToken) was false -- the same defect
// shape as loam-blc/loam-dq0o/loam-c4ab, here as a %v variant rather than a
// dropped argument.
func TestAcceptProposal_ForgeRefusal_ChainSurvivesMapping(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.accepter.AcceptProposalFunc = func(_ context.Context, _ mirrorsync.RepoID, _ string) (mirrorsync.AcceptResult, error) {
		return mirrorsync.AcceptResult{}, fmt.Errorf("opening upstream PR: %w", forge.ErrInvalidToken)
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Contains(t, connErr.Message(), forge.ErrInvalidToken.Error(), "the forge's own refusal text must still render")
	assert.ErrorIs(t, err, forge.ErrInvalidToken, "the sentinel must survive the mapping via errors.Is, not just as rendered text -- %v formatting broke this even though the message looked fine")
	assert.ErrorIs(t, err, handler.ErrFailedPrecondition)
}

// TestAcceptProposal_TransportFailure_Internal is the other half: a call
// that FAILED (the push never completed) is not a refusal the admin can
// act on, so it stays loud -- CodeInternal, and logged.
func TestAcceptProposal_TransportFailure_Internal(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.accepter.AcceptProposalFunc = func(_ context.Context, _ mirrorsync.RepoID, _ string) (mirrorsync.AcceptResult, error) {
		return mirrorsync.AcceptResult{}, errors.New("dial tcp: connection refused")
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodeInternal)
	assert.Contains(t, d.buf.String(), "unmapped handler error")
}

// TestAcceptProposal_NotAdmin_PermissionDenied proves the per-RPC admin
// gate this package adds on top of the routing-level AdminOnly wrapper, and
// proves it refuses BEFORE anything is pushed.
func TestAcceptProposal_NotAdmin_PermissionDenied(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().AcceptProposal(t.Context(), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/widgets", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodePermissionDenied)
	assert.Empty(t, d.accepter.AcceptProposalCalls())
	assert.Empty(t, d.repos.GetRepoByNameCalls(), "an unauthorized caller must not even probe which repos exist")
}

// TestAcceptProposal_UnknownRepo_NotFound keeps a missing enrollment
// distinguishable from a precondition failure.
func TestAcceptProposal_UnknownRepo_NotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.repos.GetRepoByNameFunc = func(_ context.Context, _ string) (reposstore.Repo, error) {
		return reposstore.Repo{}, reposstore.ErrNotFound
	}
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: "acme/ghost", WorkBranch: "wb-9c2f1a"}))
	requireConnectCode(t, err, connect.CodeNotFound)
}

// TestAcceptProposal_MissingFields_InvalidArgument covers the empty request.
func TestAcceptProposal_MissingFields_InvalidArgument(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().AcceptProposal(adminCtx(t), connect.NewRequest(&adminv1.AcceptProposalRequest{}))
	requireConnectCode(t, err, connect.CodeInvalidArgument)
}

// ---------------------------------------------------------------------
// ListProposals
// ---------------------------------------------------------------------

// listDeps seeds the queue with the given branches, each resolving to its
// own repo name, and returns the deps so the test can override verdicts.
func listDeps(branches ...workbranchstore.WorkBranch) *testDeps {
	d := newTestDeps()
	d.workBranches.ListFunc = func(_ context.Context, filter workbranchstore.ListFilter, _, offset int32) ([]workbranchstore.WorkBranch, int64, error) {
		if offset > 0 {
			return nil, int64(len(branches)), nil
		}
		return branches, int64(len(branches)), nil
	}
	return d
}

func branchNamed(name string, mutate func(*workbranchstore.WorkBranch)) workbranchstore.WorkBranch {
	wb := reviewedBranch()
	wb.ID = uuid.New()
	wb.Name = name
	if mutate != nil {
		mutate(&wb)
	}
	return wb
}

// TestListProposals_OnlyBranchesWithACurrentApproveAreListed is
// features/admin-proposals.feature -> "The queue lists reviewed work
// branches that have an approval": the approved branch is listed, the one
// with only a disapprove is not.
func TestListProposals_OnlyBranchesWithACurrentApproveAreListed(t *testing.T) {
	t.Parallel()
	approved := branchNamed("wb-approved", nil)
	disapproved := branchNamed("wb-disapproved", nil)
	d := listDeps(approved, disapproved)
	d.verdicts.CurrentRoundApproveCountFunc = func(_ context.Context, id uuid.UUID) (int64, error) {
		if id == approved.ID {
			return 1, nil
		}
		return 0, nil
	}
	resp, err := d.handler().ListProposals(adminCtx(t), connect.NewRequest(&adminv1.ListProposalsRequest{}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetProposals(), 1)
	assert.Equal(t, "wb-approved", resp.Msg.GetProposals()[0].GetWorkBranch().GetName())
	assert.Equal(t, uint32(1), resp.Msg.GetPageInfo().GetTotal(), "total counts proposals, not reviewed branches")
}

// TestListProposals_ConflictedBranchIsNotAProposal proves the conflict
// clause of the predicate: a reviewed, approved branch flagged against its
// target is not awaiting an admin decision -- it is awaiting a catch-up.
func TestListProposals_ConflictedBranchIsNotAProposal(t *testing.T) {
	t.Parallel()
	conflicted := branchNamed("wb-conflicted", func(wb *workbranchstore.WorkBranch) {
		wb.Conflict = workbranchstore.ConflictReset
	})
	d := listDeps(branchNamed("wb-clean", nil), conflicted)
	resp, err := d.handler().ListProposals(adminCtx(t), connect.NewRequest(&adminv1.ListProposalsRequest{}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetProposals(), 1)
	assert.Equal(t, "wb-clean", resp.Msg.GetProposals()[0].GetWorkBranch().GetName())
	for _, call := range d.verdicts.CurrentRoundApproveCountCalls() {
		assert.NotEqual(t, conflicted.ID, call.WorkBranchID, "a conflicted branch is not even a verdict candidate")
	}
}

// TestListProposals_CarriesCurrentRoundVerdictsOnly pins the Proposal
// message's own contract ("This round's verdicts") against the broader
// history view WorkBranchService.ListVerdicts returns: a prior round's
// verdict is dropped here even though it belongs to a reviewer with no
// current-round vote.
func TestListProposals_CarriesCurrentRoundVerdictsOnly(t *testing.T) {
	t.Parallel()
	d := listDeps(branchNamed("wb-9c2f1a", nil))
	d.verdicts.ListFunc = func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
		return []reviewstore.VerdictRecord{
			approve("scout-7f3a-reviewer", 2, true),
			verdict("nib-0011-reviewer", reviewstore.OutcomeDisapprove, 1, false),
		}, nil
	}
	resp, err := d.handler().ListProposals(adminCtx(t), connect.NewRequest(&adminv1.ListProposalsRequest{}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetProposals(), 1)
	verdicts := resp.Msg.GetProposals()[0].GetVerdicts()
	require.Len(t, verdicts, 1)
	assert.Equal(t, "scout-7f3a-reviewer", verdicts[0].GetReviewer())
	assert.Equal(t, loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE, verdicts[0].GetOutcome())
	assert.False(t, verdicts[0].GetStale())
	assert.Equal(t, uint32(2), verdicts[0].GetRound())
}

// TestListProposals_AcceptedBranchStaysListed pins the deliberate
// over-inclusion documented on ListProposals: a branch with a recorded PR
// is still listed, because nothing in the schema can answer "is the PR's
// branch behind the work branch" and under-including would hide the
// re-accept-after-catch-up case entirely (loam-cgg).
//
// It is here so that the day loam-cgg lands, this test fails and forces the
// documented behaviour to be revisited rather than silently changed.
func TestListProposals_AcceptedBranchStaysListed(t *testing.T) {
	t.Parallel()
	prNumber, prURL := int32(7), "https://forge.example.com/acme/widgets/pulls/7"
	accepted := branchNamed("wb-accepted", func(wb *workbranchstore.WorkBranch) {
		wb.UpstreamPRNumber, wb.UpstreamPRURL = &prNumber, &prURL
	})
	d := listDeps(accepted)
	resp, err := d.handler().ListProposals(adminCtx(t), connect.NewRequest(&adminv1.ListProposalsRequest{}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetProposals(), 1)
	assert.Equal(t, prURL, resp.Msg.GetProposals()[0].GetWorkBranch().GetUpstreamPrUrl())
}

// TestListProposals_PaginatesTheFilteredResult proves limit/offset applies
// to proposals, not to the reviewed-branch scan. With three reviewed
// branches of which two are proposals, a limit of 1 at offset 1 must return
// the SECOND proposal -- not whatever the second reviewed row happened to
// be, and not an empty page.
func TestListProposals_PaginatesTheFilteredResult(t *testing.T) {
	t.Parallel()
	first := branchNamed("wb-first", nil)
	skipped := branchNamed("wb-skipped", nil)
	second := branchNamed("wb-second", nil)
	d := listDeps(first, skipped, second)
	d.verdicts.CurrentRoundApproveCountFunc = func(_ context.Context, id uuid.UUID) (int64, error) {
		if id == skipped.ID {
			return 0, nil
		}
		return 1, nil
	}
	resp, err := d.handler().ListProposals(adminCtx(t), connect.NewRequest(&adminv1.ListProposalsRequest{
		Page: &loamv1.Page{Limit: 1, Offset: 1},
	}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetProposals(), 1)
	assert.Equal(t, "wb-second", resp.Msg.GetProposals()[0].GetWorkBranch().GetName())
	assert.Equal(t, uint32(2), resp.Msg.GetPageInfo().GetTotal())
}

// TestListProposals_ScansEveryCandidatePage proves the candidate scan pages
// through the whole reviewed set rather than reading only the first page: a
// proposal on page two must still reach the queue.
func TestListProposals_ScansEveryCandidatePage(t *testing.T) {
	t.Parallel()
	page1 := branchNamed("wb-page-one", nil)
	page2 := branchNamed("wb-page-two", nil)
	d := newTestDeps()
	d.workBranches.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, offset int32) ([]workbranchstore.WorkBranch, int64, error) {
		switch offset {
		case 0:
			return []workbranchstore.WorkBranch{page1}, 2, nil
		case 1:
			return []workbranchstore.WorkBranch{page2}, 2, nil
		default:
			return nil, 2, nil
		}
	}
	resp, err := d.handler().ListProposals(adminCtx(t), connect.NewRequest(&adminv1.ListProposalsRequest{}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetProposals(), 2)
	assert.Equal(t, "wb-page-two", resp.Msg.GetProposals()[1].GetWorkBranch().GetName())
}

// TestListProposals_FiltersOnReviewedState proves the candidate query asks
// the store for reviewed branches rather than pulling every work branch in
// the system and discarding most of them in Go.
func TestListProposals_FiltersOnReviewedState(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().ListProposals(adminCtx(t), connect.NewRequest(&adminv1.ListProposalsRequest{}))
	require.NoError(t, err)
	calls := d.workBranches.ListCalls()
	require.NotEmpty(t, calls)
	assert.Equal(t, workbranchstore.StateReviewed, calls[0].Filter.State)
	assert.Nil(t, calls[0].Filter.RepoID, "the queue spans every enrolled repo")
}

// TestListProposals_NotAdmin_PermissionDenied proves the queue is not
// readable without the admin superuser.
func TestListProposals_NotAdmin_PermissionDenied(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().ListProposals(t.Context(), connect.NewRequest(&adminv1.ListProposalsRequest{}))
	requireConnectCode(t, err, connect.CodePermissionDenied)
	assert.Empty(t, d.workBranches.ListCalls())
}

// ---------------------------------------------------------------------
// CloseWorkBranch
// ---------------------------------------------------------------------

// TestCloseWorkBranch_RecordsReasonAndReturnsClosedBranch is
// features/admin-proposals.feature -> "Closing a work branch ends it".
func TestCloseWorkBranch_RecordsReasonAndReturnsClosedBranch(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "superseded by a different approach",
	}))
	require.NoError(t, err)
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED, resp.Msg.GetWorkBranch().GetState())
	calls := d.workBranches.CloseCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "superseded by a different approach", calls[0].Reason)
	assert.Equal(t, testWorkBranchID, calls[0].ID)
}

// TestCloseWorkBranch_WithOpenPR_ClosesItUpstream is
// features/admin-proposals.feature -> "Closing a work branch closes its
// upstream PR": Loam opened it, Loam closes it.
func TestCloseWorkBranch_WithOpenPR_ClosesItUpstream(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	prNumber := int32(7)
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		wb := reviewedBranch()
		wb.Name, wb.UpstreamPRNumber = name, &prNumber
		return wb, nil
	}
	_, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "not going upstream",
	}))
	require.NoError(t, err)
	calls := d.prCloser.ClosePRAndCleanupCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, mirrorsync.RepoID("acme/widgets"), calls[0].Repo)
	assert.Equal(t, "wb-9c2f1a", calls[0].WorkBranchName)
	assert.Equal(t, 7, calls[0].PrNumber)
}

// TestCloseWorkBranch_NoRecordedPR_DoesNotTouchTheForge proves a branch Loam
// never accepted is closed without a forge round trip at all.
func TestCloseWorkBranch_NoRecordedPR_DoesNotTouchTheForge(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "abandoned",
	}))
	require.NoError(t, err)
	assert.Empty(t, d.prCloser.ClosePRAndCleanupCalls())
}

// TestCloseWorkBranch_UpstreamCloseFails_RPCStillSucceeds pins the
// best-effort contract: the work_branches row is already closed by the time
// the forge is touched, so failing the RPC would tell the admin the close
// did not happen when it demonstrably did -- and a retry would then answer
// ErrIllegalTransition on the already-closed row.
func TestCloseWorkBranch_UpstreamCloseFails_RPCStillSucceeds(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	prNumber := int32(7)
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		wb := reviewedBranch()
		wb.Name, wb.UpstreamPRNumber = name, &prNumber
		return wb, nil
	}
	d.prCloser.ClosePRAndCleanupFunc = func(_ context.Context, _ mirrorsync.RepoID, _ string, _ int) error {
		return errors.New("forge unreachable")
	}
	resp, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "abandoned",
	}))
	require.NoError(t, err)
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED, resp.Msg.GetWorkBranch().GetState())
	assert.Contains(t, d.buf.String(), "its upstream PR could not be closed")
}

// TestCloseWorkBranch_ClosesTheRowBeforeTheForge pins the ORDER, which is
// the property that makes the best-effort rule above safe: if the forge
// were closed first and the row close then failed, the PR would be gone
// upstream with the branch still live here.
func TestCloseWorkBranch_ClosesTheRowBeforeTheForge(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	prNumber := int32(7)
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		wb := reviewedBranch()
		wb.Name, wb.UpstreamPRNumber = name, &prNumber
		return wb, nil
	}
	var order []string
	d.workBranches.CloseFunc = func(_ context.Context, _ uuid.UUID, reason string) (workbranchstore.WorkBranch, error) {
		order = append(order, "store")
		closed := reviewedBranch()
		closed.State, closed.CloseReason = workbranchstore.StateClosed, &reason
		return closed, nil
	}
	d.prCloser.ClosePRAndCleanupFunc = func(_ context.Context, _ mirrorsync.RepoID, _ string, _ int) error {
		order = append(order, "forge")
		return nil
	}
	_, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "abandoned",
	}))
	require.NoError(t, err)
	assert.Equal(t, []string{"store", "forge"}, order)
}

// TestCloseWorkBranch_StoreRefusesTerminalBranch_FailedPrecondition proves a
// branch that already reached complete or closed answers a precondition
// failure, and that the forge is never touched for it.
func TestCloseWorkBranch_StoreRefusesTerminalBranch_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	prNumber := int32(7)
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		wb := reviewedBranch()
		wb.Name, wb.State, wb.UpstreamPRNumber = name, workbranchstore.StateComplete, &prNumber
		return wb, nil
	}
	d.workBranches.CloseFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("closing work branch: %w", workbranchstore.ErrIllegalTransition)
	}
	_, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "too late",
	}))
	requireConnectCode(t, err, connect.CodeFailedPrecondition)
	assert.Empty(t, d.prCloser.ClosePRAndCleanupCalls(), "a refused close must not close the upstream PR")
}

// TestCloseWorkBranch_TerminalState_MessageNamesIllegalTransition is
// loam-blc's and loam-dq0o's acceptance test mirrored a third time for
// mapWorkBranchStoreErr's close path (loam-c4ab): it used to substitute a
// hand-written "the work branch has already reached a terminal state"
// message for err entirely, so errors.Is(mapped,
// workbranchstore.ErrIllegalTransition) was false even though the rendered
// text read fine. err already names the work branch id (workbranchstore's
// own "%s work branch %s: %w" wrapping), so the mapped error must preserve
// it -- both in the rendered message (the CLI prints connectErr.Message()
// directly, per docs/cli-spec.md -> Exit Codes & Errors) and via errors.Is,
// which callers may still match on.
func TestCloseWorkBranch_TerminalState_MessageNamesIllegalTransition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	// The stub names a FRESH random UUID, distinct from testWorkBranchID --
	// not "wb-9c2f1a"/"acme/widgets" -- and that choice is what lets this
	// test fail at all: the handler's own context prefix ("closing work
	// branch acme/widgets/wb-9c2f1a") never contains this id, so a stub
	// naming it cannot separate "err was preserved" from "only the bare
	// sentinel was wrapped".
	staleID := uuid.New()
	d.workBranches.CloseFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("closing work branch %s: %w", staleID, workbranchstore.ErrIllegalTransition)
	}
	_, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "too late",
	}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), staleID.String(), "the message must name WHICH work branch the store refused -- the store's own wrapping appears nowhere in the handler's own context prefix, so only preserving err can put it there")
	assert.Contains(t, connectErr.Message(), workbranchstore.ErrIllegalTransition.Error(), "and the sentinel's own text must survive, not just terminate in a hand-written substitute message")
	assert.ErrorIs(t, err, workbranchstore.ErrIllegalTransition, "the sentinel must survive the mapping, not just the Connect code")
	assert.ErrorIs(t, err, handler.ErrFailedPrecondition)
}

// TestCloseWorkBranch_EmptyBody_InvalidArgument: close_reason is what the
// author is told, so closing without one is refused rather than recorded as
// an empty string.
func TestCloseWorkBranch_EmptyBody_InvalidArgument(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a",
	}))
	requireConnectCode(t, err, connect.CodeInvalidArgument)
	assert.Empty(t, d.workBranches.CloseCalls())
}

// TestCloseWorkBranch_NotAdmin_PermissionDenied proves the per-RPC admin
// gate covers the destructive close path too.
func TestCloseWorkBranch_NotAdmin_PermissionDenied(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().CloseWorkBranch(t.Context(), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-9c2f1a", Body: "no",
	}))
	requireConnectCode(t, err, connect.CodePermissionDenied)
	assert.Empty(t, d.workBranches.CloseCalls())
}

// TestCloseWorkBranch_UnknownWorkBranch_NotFound keeps a typo'd branch name
// distinguishable from a refusal.
func TestCloseWorkBranch_UnknownWorkBranch_NotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, workbranchstore.ErrNotFound
	}
	_, err := d.handler().CloseWorkBranch(adminCtx(t), connect.NewRequest(&adminv1.CloseWorkBranchRequest{
		Repo: "acme/widgets", WorkBranch: "wb-ghost", Body: "gone",
	}))
	requireConnectCode(t, err, connect.CodeNotFound)
}
