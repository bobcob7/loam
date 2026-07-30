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
	"github.com/bobcob7/loam/internal/gitdiff"
	"github.com/bobcob7/loam/internal/gitref"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/workbranch"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewpublish"
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

// sampleRepoID and sampleWorkBranchID are shared fixture ids every test
// below builds its store stubs around.
var sampleRepoID = uuid.New()

var sampleWorkBranchID = uuid.New()

func sampleTitledWorkBranch(state workbranchstore.State) workbranchstore.WorkBranch {
	title, description := "Add login", "Adds a login flow."
	return workbranchstore.WorkBranch{
		ID: sampleWorkBranchID, RepoID: sampleRepoID, Name: "wb-9c2f1a", Target: "main",
		Title: &title, Description: &description, State: state, Author: "grace-hopper-3-author",
	}
}

// allMocks builds a fully configured set of the seven seams the Handler
// consumes, each answering a benign success, so a test that only cares
// about one behavior (e.g. the capability gate) can override just the
// relevant Func and trust every other mocked method is safe to call rather
// than nil-panicking -- the "beware the incomplete-mock trap" discipline: a
// mutation that removes an early-return gate must fall through to a real,
// observable success this test can assert against, never a panic that
// would obscure the real failure.
func allMocks() (*workbranch.WorkBranchStoreMock, *workbranch.RepoStoreMock, *workbranch.RoundStoreMock, *workbranch.DiffComputerMock, *workbranch.ThreadStoreMock, *workbranch.VerdictStoreMock, *workbranch.VerdictPublisherMock) {
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
	rounds := &workbranch.RoundStoreMock{
		OpenRoundFunc: func(_ context.Context, workBranchID uuid.UUID, requestedBy string) (reviewstore.Round, error) {
			return reviewstore.Round{ID: uuid.New(), WorkBranchID: workBranchID, Number: 1, RequestedBy: requestedBy}, nil
		},
		// A benign success, like every other default here: a branch under
		// review has a round, which is the shape ReplyToThread needs to
		// reach its write at all. The three RequestReview self-heal tests
		// and TestReplyToThread_NoReviewRound_FailedPrecondition each
		// override this with reviewstore.ErrNoCurrentRound explicitly,
		// since "there is no round" is the thing THOSE tests are about --
		// making it the shared default instead would silently gate every
		// other reply test behind an unrelated precondition.
		CurrentRoundFunc: func(_ context.Context, workBranchID uuid.UUID) (reviewstore.Round, error) {
			return reviewstore.Round{ID: sampleRoundID, WorkBranchID: workBranchID, Number: 1}, nil
		},
	}
	diff := &workbranch.DiffComputerMock{
		DiffFunc: func(_ context.Context, _ workbranchstore.WorkBranch) (string, error) {
			return "--- a/f\n+++ b/f\n", nil
		},
	}
	threads := &workbranch.ThreadStoreMock{
		ListFunc: func(_ context.Context, _ uuid.UUID, _, _ int32) ([]reviewstore.ThreadWithComments, int64, error) {
			return []reviewstore.ThreadWithComments{sampleThread()}, 1, nil
		},
		GetFunc: func(_ context.Context, workBranchID, id uuid.UUID) (reviewstore.Thread, error) {
			return reviewstore.Thread{ID: id, WorkBranchID: workBranchID, RoundID: sampleRoundID, Author: sampleReviewer}, nil
		},
		ReplyFunc: func(_ context.Context, threadID, roundID uuid.UUID, roundNumber int32, author, body string) (reviewstore.Comment, error) {
			return reviewstore.Comment{ID: uuid.New(), ThreadID: threadID, RoundID: roundID, RoundNumber: roundNumber, Author: author, Body: body}, nil
		},
	}
	verdicts := &workbranch.VerdictStoreMock{
		ListFunc: func(_ context.Context, _ uuid.UUID) ([]reviewstore.VerdictRecord, error) {
			return []reviewstore.VerdictRecord{{
				Verdict:     reviewstore.Verdict{ID: uuid.New(), RoundID: sampleRoundID, Reviewer: sampleReviewer, Outcome: reviewstore.OutcomeApprove},
				RoundNumber: 1, Current: true,
			}}, nil
		},
	}
	publisher := &workbranch.VerdictPublisherMock{
		PublishFunc: func(_ context.Context, req reviewpublish.Request) (reviewpublish.Result, error) {
			return reviewpublish.Result{
				Verdict:   reviewstore.Verdict{ID: uuid.New(), RoundID: sampleRoundID, Reviewer: req.Reviewer, Outcome: req.Outcome},
				Round:     reviewstore.Round{ID: sampleRoundID, WorkBranchID: req.WorkBranchID, Number: 1},
				Published: len(req.Comments),
				State:     workbranchstore.StateReviewed,
			}, nil
		},
	}
	return workBranches, repos, rounds, diff, threads, verdicts, publisher
}

// sampleRoundID and sampleReviewer are the fixture round and reviewer the
// review-half seams above answer with.
var sampleRoundID = uuid.New()

const sampleReviewer = "ada-lovelace-7-reviewer"

// sampleThread is allMocks' default published thread: anchored, unresolved,
// raised in round 1, carrying one comment posted in round 2 -- a comment's
// round is its own, never inherited from its thread, so a default fixture
// that made them equal would let a handler bug that copies the thread's
// round into every comment pass unnoticed.
func sampleThread() reviewstore.ThreadWithComments {
	file, line := "auth.go", int32(42)
	threadID := uuid.New()
	return reviewstore.ThreadWithComments{
		Thread: reviewstore.Thread{
			ID: threadID, WorkBranchID: sampleWorkBranchID, RoundID: sampleRoundID, RoundNumber: 1,
			Author: sampleReviewer, File: &file, Line: &line,
		},
		Comments: []reviewstore.Comment{{
			ID: uuid.New(), ThreadID: threadID, RoundID: uuid.New(), RoundNumber: 2,
			Author: sampleReviewer, Body: "this needs a guard",
		}},
	}
}

// okRefWriter is a WorkBranchRefWriter whose every method succeeds -- the
// benign default newHandler supplies for the great majority of tests, which
// are not about ref writing at all. Its calls are still RECORDED, so a test
// that cares can assert on them without wiring its own mock.
func okRefWriter() *workbranch.WorkBranchRefWriterMock {
	return &workbranch.WorkBranchRefWriterMock{
		CreateWorkBranchRefFunc: func(context.Context, string, string, string) error { return nil },
		DeleteWorkBranchRefFunc: func(context.Context, string, string) error { return nil },
	}
}

// newHandler wires a workbranch.Handler over the seven given seams plus a
// benign okRefWriter, with a capability checker backed by roleCaps and an
// ErrorMapper that logs to buf so tests can assert on the logged line for
// unmapped errors. Tests that need to observe or fail the ref writer use
// newHandlerWithRefs.
func newHandler(workBranches workbranch.WorkBranchStore, repos workbranch.RepoStore, rounds workbranch.RoundStore, diff workbranch.DiffComputer, threads workbranch.ThreadStore, verdicts workbranch.VerdictStore, publisher workbranch.VerdictPublisher, roleCaps []handler.Capability, buf *bytes.Buffer) *workbranch.Handler {
	return newHandlerWithRefs(workBranches, repos, rounds, diff, okRefWriter(), threads, verdicts, publisher, roleCaps, buf)
}

// newHandlerWithRefs is newHandler with the WorkBranchRefWriter seam
// supplied by the caller.
func newHandlerWithRefs(workBranches workbranch.WorkBranchStore, repos workbranch.RepoStore, rounds workbranch.RoundStore, diff workbranch.DiffComputer, refs workbranch.WorkBranchRefWriter, threads workbranch.ThreadStore, verdicts workbranch.VerdictStore, publisher workbranch.VerdictPublisher, roleCaps []handler.Capability, buf *bytes.Buffer) *workbranch.Handler {
	checker := handler.NewCapabilityChecker(fixedRoleStore{capabilities: roleCaps})
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(buf, nil)))
	return workbranch.New(workBranches, repos, rounds, diff, refs, threads, verdicts, publisher, checker, mapper, testLogger())
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, nil, &buf)
	_, err := h.CreateWorkBranch(adminCtx(t), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, workBranches.CreateCalls())
}

// TestCreateWorkBranch_EmptyRepoOrFrom_ReturnsInvalidArgument proves both
// required fields are validated before any store call.
func TestCreateWorkBranch_EmptyRepoOrFrom_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ repo, from string }{
		"empty repo": {"", "main"},
		"empty from": {"bobcob7/doc-server", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)
			_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: tc.repo, From: tc.from}))
			require.Error(t, err)
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.Empty(t, workBranches.CreateCalls())
		})
	}
}

// TestCreateWorkBranch_UnenrolledRepo_ReturnsNotFound proves a genuinely
// unenrolled repo maps to CodeNotFound, not the group-level fallback's
// indistinguishable 404.
func TestCreateWorkBranch_UnenrolledRepo_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	repos.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{}, fmt.Errorf("getting repo %s: %w", name, reposstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)
	resp, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))
	require.NoError(t, err)
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetWorkBranch().GetRepo())
	assert.Equal(t, "main", resp.Msg.GetWorkBranch().GetTarget())
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT, resp.Msg.GetWorkBranch().GetState())
	assert.Equal(t, "grace-hopper-3-author", resp.Msg.GetWorkBranch().GetAuthor())
	require.Len(t, workBranches.CreateCalls(), 1)
	assert.Regexp(t, `^wb-[0-9a-f]{6}$`, workBranches.CreateCalls()[0].Name)
}

// TestCreateWorkBranch_CreatesTheMirrorRef_BeforeTheRow is loam-5iu: the
// server OWES a work branch its ref (docs/git-spec.md -> Ref Policy,
// "created server-side by `work start` only"), and before this it created
// only the row -- so GetWorkBranchDiff answered FailedPrecondition for
// essentially every work branch.
//
// The ORDER is asserted, not just the fact of the call, because the two
// half-states are not equally bad: a ref with no row is inert, a row with
// no ref is the defect this exists to remove. The name and target the ref
// is created with must be the same ones the row gets, or the two halves
// would describe different branches.
func TestCreateWorkBranch_CreatesTheMirrorRef_BeforeTheRow(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	var order []string
	refs := okRefWriter()
	refs.CreateWorkBranchRefFunc = func(context.Context, string, string, string) error {
		order = append(order, "ref")
		return nil
	}
	inner := workBranches.CreateFunc
	workBranches.CreateFunc = func(ctx context.Context, repoID uuid.UUID, name, target, author string) (workbranchstore.WorkBranch, error) {
		order = append(order, "row")
		return inner(ctx, repoID, name, target, author)
	}
	h := newHandlerWithRefs(workBranches, repos, rounds, diff, refs, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)

	_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))

	require.NoError(t, err)
	assert.Equal(t, []string{"ref", "row"}, order, "the mirror ref must be written before the row: a ref with no row is inert, a row with no ref is loam-5iu")
	require.Len(t, refs.CreateWorkBranchRefCalls(), 1)
	require.Len(t, workBranches.CreateCalls(), 1)
	assert.Equal(t, "bobcob7/doc-server", refs.CreateWorkBranchRefCalls()[0].RepoName)
	assert.Equal(t, workBranches.CreateCalls()[0].Name, refs.CreateWorkBranchRefCalls()[0].Name, "the ref and the row must name the same branch")
	assert.Equal(t, "main", refs.CreateWorkBranchRefCalls()[0].From)
	assert.Empty(t, refs.DeleteWorkBranchRefCalls(), "nothing was rolled back on the happy path")
}

// TestCreateWorkBranch_RefWriteFails_NoRowIsInserted proves the ref failure
// is fatal to the whole operation rather than logged and shrugged off. A
// handler that carried on would recreate loam-5iu exactly: a row with no
// ref, indistinguishable to the agent from a healthy branch until `work
// diff` or `loam clone` failed.
func TestCreateWorkBranch_RefWriteFails_NoRowIsInserted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	refs := okRefWriter()
	refs.CreateWorkBranchRefFunc = func(context.Context, string, string, string) error {
		return fmt.Errorf("creating ref: %w", gitref.ErrTargetMissing)
	}
	h := newHandlerWithRefs(workBranches, repos, rounds, diff, refs, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)

	_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))

	require.Error(t, err)
	// A target branch registered in repo_target_branches but not yet in
	// the mirror is a precondition the caller can act on (retry once the
	// repo has synced), not an internal fault.
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, workBranches.CreateCalls(), "no row may be inserted for a work branch whose ref could not be created")
}

// TestCreateWorkBranch_RowInsertFails_RollsBackTheRef proves the
// compensating delete: with the ref written first, a failed insert would
// otherwise leave an orphan ref in the mirror forever -- protected from
// the mirror fetch by the reserved namespace, and so never cleaned up by
// anything.
func TestCreateWorkBranch_RowInsertFails_RollsBackTheRef(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.CreateFunc = func(context.Context, uuid.UUID, string, string, string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, errors.New("insert failed")
	}
	refs := okRefWriter()
	h := newHandlerWithRefs(workBranches, repos, rounds, diff, refs, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)

	_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))

	require.Error(t, err)
	require.Len(t, refs.CreateWorkBranchRefCalls(), 1)
	require.Len(t, refs.DeleteWorkBranchRefCalls(), 1, "the ref written before the failed insert must be rolled back")
	assert.Equal(t, refs.CreateWorkBranchRefCalls()[0].Name, refs.DeleteWorkBranchRefCalls()[0].Name)
	assert.Equal(t, "bobcob7/doc-server", refs.DeleteWorkBranchRefCalls()[0].RepoName)
}

// TestCreateWorkBranch_RollbackFails_StillReportsTheInsertError proves the
// error the AGENT sees is the one that actually went wrong. A rollback
// failure is an operator concern -- it leaves an inert orphan ref -- and
// reporting it instead would tell the agent to fix the wrong thing.
func TestCreateWorkBranch_RollbackFails_StillReportsTheInsertError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.CreateFunc = func(context.Context, uuid.UUID, string, string, string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, errors.New("the insert error the agent must see")
	}
	refs := okRefWriter()
	refs.DeleteWorkBranchRefFunc = func(context.Context, string, string) error {
		return errors.New("the rollback error the agent must NOT see")
	}
	h := newHandlerWithRefs(workBranches, repos, rounds, diff, refs, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkStart}, &buf)

	_, err := h.CreateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.CreateWorkBranchRequest{Repo: "bobcob7/doc-server", From: "main"}))

	require.Error(t, err)
	assert.Contains(t, buf.String(), "the insert error the agent must see", "the mapped error is the insert's")
	assert.NotContains(t, buf.String(), "the rollback error the agent must NOT see")
}

// --- UpdateWorkBranch ---

// TestUpdateWorkBranch_AgentLackingWorkSet_Denied proves the capability
// gate runs before the store round-trip.
func TestUpdateWorkBranch_AgentLackingWorkSet_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkSet}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.SetTitleDescriptionFunc = func(_ context.Context, id uuid.UUID, _, _ string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("setting title/description on work branch %s: %w", id, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkSet}, &buf)
	title := "New title"
	_, err := h.UpdateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.UpdateWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a", Title: &title}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
}

// TestUpdateWorkBranch_TerminalState_MessageNamesIllegalTransition is
// loam-blc's acceptance test mirrored for mapWorkBranchStoreErr: it used to
// wrap only handler.ErrFailedPrecondition and discard
// workbranchstore.ErrIllegalTransition, so the message an agent or operator
// saw ended in the generic "handler: failed precondition" with nothing
// naming which work branch or transition was actually rejected. err already
// names the work branch id (workbranchstore's own "%s work branch %s: %w"
// wrapping in transitionErr), so the mapped error must preserve it -- both
// in the rendered message (the CLI prints connectErr.Message() directly,
// per docs/cli-spec.md -> Exit Codes & Errors) and via errors.Is, which
// callers may still match on.
func TestUpdateWorkBranch_TerminalState_MessageNamesIllegalTransition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	// The stub names the work branch's ID (a fresh random UUID, distinct
	// from sampleWorkBranchID), not "wb-9c2f1a"/"bobcob7/doc-server" -- and
	// that choice is what lets this test fail at all: the handler's own
	// context prefix ("updating work branch bobcob7/doc-server/wb-9c2f1a")
	// never contains the id, so a stub naming it cannot separate "err was
	// preserved" from "only the bare sentinel was wrapped".
	//
	// It is also the realistic failure: workbranchstore's transitionErr
	// produces "setting title/description on work branch <id>: illegal
	// work branch state transition" (store.go's transitionErr), and naming
	// WHICH work branch and transition failed is the entire point of
	// loam-dq0o (mirroring loam-blc).
	workBranches.SetTitleDescriptionFunc = func(_ context.Context, id uuid.UUID, _, _ string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("setting title/description on work branch %s: %w", id, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkSet}, &buf)
	title := "New title"
	_, err := h.UpdateWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.UpdateWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a", Title: &title}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), "setting title/description on work branch", "the message must name WHICH transition was attempted -- the store's own wrapping appears nowhere in the handler's own context prefix, so only preserving err can put it there")
	assert.Contains(t, connectErr.Message(), workbranchstore.ErrIllegalTransition.Error(), "and the sentinel's own text must survive, not just terminate in the generic \"handler: failed precondition\"")
	assert.ErrorIs(t, err, workbranchstore.ErrIllegalTransition, "the sentinel must survive the mapping, not just the Connect code")
	assert.ErrorIs(t, err, handler.ErrFailedPrecondition)
}

// TestUpdateWorkBranch_NotFound_ReturnsCodeNotFound proves
// workbranchstore.ErrNotFound (a distinct sentinel from
// ErrIllegalTransition) maps to CodeNotFound, not CodeFailedPrecondition --
// pinning the OTHER half of the "map these correctly" requirement.
func TestUpdateWorkBranch_NotFound_ReturnsCodeNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("getting work branch %s: %w", name, workbranchstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkSet}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, nil, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.UpdateStateFunc = func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, id, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, rounds.OpenRoundCalls(), "a round must not be opened when the state transition itself failed")
}

// TestRequestReview_SelfHealsAfterInterruptedRoundOpen is MUST-FIX 1's
// acceptance test: an interrupted RequestReview (UpdateState landed,
// OpenRound never did -- an ordinary client disconnect/deadline between
// the two round-trips is enough, no crash required) must not leave the
// work branch in the unrecoverable dead-end a bare retry would hit
// (ErrIllegalTransition on reviewable->reviewable forever). The retry must
// SUCCEED by opening the missing round itself, not merely fail
// differently -- this proves the SECOND call's outcome, not just that the
// first one errors.
func TestRequestReview_SelfHealsAfterInterruptedRoundOpen(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	openRoundCalls := 0
	rounds.OpenRoundFunc = func(_ context.Context, workBranchID uuid.UUID, requestedBy string) (reviewstore.Round, error) {
		openRoundCalls++
		if openRoundCalls == 1 {
			return reviewstore.Round{}, context.DeadlineExceeded
		}
		return reviewstore.Round{ID: uuid.New(), WorkBranchID: workBranchID, Number: 1, RequestedBy: requestedBy}, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err, "the first call's OpenRound failed, so it must not silently report success")
	// The retry: the work branch is now already reviewable (UpdateState's
	// write from the first call autocommitted), so a real store would
	// reject a second UpdateState with ErrIllegalTransition -- simulated
	// here since WorkBranchStoreMock does not itself hold state. There is
	// still no current round (the first OpenRound never landed), so the
	// handler must self-heal: open one now and report SUCCESS, not repeat
	// the misleading "failed precondition" a bare retry would surface.
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return sampleTitledWorkBranch(workbranchstore.StateReviewable), nil
	}
	workBranches.UpdateStateFunc = func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, id, workbranchstore.ErrIllegalTransition)
	}
	rounds.CurrentRoundFunc = func(_ context.Context, workBranchID uuid.UUID) (reviewstore.Round, error) {
		return reviewstore.Round{}, fmt.Errorf("getting current round for work branch %s: %w", workBranchID, reviewstore.ErrNoCurrentRound)
	}
	resp, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err, "the retry must self-heal: open the missing round and succeed, not repeat the misleading failed-precondition error")
	assert.Equal(t, loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE, resp.Msg.GetWorkBranch().GetState())
	assert.Equal(t, 2, openRoundCalls, "the self-heal must call OpenRound again")
}

// TestRequestReview_AlreadyReviewableWithRound_ReturnsFailedPrecondition
// proves the self-heal does NOT fire for a genuine reviewable->reviewable
// rejection: when a current round already exists, the branch is
// legitimately already under review, and no round should be opened.
func TestRequestReview_AlreadyReviewableWithRound_ReturnsFailedPrecondition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return sampleTitledWorkBranch(workbranchstore.StateReviewable), nil
	}
	workBranches.UpdateStateFunc = func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, id, workbranchstore.ErrIllegalTransition)
	}
	rounds.CurrentRoundFunc = func(_ context.Context, workBranchID uuid.UUID) (reviewstore.Round, error) {
		return reviewstore.Round{ID: uuid.New(), WorkBranchID: workBranchID, Number: 1}, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err, "an already-reviewable branch that already has a round is a genuine illegal transition, not something to self-heal")
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, rounds.OpenRoundCalls(), "no round should be opened when one already exists")
}

// TestRequestReview_CurrentRoundLookupFails_MapsToInternal proves a genuine
// (non-ErrNoCurrentRound) failure checking for a current round during the
// self-heal attempt is reported, not silently swallowed into either a
// false success or the misleading failed-precondition message.
func TestRequestReview_CurrentRoundLookupFails_MapsToInternal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return sampleTitledWorkBranch(workbranchstore.StateReviewable), nil
	}
	workBranches.UpdateStateFunc = func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, id, workbranchstore.ErrIllegalTransition)
	}
	dbErr := errors.New("connection reset by peer")
	rounds.CurrentRoundFunc = func(_ context.Context, workBranchID uuid.UUID) (reviewstore.Round, error) {
		return reviewstore.Round{}, fmt.Errorf("getting current round for work branch %s: %w", workBranchID, dbErr)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.Contains(t, buf.String(), "connection reset by peer")
}

// TestRequestReview_TerminalState_MessageNamesTerminalState and
// TestRequestReview_MissingTitleOrDescription_MessageNamesMissingFields are
// SHOULD-FIX 3's acceptance tests: the CLI renders err.Message() directly
// (docs/cli-spec.md -> Exit Codes & Errors), so the two causes
// ErrIllegalTransition conflates must produce DIFFERENT, individually
// accurate messages -- not the same generic "failed precondition" string
// regardless of which is true.
func TestRequestReview_TerminalState_MessageNamesTerminalState(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return sampleTitledWorkBranch(workbranchstore.StateClosed), nil
	}
	workBranches.UpdateStateFunc = func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, id, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), "terminal state")
	assert.NotContains(t, connectErr.Message(), "title or description")
}

// TestRequestReview_TerminalState_MessageNamesIllegalTransition is
// loam-jv8f's acceptance test for mapRequestReviewErr, mirroring loam-dq0o's
// TestUpdateWorkBranch_TerminalState_MessageNamesIllegalTransition: the
// mapper used to wrap only requestReviewPreconditionMessage(before) and
// handler.ErrFailedPrecondition, discarding workbranchstore.ErrIllegalTransition
// entirely, so errors.Is(mapped, workbranchstore.ErrIllegalTransition) was
// false.
func TestRequestReview_TerminalState_MessageNamesIllegalTransition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return sampleTitledWorkBranch(workbranchstore.StateClosed), nil
	}
	// The stub names a FRESH random UUID, distinct from sampleWorkBranchID,
	// so the assertion cannot be satisfied textually by the handler's own
	// context prefix ("requesting review on work branch
	// bobcob7/doc-server/wb-9c2f1a" never contains the id) -- only actually
	// preserving err can put it in the message. Mirrors the trap
	// loam-dq0o's precedent test deliberately set up.
	freshID := uuid.New()
	workBranches.UpdateStateFunc = func(_ context.Context, _ uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, freshID, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), freshID.String(), "the store's own wrapping (naming which work branch failed) must survive -- it used to be discarded in favor of only the hand-written precondition message")
	assert.ErrorIs(t, err, workbranchstore.ErrIllegalTransition, "the sentinel must survive the mapping, not just the Connect code")
	assert.ErrorIs(t, err, handler.ErrFailedPrecondition)
}

func TestRequestReview_MissingTitleOrDescription_MessageNamesMissingFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	untitled := workbranchstore.WorkBranch{ID: sampleWorkBranchID, RepoID: sampleRepoID, Name: "wb-9c2f1a", Target: "main", State: workbranchstore.StateDraft, Author: "grace-hopper-3-author"}
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		return untitled, nil
	}
	workBranches.UpdateStateFunc = func(_ context.Context, id uuid.UUID, to workbranchstore.State) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("transitioning to %s work branch %s: %w", to, id, workbranchstore.ErrIllegalTransition)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRequestReview}, &buf)
	_, err := h.RequestReview(agentCtx(t, "author"), connect.NewRequest(&loamv1.RequestReviewRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), "title or description")
	assert.NotContains(t, connectErr.Message(), "terminal state")
}

// --- ListWorkBranches ---

// TestListWorkBranches_AgentLackingWorkRead_Denied proves the capability
// gate runs before the store is consulted.
func TestListWorkBranches_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, nil, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	other := sampleTitledWorkBranch(workbranchstore.StateReviewable)
	other.Name = "wb-abc123"
	workBranches.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
		return []workbranchstore.WorkBranch{sampleTitledWorkBranch(workbranchstore.StateReviewable), other}, 2, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	repos.GetRepoByNameFunc = func(_ context.Context, name string) (reposstore.Repo, error) {
		return reposstore.Repo{}, fmt.Errorf("getting repo %s: %w", name, reposstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
		return []workbranchstore.WorkBranch{sampleTitledWorkBranch(workbranchstore.StateReviewable)}, 5, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{}))
	require.NoError(t, err)
	assert.True(t, resp.Msg.GetTruncated())
	assert.Equal(t, uint32(5), resp.Msg.GetPageInfo().GetTotal())
}

// TestListWorkBranches_BuildsExactFilterAndPage is SHOULD-FIX 2's central
// acceptance test: it sets Target, Author, an explicit State, and a
// non-default Page{Limit, Offset} all at once and asserts the EXACT
// workbranchstore.ListFilter and (limit, offset) handed to
// WorkBranchStore.List -- not just that the call happened. This alone
// kills "drop the Target filter", "drop the Author filter", and "ignore
// Page.offset".
func TestListWorkBranches_BuildsExactFilterAndPage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	target, author := "feature-x", "grace-hopper-3-author"
	state := loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED
	req := &loamv1.ListWorkBranchesRequest{Target: &target, Author: &author, State: &state, Page: &loamv1.Page{Limit: 7, Offset: 20}}
	_, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(req))
	require.NoError(t, err)
	require.Len(t, workBranches.ListCalls(), 1)
	call := workBranches.ListCalls()[0]
	assert.Equal(t, workbranchstore.ListFilter{Target: target, Author: author, State: workbranchstore.StateReviewed}, call.Filter)
	assert.Equal(t, int32(7), call.Limit)
	assert.Equal(t, int32(20), call.Offset)
}

// TestListWorkBranches_UnsetPage_DefaultsLimitTo100 proves the exact
// default page size (docs/cli-spec.md -> "list": "defaults to 100"), not
// just "some" default -- kills a mutation changing defaultListLimit to any
// other value.
func TestListWorkBranches_UnsetPage_DefaultsLimitTo100(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{}))
	require.NoError(t, err)
	require.Len(t, workBranches.ListCalls(), 1)
	assert.Equal(t, int32(100), workBranches.ListCalls()[0].Limit)
	assert.Equal(t, int32(0), workBranches.ListCalls()[0].Offset)
}

// TestListWorkBranches_ExplicitUnspecifiedState_ReturnsInvalidArgument
// proves an explicitly present but WORK_BRANCH_STATE_UNSPECIFIED filter is
// rejected as a bad filter value (docs/cli-spec.md -> "list": "exit 2 on a
// bad filter value"), not silently treated the same as an absent filter
// (which defaults to reviewable).
func TestListWorkBranches_ExplicitUnspecifiedState_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	state := loamv1.WorkBranchState_WORK_BRANCH_STATE_UNSPECIFIED
	_, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{State: &state}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, workBranches.ListCalls())
}

// TestListWorkBranches_StateFilter_RoundTripsEveryEnumValue is
// protoToState's round-trip proof across every non-unspecified enum value,
// killing a mutation that maps any one of them to the wrong
// workbranchstore.State (e.g. DRAFT -> StateComplete).
func TestListWorkBranches_StateFilter_RoundTripsEveryEnumValue(t *testing.T) {
	t.Parallel()
	cases := map[loamv1.WorkBranchState]workbranchstore.State{
		loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT:      workbranchstore.StateDraft,
		loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE: workbranchstore.StateReviewable,
		loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED:   workbranchstore.StateReviewed,
		loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE:   workbranchstore.StateComplete,
		loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED:     workbranchstore.StateClosed,
	}
	for proto, want := range cases {
		t.Run(string(want), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
			state := proto
			_, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{State: &state}))
			require.NoError(t, err)
			require.Len(t, workBranches.ListCalls(), 1)
			assert.Equal(t, want, workBranches.ListCalls()[0].Filter.State)
		})
	}
}

// TestListWorkBranches_NoRepoFilter_ResolvesEachDistinctRepoCorrectly
// proves repoNamesFor pairs each row with ITS OWN repo's name via
// RepoStore.GetRepoByID when no --repo filter narrows the list to one
// already-known repo -- killing a mutation that resolves the wrong name
// (e.g. reusing the first row's name for every row, or swapping two
// repos' names).
func TestListWorkBranches_NoRepoFilter_ResolvesEachDistinctRepoCorrectly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	otherRepoID := uuid.New()
	wbA := sampleTitledWorkBranch(workbranchstore.StateReviewable)
	wbB := sampleTitledWorkBranch(workbranchstore.StateReviewable)
	wbB.RepoID, wbB.Name = otherRepoID, "wb-other"
	workBranches.ListFunc = func(_ context.Context, _ workbranchstore.ListFilter, _, _ int32) ([]workbranchstore.WorkBranch, int64, error) {
		return []workbranchstore.WorkBranch{wbA, wbB}, 2, nil
	}
	repos.GetRepoByIDFunc = func(_ context.Context, id uuid.UUID) (reposstore.Repo, error) {
		if id == otherRepoID {
			return reposstore.Repo{ID: id, Name: "acme/other-repo"}, nil
		}
		return reposstore.Repo{ID: id, Name: "bobcob7/doc-server"}, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.ListWorkBranches(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.ListWorkBranchesRequest{}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetWorkBranches(), 2)
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetWorkBranches()[0].GetRepo())
	assert.Equal(t, "acme/other-repo", resp.Msg.GetWorkBranches()[1].GetRepo())
}

// --- GetWorkBranch ---

// TestGetWorkBranch_AgentLackingWorkRead_Denied proves the capability gate
// runs before the store round-trip.
func TestGetWorkBranch_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, nil, &buf)
	_, err := h.GetWorkBranch(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
	assert.Empty(t, workBranches.GetByNameCalls())
}

// TestGetWorkBranch_Success_ReturnsWorkBranch proves the happy path.
func TestGetWorkBranch_Success_ReturnsWorkBranch(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, name string) (workbranchstore.WorkBranch, error) {
		return workbranchstore.WorkBranch{}, fmt.Errorf("getting work branch %s: %w", name, workbranchstore.ErrNotFound)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetWorkBranch(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-ghost"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
}

// TestGetWorkBranch_State_RoundTripsEveryEnumValue is stateToProto's
// round-trip proof across every workbranchstore.State value, killing a
// mutation that maps any one of them to the wrong proto enum value (e.g.
// REVIEWED -> WORK_BRANCH_STATE_CLOSED).
func TestGetWorkBranch_State_RoundTripsEveryEnumValue(t *testing.T) {
	t.Parallel()
	cases := map[workbranchstore.State]loamv1.WorkBranchState{
		workbranchstore.StateDraft:      loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT,
		workbranchstore.StateReviewable: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE,
		workbranchstore.StateReviewed:   loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED,
		workbranchstore.StateComplete:   loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE,
		workbranchstore.StateClosed:     loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED,
	}
	for store, want := range cases {
		t.Run(string(store), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
			workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
				return sampleTitledWorkBranch(store), nil
			}
			h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
			resp, err := h.GetWorkBranch(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
			require.NoError(t, err)
			assert.Equal(t, want, resp.Msg.GetWorkBranch().GetState())
		})
	}
}

// TestGetWorkBranch_PreservesUpstreamPRURL proves UpstreamPRURL survives
// the store-to-proto conversion, killing a mutation that drops it.
func TestGetWorkBranch_PreservesUpstreamPRURL(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	url := "https://github.com/bobcob7/doc-server/pull/42"
	workBranches.GetByNameFunc = func(_ context.Context, _ uuid.UUID, _ string) (workbranchstore.WorkBranch, error) {
		wb := sampleTitledWorkBranch(workbranchstore.StateReviewed)
		wb.UpstreamPRURL = &url
		return wb, nil
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.GetWorkBranch(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	require.NotNil(t, resp.Msg.GetWorkBranch().UpstreamPrUrl)
	assert.Equal(t, url, resp.Msg.GetWorkBranch().GetUpstreamPrUrl())
}

// --- GetWorkBranchDiff ---

// TestGetWorkBranchDiff_AgentLackingWorkRead_Denied proves the capability
// gate runs before the work branch is even resolved.
func TestGetWorkBranchDiff_AgentLackingWorkRead_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, nil, &buf)
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
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	resp, err := h.GetWorkBranchDiff(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.NoError(t, err)
	assert.Equal(t, "--- a/f\n+++ b/f\n", resp.Msg.GetDiff())
	require.Len(t, diff.DiffCalls(), 1)
	assert.Equal(t, sampleWorkBranchID, diff.DiffCalls()[0].WorkBranch.ID)
}

// TestGetWorkBranchDiff_UnmappedComputerError_FailsLoudlyAsInternal proves
// a DiffComputer error with no handler.Err* sentinel match (mapDiffComputerErr's
// default branch, e.g. gitdiff.ErrMirrorMissing -- an operational fault,
// not a caller mistake) falls through to ErrorMapper's own default: logged
// and CodeInternal, never silently dropped or turned into an empty diff.
func TestGetWorkBranchDiff_UnmappedComputerError_FailsLoudlyAsInternal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	sentinel := errors.New("bare mirror missing or invalid on disk")
	diff.DiffFunc = func(context.Context, workbranchstore.WorkBranch) (string, error) {
		return "", sentinel
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetWorkBranchDiff(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.Contains(t, buf.String(), "missing or invalid on disk", "an unmapped diff-computer failure must be logged, not silently dropped")
}

// TestGetWorkBranchDiff_RefMissing_MapsToFailedPrecondition proves
// gitdiff.ErrRefMissing (a ref the diff range needs -- target or the work
// branch's own name -- absent from the mirror) surfaces as
// CodeFailedPrecondition, not CodeInternal: the request itself is valid,
// but the mirror's current state does not support it.
func TestGetWorkBranchDiff_RefMissing_MapsToFailedPrecondition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	diff.DiffFunc = func(context.Context, workbranchstore.WorkBranch) (string, error) {
		return "", fmt.Errorf("wb-9c2f1a: %w", gitdiff.ErrRefMissing)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetWorkBranchDiff(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
}

// TestGetWorkBranchDiff_RefMissing_MessageNamesMissingRef is loam-blc's
// acceptance test: mapDiffComputerErr used to wrap only
// handler.ErrFailedPrecondition and discard gitdiff.ErrRefMissing, so the
// message an agent or operator saw ended in the generic "handler: failed
// precondition" with nothing pointing at a missing ref. err already names
// the ref (internal/gitdiff's own "<ref>: %w" wrapping in Diff), so the
// mapped error must preserve it -- both in the rendered message (the CLI
// prints connectErr.Message() directly, per docs/cli-spec.md -> Exit Codes
// & Errors) and via errors.Is, which callers may still match on.
func TestGetWorkBranchDiff_RefMissing_MessageNamesMissingRef(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	// The stub names the TARGET ref, not the work branch, and that choice
	// is what lets this test fail at all: "wb-9c2f1a" already appears in
	// the handler's own context prefix ("computing diff for work branch
	// bobcob7/doc-server/wb-9c2f1a"), so a stub naming it cannot separate
	// "err was preserved" from "only the bare sentinel was wrapped" --
	// both render a message containing wb-9c2f1a. "main" appears nowhere
	// in that prefix.
	//
	// It is also the realistic failure: internal/gitdiff produces
	// "verifying target branch main: main: gitdiff: ref not found in
	// mirror" (diff.go:172,202), and naming WHICH ref is missing is the
	// entire point of loam-blc.
	diff.DiffFunc = func(context.Context, workbranchstore.WorkBranch) (string, error) {
		return "", fmt.Errorf("verifying target branch main: main: %w", gitdiff.ErrRefMissing)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetWorkBranchDiff(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Contains(t, connectErr.Message(), "verifying target branch main", "the message must name WHICH ref is missing -- the target ref's name appears nowhere in the handler's own context prefix, so only preserving err can put it there")
	assert.Contains(t, connectErr.Message(), gitdiff.ErrRefMissing.Error(), "and the sentinel's own text must survive, not just terminate in the generic \"handler: failed precondition\"")
	assert.ErrorIs(t, err, gitdiff.ErrRefMissing)
	assert.ErrorIs(t, err, handler.ErrFailedPrecondition)
}

// TestGetWorkBranchDiff_NoMergeBase_MapsToFailedPrecondition is the same
// proof for gitdiff.ErrNoMergeBase (target and the work branch share no
// common ancestor -- unrelated histories).
func TestGetWorkBranchDiff_NoMergeBase_MapsToFailedPrecondition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	workBranches, repos, rounds, diff, threads, verdicts, publisher := allMocks()
	diff.DiffFunc = func(context.Context, workbranchstore.WorkBranch) (string, error) {
		return "", fmt.Errorf("main...wb-9c2f1a: %w", gitdiff.ErrNoMergeBase)
	}
	h := newHandler(workBranches, repos, rounds, diff, threads, verdicts, publisher, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetWorkBranchDiff(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetWorkBranchDiffRequest{Repo: "bobcob7/doc-server", WorkBranch: "wb-9c2f1a"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	// mapDiffComputerErr's doc comment claims BOTH precondition branches
	// preserve err; without this the ErrNoMergeBase branch had nothing
	// proving it, so a revert to the old single-%w wrap would have been
	// caught only on the ErrRefMissing side.
	assert.ErrorIs(t, err, gitdiff.ErrNoMergeBase, "the sentinel must survive the mapping, not just the Connect code")
}

func strPtr(s string) *string { return &s }
