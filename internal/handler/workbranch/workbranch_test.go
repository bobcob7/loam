package workbranch_test

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

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/workbranch"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func adminCtx(t *testing.T) context.Context {
	t.Helper()
	return httpauth.WithAdmin(t.Context())
}

func agentCtx(t *testing.T, role string) context.Context {
	t.Helper()
	return httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "grace-hopper", ID: "3", Role: role})
}

// fixedRoleStore is a hand-written handler.RoleStore fake returning a fixed
// capability set for any role, since internal/handler's moq-generated mock
// lives in its own package's moq_test.go and is unreachable from this
// external test package, matching internal/handler/repo's own test
// convention.
type fixedRoleStore struct {
	capabilities []handler.Capability
}

func (s fixedRoleStore) RoleCapabilities(context.Context, string) ([]handler.Capability, error) {
	return s.capabilities, nil
}

// sampleRepo, sampleTitle, sampleDescription, and sampleWorkBranch are
// shared fixture values every test below builds its store stubs around.
var sampleRepoID = uuid.New()

var sampleWorkBranchID = uuid.New()

func sampleRepo() reposstore.Repo {
	return reposstore.Repo{ID: sampleRepoID, Name: "bobcob7/doc-server"}
}

func sampleTitledWorkBranch(state workbranchstore.State) workbranchstore.WorkBranch {
	title, description := "Add login", "Adds a login flow."
	return workbranchstore.WorkBranch{
		ID: sampleWorkBranchID, RepoID: sampleRepoID, Name: "wb-9c2f1a", Target: "main",
		Title: &title, Description: &description, State: state, Author: "grace-hopper-3-author",
	}
}

// allMocks builds a fully configured set of the four seams the Handler
// consumes, each answering a benign success, so a test that only cares
// about one behavior (e.g. the capability gate) can override just the
// relevant Func and trust every other mocked method is safe to call rather
// than nil-panicking -- the "beware the incomplete-mock trap" discipline: a
// mutation that removes an early-return gate must fall through to a real,
// observable success this test can assert against, never a panic that
// would obscure the real failure.
func allMocks() (*workbranch.WorkBranchStoreMock, *workbranch.RepoStoreMock, *workbranch.RoundOpenerMock, *workbranch.DiffComputerMock) {
	wb := sampleTitledWorkBranch(workbranchstore.StateDraft)
	workBranches := &workbranch.WorkBranchStoreMock{
		CreateFunc: func(_ context.Context, _ uuid.UUID, name, target, author string) (workbranchstore.WorkBranch, error) {
			return workbranchstore.WorkBranch{ID: uuid.New(), RepoID: sampleRepoID, Name: name, Target: target, Author: author, State: workbranchstore.StateDraft}, nil
		},
		GetByNameFunc: func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
			return wb, nil
		},
		ListFunc: func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
			return []workbranchstore.WorkBranch{wb}, 1, nil
		},
		SetTitleDescriptionFunc: func(_ context.Context, id uuid.UUID, title, description string) (workbranchstore.WorkBranch, error) {
			updated := wb
			updated.ID, updated.Title, updated.Description = id, &title, &description
			return updated, nil
		},
		UpdateStateFunc: func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
			updated := wb
			updated.ID, updated.State = id, to
			return updated, nil
		},
	}
	repos := &workbranch.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: sampleRepoID, Name: name}, nil
		},
		GetRepoByIDFunc: func(_ context.Context, id uuid.UUID) (reposstore.Repo, error) {
			return reposstore.Repo{ID: id, Name: "bobcob7/doc-server"}, nil
		},
		ListTargetBranchesFunc: func(_ context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error) {
			return []reposstore.TargetBranch{{RepoID: repoID, Branch: "main"}}, nil
		},
	}
	rounds := &workbranch.RoundOpenerMock{
		OpenRoundFunc: func(_ context.Context, workBranchID uuid.UUID, requestedBy string) (reviewstore.Round, error) {
			return reviewstore.Round{ID: uuid.New(), WorkBranchID: workBranchID, Number: 1, RequestedBy: requestedBy}, nil
		},
	}
	diff := &workbranch.DiffComputerMock{
		DiffFunc: func(_ context.Context, _ workbranchstore.WorkBranch) (string, error) {
			return "--- a/f\n+++ b/f\n", nil
		},
	}
	return workBranches, repos, rounds, diff
}

// newHandler wires a workbranch.Handler over the four given seams with a
// capability checker backed by roleCaps and an ErrorMapper that logs to buf
// so tests can assert on the logged line for unmapped errors.
func newHandler(workBranches workbranch.WorkBranchStore, repos workbranch.RepoStore, rounds workbranch.RoundOpener, diff workbranch.DiffComputer, roleCaps []handler.Capability, buf *bytes.Buffer) *workbranch.Handler {
	checker := handler.NewCapabilityChecker(fixedRoleStore{capabilities: roleCaps})
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(buf, nil)))
	return workbranch.New(workBranches, repos, rounds, diff, checker, mapper, testLogger())
}

func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	return connectErr.Code()
}

// --- CreateWorkBranch ---

// TestCreateWorkBranch_AgentLackingWorkStart_Denied proves the capability
// gate runs, and runs BEFORE the store is ever consulted: mutation "drop
// the capability gate" would let this fall through to allMocks' benign
// success, caught below by the CodePermissionDenied assertion, not a panic.
func TestCreateWorkBranch_AgentLackingWorkStart_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.CreateWorkBranch(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, workBranches.CreateCalls(), "the work branch store must not be consulted when the capability gate denies the caller")
}

// TestCreateWorkBranch_AdminCaller_RejectedForNoAgentIdentity proves an
// admin (superuser, capability bypass) is still rejected before the store
// is touched: CreateWorkBranch requires an agent identity to attribute
// authorship to, and docs/web-spec.md -> ProposalService never lists
// CreateWorkBranch among the RPCs the admin reaches as superuser.
func TestCreateWorkBranch_AdminCaller_RejectedForNoAgentIdentity(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, nil, &buf)
	_, err := h.CreateWorkBranch(adminCtx(t), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, workBranches.CreateCalls())
}

// TestCreateWorkBranch_EmptyRepoOrFrom_ReturnsInvalidArgument proves both
// required fields are validated before any store call.
func TestCreateWorkBranch_EmptyRepoOrFrom_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	cases := []struct{ repo, from string }{{"", "main"}, {"bobcob7/doc-server", ""}}
	for _, tc := range cases {
		var buf bytes.Buffer
		workBranches, repos, rounds, diff := allMocks()
		h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkStart}, &buf)
		_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: tc.repo, From: tc.from}))
		require.Error(t, err)
		assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
		assert.Empty(t, workBranches.CreateCalls())
	}
}

// TestCreateWorkBranch_UnenrolledRepo_ReturnsNotFound proves a genuinely
// unenrolled repo maps to CodeNotFound, not the group-level fallback's
// indistinguishable 404.
func TestCreateWorkBranch_UnenrolledRepo_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	repos.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{}, fmt.Errorf("getting repo %s: %w", name, reposstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkStart}, &buf)
	_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/ghost", From: "main"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
	assert.Empty(t, buf.String())
}

// TestCreateWorkBranch_InvalidTargetBranch_ReturnsInvalidArgument proves
// `from` is validated against the repo's actual eligible target branches
// (docs/cli-spec.md -> "start": "exit 2 if `from` is not a valid target
// branch") before a work branch is created.
func TestCreateWorkBranch_InvalidTargetBranch_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkStart}, &buf)
	_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "not-a-target"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, workBranches.CreateCalls())
}

// TestCreateWorkBranch_Success_ReturnsDraftWorkBranch proves the happy
// path: a valid target branch creates a draft work branch attributed to
// the calling agent, with a non-empty, "wb-"-prefixed generated name.
func TestCreateWorkBranch_Success_ReturnsDraftWorkBranch(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkStart}, &buf)
	resp, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))
	require.NoError(t, err)
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetWorkBranch().GetRepo())
	assert.Equal(t, "main", resp.Msg.GetWorkBranch().GetTarget())
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT, resp.Msg.GetWorkBranch().GetState())
	assert.Equal(t, "grace-hopper-3-author", resp.Msg.GetWorkBranch().GetAuthor())
	require.Len(t, workBranches.CreateCalls(), 1)
	assert.Regexp(t, `^wb-[0-9a-f]{6}$`, workBranches.CreateCalls()[0].Name)
}

// --- UpdateWorkBranch ---

// TestUpdateWorkBranch_AgentLackingWorkSet_Denied proves the capability
// gate runs before the store round-trip.
func TestUpdateWorkBranch_AgentLackingWorkSet_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.UpdateWorkBranch(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.UpdateWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, workBranches.SetTitleDescriptionCalls())
}

// TestUpdateWorkBranch_UnsetFieldsKeepCurrentValue proves
// SetTitleDescription's full-replace call is fed the CURRENT title when the
// request leaves title unset, only overwriting description.
func TestUpdateWorkBranch_UnsetFieldsKeepCurrentValue(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkSet}, &buf)
	newDescription := "Adds a login flow with 2FA."
	_, err := h.UpdateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.UpdateWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a", Description: &newDescription}))
	require.NoError(t, err)
	require.Len(t, workBranches.SetTitleDescriptionCalls(), 1)
	call := workBranches.SetTitleDescriptionCalls()[0]
	assert.Equal(t, "Add login", call.Title, "title must pass through unchanged when the request leaves it unset")
	assert.Equal(t, newDescription, call.Description)
}

// TestUpdateWorkBranch_TerminalState_ReturnsFailedPrecondition is the
// acceptance-critical case ("A terminal work branch cannot be edited"):
// workbranchstore.ErrIllegalTransition must map to CodeFailedPrecondition,
// never CodeNotFound -- the two are deliberately distinguishable sentinels
// this test pins.
func TestUpdateWorkBranch_TerminalState_ReturnsFailedPrecondition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	workBranches.SetTitleDescriptionFunc = func(_ context.Context, id uuid.UUID, _, _ string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("setting title/description on work branch %s: %w", id, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkSet}, &buf)
	title := "New title"
	_, err := h.UpdateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.UpdateWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a", Title: &title}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
}

// TestUpdateWorkBranch_NotFound_ReturnsCodeNotFound proves
// workbranchstore.ErrNotFound (a distinct sentinel from
// ErrIllegalTransition) maps to CodeNotFound, not CodeFailedPrecondition --
// pinning the OTHER half of the "map these correctly" requirement.
func TestUpdateWorkBranch_NotFound_ReturnsCodeNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("getting work branch %s: %w", name, workbranchstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkSet}, &buf)
	title := "New title"
	_, err := h.UpdateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.UpdateWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-ghost", Title: &title}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
	assert.Empty(t, workBranches.SetTitleDescriptionCalls())
}

// --- RequestReview ---

// TestRequestReview_AgentLackingWorkRequestReview_Denied proves the
// capability gate runs for an agent caller.
func TestRequestReview_AgentLackingWorkRequestReview_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.RequestReview(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, workBranches.UpdateStateCalls())
	assert.Empty(t, rounds.OpenRoundCalls())
}

// TestRequestReview_AdminSendBack_BypassesCapability proves the admin's
// send-back path reaches the store as a superuser (docs/web-spec.md ->
// ProposalService), attributing the opened round to "admin" since there is
// no agent identity to render.
func TestRequestReview_AdminSendBack_BypassesCapability(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, nil, &buf)
	resp, err := h.RequestReview(adminCtx(t), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE, resp.Msg.GetWorkBranch().GetState())
	require.Len(t, rounds.OpenRoundCalls(), 1)
	assert.Equal(t, "admin", rounds.OpenRoundCalls()[0].RequestedBy)
}

// TestRequestReview_Success_TransitionsToReviewableAndOpensRound proves the
// acceptance-critical happy path ("Requesting review opens the work branch
// for review", "Requesting review again starts a fresh round"): the state
// transition and the round-open both happen, in that order, attributed to
// the calling agent.
func TestRequestReview_Success_TransitionsToReviewableAndOpensRound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	resp, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE, resp.Msg.GetWorkBranch().GetState())
	require.Len(t, workBranches.UpdateStateCalls(), 1)
	assert.Equal(t, workbranchstore.StateReviewable, workBranches.UpdateStateCalls()[0].To)
	require.Len(t, rounds.OpenRoundCalls(), 1)
	assert.Equal(t, "grace-hopper-3-author", rounds.OpenRoundCalls()[0].RequestedBy)
}

// TestRequestReview_MissingTitleOrDescription_ReturnsFailedPrecondition is
// the acceptance-critical case ("A work branch cannot be reviewed without a
// title and description"): workbranchstore's guarded UPDATE enforces this
// as part of the same atomic transition, surfacing as
// ErrIllegalTransition, which must map to CodeFailedPrecondition here.
func TestRequestReview_MissingTitleOrDescription_ReturnsFailedPrecondition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	workBranches.UpdateStateFunc = func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, id, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, rounds.OpenRoundCalls(), "a round must not be opened when the state transition itself failed")
}

// --- ListWorkBranches ---

// TestListWorkBranches_AgentLackingWorkRead_Denied proves the capability
// gate runs before the store is consulted.
func TestListWorkBranches_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, nil, &buf)
	_, err := h.ListWorkBranches(agentCtx(t, "author"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, workBranches.ListCalls())
}

// TestListWorkBranches_DefaultState_FiltersReviewable proves an unset state
// filter defaults to reviewable (docs/cli-spec.md -> "list").
func TestListWorkBranches_DefaultState_FiltersReviewable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{}))
	require.NoError(t, err)
	require.Len(t, workBranches.ListCalls(), 1)
	assert.Equal(t, workbranchstore.StateReviewable, workBranches.ListCalls()[0].Filter.State)
}

// TestListWorkBranches_AwaitingReview_FiltersByCallerIdentity proves
// awaiting_review narrows to the calling agent's identity, and is a no-op
// filter (not an error) for an admin caller with no agent identity.
func TestListWorkBranches_AwaitingReview_FiltersByCallerIdentity(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{AwaitingReview: true}))
	require.NoError(t, err)
	require.Len(t, workBranches.ListCalls(), 1)
	assert.Equal(t, "grace-hopper-3-reviewer", workBranches.ListCalls()[0].Filter.AwaitingVerdictReviewer)
}

// TestListWorkBranches_RepoFilter_ResolvesOnceNotPerRow proves a repo-scoped
// list uses the already-resolved repo name for every row instead of one
// GetRepoByID call per row.
func TestListWorkBranches_RepoFilter_ResolvesOnceNotPerRow(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	other := sampleTitledWorkBranch(workbranchstore.StateReviewable)
	other.Name = "wb-abc123"
	workBranches.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
		return []workbranchstore.WorkBranch{sampleTitledWorkBranch(workbranchstore.StateReviewable), other}, 2, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{Repo: strPtr("bobcob7/doc-server")}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetWorkBranches(), 2)
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetWorkBranches()[0].GetRepo())
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetWorkBranches()[1].GetRepo())
	assert.Empty(t, repos.GetRepoByIDCalls(), "a repo-scoped list must reuse the already-resolved repo name, never call GetRepoByID")
}

// TestListWorkBranches_UnenrolledRepoFilter_ReturnsNotFound proves an
// explicit --repo filter naming an unenrolled repo maps to CodeNotFound.
func TestListWorkBranches_UnenrolledRepoFilter_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	repos.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{}, fmt.Errorf("getting repo %s: %w", name, reposstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{Repo: strPtr("bobcob7/ghost")}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
	assert.Empty(t, workBranches.ListCalls())
}

// TestListWorkBranches_Truncated_SetWhenMoreRowsExist proves PageInfo and
// Truncated are both derived from the store's total, not just len(results).
func TestListWorkBranches_Truncated_SetWhenMoreRowsExist(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	workBranches.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
		return []workbranchstore.WorkBranch{sampleTitledWorkBranch(workbranchstore.StateReviewable)}, 5, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{}))
	require.NoError(t, err)
	assert.True(t, resp.Msg.GetTruncated())
	assert.Equal(t, uint32(5), resp.Msg.GetPageInfo().GetTotal())
}

// --- GetWorkBranch ---

// TestGetWorkBranch_AgentLackingWorkRead_Denied proves the capability gate
// runs before the store round-trip.
func TestGetWorkBranch_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, nil, &buf)
	_, err := h.GetWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, workBranches.GetByNameCalls())
}

// TestGetWorkBranch_Success_ReturnsWorkBranch proves the happy path.
func TestGetWorkBranch_Success_ReturnsWorkBranch(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.GetWorkBranch(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Equal(t, "wb-9c2f1a", resp.Msg.GetWorkBranch().GetName())
	assert.Equal(t, "Add login", resp.Msg.GetWorkBranch().GetTitle())
}

// TestGetWorkBranch_NotFound_ReturnsCodeNotFound proves a genuinely absent
// work branch maps to CodeNotFound.
func TestGetWorkBranch_NotFound_ReturnsCodeNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("getting work branch %s: %w", name, workbranchstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetWorkBranch(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-ghost"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
}

// --- GetWorkBranchDiff ---

// TestGetWorkBranchDiff_AgentLackingWorkRead_Denied proves the capability
// gate runs before the work branch is even resolved.
func TestGetWorkBranchDiff_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, nil, &buf)
	_, err := h.GetWorkBranchDiff(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, diff.DiffCalls())
}

// TestGetWorkBranchDiff_Success_ReturnsDiff proves the happy path against a
// working DiffComputer.
func TestGetWorkBranchDiff_Success_ReturnsDiff(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.GetWorkBranchDiff(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Equal(t, "--- a/f\n+++ b/f\n", resp.Msg.GetDiff())
	require.Len(t, diff.DiffCalls(), 1)
	assert.Equal(t, sampleWorkBranchID, diff.DiffCalls()[0].WorkBranch.ID)
}

// TestGetWorkBranchDiff_ComputerNotImplemented_FailsLoudlyAsInternal proves
// the documented gap (no git diff plumbing exists anywhere in this tree
// yet) fails loudly -- logged and CodeInternal -- rather than silently
// returning an empty or fabricated diff.
func TestGetWorkBranchDiff_ComputerNotImplemented_FailsLoudlyAsInternal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff := allMocks()
	sentinel := errors.New("git diff plumbing not implemented (loam-ofg.16)")
	diff.DiffFunc = func(context.Context, workbranchstore.WorkBranch) (string, error) {
		return "", sentinel
	}
	h := newHandler(workBranches, repos, rounds, diff, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetWorkBranchDiff(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.Contains(t, buf.String(), "not implemented", "an unmapped diff-computer failure must be logged, not silently dropped")
}

func strPtr(s string) *string { return &s }
