package workbranch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/gitdiff"
	"github.com/bobcob7/loam/internal/gitref"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// defaultListLimit is the page size ListWorkBranches uses when the
// request's Page.Limit is unset (0), matching docs/cli-spec.md -> "list":
// "--limit <n> ... defaults to 100".
const defaultListLimit = 100

// adminRoundActor names the review round's requested_by column
// (internal/reviewstore.Round) when RequestReview is called by the admin's
// send-back path (docs/web-spec.md -> ProposalService: "the admin calls
// WorkBranchService.RequestReview ... as a superuser"), which carries no
// agent identity to render via httpauth.Identity.Identifier(). The spec is
// silent on what to record here; "admin" is the conservative, documented
// choice -- distinguishable at a glance from every real
// "<name>-<id>-<role>" identifier, which always carries two hyphens.
const adminRoundActor = "admin"

// Handler implements loamv1connect.WorkBranchServiceHandler in full: the
// lifecycle half (loam-ofg.8) -- CreateWorkBranch, UpdateWorkBranch,
// RequestReview, ListWorkBranches, GetWorkBranch, GetWorkBranchDiff -- in
// this file, and the review half (loam-ofg.9) -- ListComments,
// ListVerdicts, SubmitVerdict, ReplyToThread -- in review.go.
//
// The embedded loamv1connect.UnimplementedWorkBranchServiceHandler no
// longer covers any method: every RPC the service declares is implemented
// here. It stays only so that a future RPC added to the proto still
// compiles into a registered-but-Unimplemented route instead of breaking
// the build at loamv1connect.NewWorkBranchServiceHandler -- the same reason
// the generated code offers it.
type Handler struct {
	loamv1connect.UnimplementedWorkBranchServiceHandler
	workBranches WorkBranchStore
	repos        RepoStore
	rounds       RoundStore
	diff         DiffComputer
	refs         WorkBranchRefWriter
	threads      ThreadStore
	verdicts     VerdictStore
	publisher    VerdictPublisher
	capabilities *handler.CapabilityChecker
	errors       *handler.ErrorMapper
	logger       *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ loamv1connect.WorkBranchServiceHandler = (*Handler)(nil)

// New builds a Handler over the given seams, gating every RPC with
// capabilities and mapping domain errors through errors.
func New(workBranches WorkBranchStore, repos RepoStore, rounds RoundStore, diff DiffComputer, refs WorkBranchRefWriter, threads ThreadStore, verdicts VerdictStore, publisher VerdictPublisher, capabilities *handler.CapabilityChecker, errors *handler.ErrorMapper, logger *slog.Logger) *Handler {
	return &Handler{
		workBranches: workBranches, repos: repos, rounds: rounds, diff: diff, refs: refs,
		threads: threads, verdicts: verdicts, publisher: publisher,
		capabilities: capabilities, errors: errors, logger: logger,
	}
}

// CreateWorkBranch creates a work branch server-side from a target branch,
// without a working copy (docs/cli-spec.md -> "start"). Gated by
// CapabilityWorkStart. from is always explicit -- there is no default base
// branch (proto's own comment on CreateWorkBranchRequest.from: "Always
// explicit"); a bead DESIGN note claiming otherwise does not match the
// proto this handler implements and is not followed here.
//
// # The ref is written BEFORE the row, and rolled back if the row fails
//
// A work branch is two things: a work_branches row and a ref in the bare
// mirror (docs/git-spec.md -> Ref Policy, which makes the ref the server's
// job and forbids a push from creating one). They cannot be written
// atomically -- one is Postgres, one is a git subprocess -- so the order
// decides which half-state is reachable, and the two are not equally bad:
//
//   - Ref, then row. A failed insert leaves an ORPHAN REF: invisible to
//     every RPC (nothing lists refs), unpushable (refpolicy rejects a ref
//     with no row), and protected from the mirror fetch by
//     refnames.ReservedExclusionRefspec. Inert. It is also compensated
//     below, so it only survives if the rollback itself fails.
//   - Row, then ref. A failed create leaves a ROW WITH NO REF -- which is
//     precisely the loam-5iu defect this code exists to remove, reachable
//     again on every partial failure: `work diff` answers
//     FailedPrecondition and `loam clone` has nothing to fetch, with no
//     way for the agent to tell that branch from a healthy one.
//
// So the ref goes first, and the invariant this establishes is the strong
// one every other component already assumes: a work_branches row always
// has its ref. The write pair itself lives in createWorkBranchRefFirst.
func (h *Handler) CreateWorkBranch(ctx context.Context, req *connect.Request[loamv1.CreateWorkBranchRequest]) (*connect.Response[loamv1.CreateWorkBranchResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkStart); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, from := req.Msg.GetRepo(), req.Msg.GetFrom()
	if repo == "" || from == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("repo and from are required: %w", handler.ErrInvalidArgument))
	}
	author, err := authorIdentifier(ctx)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repoRow, err := h.repos.GetRepoByName(ctx, repo)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapRepoStoreErr(err, fmt.Sprintf("repo %s", repo)))
	}
	targets, err := h.repos.ListTargetBranches(ctx, repoRow.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing target branches for repo %s: %w", repo, err))
	}
	if !hasTargetBranch(targets, from) {
		return nil, h.errors.ToConnectErr(fmt.Errorf("%s is not a valid target branch for repo %s: %w", from, repo, handler.ErrInvalidArgument))
	}
	name, err := randomWorkBranchName()
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("generating work branch name: %w", err))
	}
	wb, err := h.createWorkBranchRefFirst(ctx, repoRow, name, from, author)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	return connect.NewResponse(&loamv1.CreateWorkBranchResponse{WorkBranch: workBranchToProto(repoRow.Name, wb)}), nil
}

// createWorkBranchRefFirst performs CreateWorkBranch's two non-atomic
// writes in the order its doc comment justifies: the mirror ref, then the
// work_branches row, with a best-effort delete of the ref if the row fails.
func (h *Handler) createWorkBranchRefFirst(ctx context.Context, repoRow reposstore.Repo, name, from, author string) (workbranchstore.WorkBranch, error) {
	if err := h.refs.CreateWorkBranchRef(ctx, repoRow.Name, name, from); err != nil {
		return workbranchstore.WorkBranch{}, mapRefWriterErr(err, fmt.Sprintf("creating the mirror ref for work branch %s/%s from %s", repoRow.Name, name, from))
	}
	wb, err := h.workBranches.Create(ctx, repoRow.ID, name, from, author)
	if err != nil {
		// Best-effort compensation: a failure here is logged, not
		// returned, because the caller's error is the INSERT's -- that is
		// what actually went wrong and what the agent must act on -- and
		// what is left behind is an inert ref (see above), not a
		// half-created work branch.
		if delErr := h.refs.DeleteWorkBranchRef(ctx, repoRow.Name, name); delErr != nil {
			h.logger.ErrorContext(ctx, "rolling back a work-branch ref after its row failed to insert did not succeed; the mirror holds an orphan ref", "repo", repoRow.Name, "work_branch", name, "error", delErr)
		}
		return workbranchstore.WorkBranch{}, fmt.Errorf("creating work branch in repo %s from %s: %w", repoRow.Name, from, err)
	}
	return wb, nil
}

// UpdateWorkBranch replaces a work branch's title and/or description at any
// point before it reaches a terminal state (docs/cli-spec.md -> "set").
// Gated by CapabilityWorkSet. Any field the request leaves unset keeps its
// current value -- workbranchstore.SetTitleDescription is a full replace,
// not a partial patch, so this handler reads the current row first.
//
// docs/README.md -> "Future Work" lists per-repo JSON Schema validation of
// descriptions as explicitly "Dropped from the MVP; free text until then" --
// so no such validation runs here; applying one would contradict the spec
// this handler implements, not fill a gap in it.
func (h *Handler) UpdateWorkBranch(ctx context.Context, req *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkSet); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	repoRow, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	title := derefOr(wb.Title, "")
	if req.Msg.Title != nil {
		title = req.Msg.GetTitle()
	}
	description := derefOr(wb.Description, "")
	if req.Msg.Description != nil {
		description = req.Msg.GetDescription()
	}
	updated, err := h.workBranches.SetTitleDescription(ctx, wb.ID, title, description)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapWorkBranchStoreErr(err, fmt.Sprintf("updating work branch %s/%s", repo, name)))
	}
	return connect.NewResponse(&loamv1.UpdateWorkBranchResponse{WorkBranch: workBranchToProto(repoRow.Name, updated)}), nil
}

// RequestReview transitions a work branch to reviewable -- from draft (the
// first review) or from reviewed (a re-review) -- and opens a fresh review
// round (docs/cli-spec.md -> "request-review"). Gated by
// CapabilityWorkRequestReview for an agent caller; an admin superuser
// bypasses via CapabilityChecker.RequireCapability's own admin check, not
// this capability, per docs/web-spec.md -> ProposalService's send-back
// path. proto's RequestReviewRequest carries no comment field (reserved 3
// "comment") -- the bead DESIGN note's "optional comment" does not match
// the proto this handler implements and is not followed here; feedback
// lives in comment threads (loam-ofg.9), not on this RPC.
//
// If UpdateState succeeds but the following OpenRound is interrupted (an
// ordinary client disconnect or deadline between the two round-trips is
// enough -- see RoundStore's doc comment for why this is routinely
// reachable, not a rare crash-window edge case), a retry self-heals: see
// selfHealInterruptedRequestReview.
func (h *Handler) RequestReview(ctx context.Context, req *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkRequestReview); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	repoRow, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	opContext := fmt.Sprintf("requesting review for work branch %s/%s", repo, name)
	updated, err := h.workBranches.UpdateState(ctx, wb.ID, workbranchstore.StateReviewable)
	if err != nil {
		healed, healErr := h.selfHealInterruptedRequestReview(ctx, wb, err, opContext)
		if healErr != nil {
			return nil, h.errors.ToConnectErr(healErr)
		}
		if healed == nil {
			return nil, h.errors.ToConnectErr(mapRequestReviewErr(err, wb, opContext))
		}
		return connect.NewResponse(&loamv1.RequestReviewResponse{WorkBranch: workBranchToProto(repoRow.Name, *healed)}), nil
	}
	if _, err := h.rounds.OpenRound(ctx, updated.ID, requestReviewActor(ctx)); err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("opening review round for work branch %s/%s: %w", repo, name, err))
	}
	return connect.NewResponse(&loamv1.RequestReviewResponse{WorkBranch: workBranchToProto(repoRow.Name, updated)}), nil
}

// selfHealInterruptedRequestReview recovers from the unrecoverable dead-end
// an interrupted RequestReview otherwise leaves behind (see RoundStore's
// doc comment): transitionErr is UpdateState's failure; before is the work
// branch's state as read BEFORE that call was attempted. Returns a non-nil
// *workbranchstore.WorkBranch (and a nil error) only when this genuinely
// was the interrupted-retry shape and the round has now been opened --
// callers must treat that as success. A nil, nil return means transitionErr
// is an ordinary, unrelated failure the caller should map and report as
// usual; a non-nil error means the healing attempt itself failed and must
// be reported instead.
func (h *Handler) selfHealInterruptedRequestReview(ctx context.Context, before workbranchstore.WorkBranch, transitionErr error, opContext string) (*workbranchstore.WorkBranch, error) {
	if !errors.Is(transitionErr, workbranchstore.ErrIllegalTransition) || before.State != workbranchstore.StateReviewable {
		return nil, nil
	}
	_, roundErr := h.rounds.CurrentRound(ctx, before.ID)
	if roundErr == nil {
		return nil, nil
	}
	if !errors.Is(roundErr, reviewstore.ErrNoCurrentRound) {
		return nil, fmt.Errorf("checking for a current round while %s: %w", opContext, roundErr)
	}
	if _, err := h.rounds.OpenRound(ctx, before.ID, requestReviewActor(ctx)); err != nil {
		return nil, fmt.Errorf("self-healing an interrupted request-review by opening the missing round while %s: %w", opContext, err)
	}
	h.logger.InfoContext(ctx, "self-healed an interrupted request-review", "work_branch_id", before.ID)
	return &before, nil
}

// ListWorkBranches returns work branches across enrolled repos, filtered by
// the request fields (docs/cli-spec.md -> "list"). Gated by
// CapabilityWorkRead. State defaults to reviewable when unset;
// awaiting_review narrows to reviewable branches awaiting the calling
// agent's verdict -- meaningless for an admin caller (no agent identity),
// so it is silently a no-op filter for one, the conservative reading given
// the spec does not address that combination.
func (h *Handler) ListWorkBranches(ctx context.Context, req *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkRead); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	filter := workbranchstore.ListFilter{Target: req.Msg.GetTarget(), Author: req.Msg.GetAuthor()}
	var knownRepoName string
	if repoName := req.Msg.GetRepo(); repoName != "" {
		repoRow, err := h.repos.GetRepoByName(ctx, repoName)
		if err != nil {
			return nil, h.errors.ToConnectErr(mapRepoStoreErr(err, fmt.Sprintf("repo %s", repoName)))
		}
		knownRepoName = repoRow.Name
		filter.RepoID = &repoRow.ID
	}
	state, err := listWorkBranchesStateFilter(req.Msg.State)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	filter.State = state
	if req.Msg.GetAwaitingReview() {
		if identity, ok := httpauth.IdentityFromContext(ctx); ok {
			filter.AwaitingVerdictReviewer = identity.Identifier()
		}
	}
	limit, offset := pageLimitOffset(req.Msg.GetPage())
	branches, total, err := h.workBranches.List(ctx, filter, limit, offset)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing work branches: %w", err))
	}
	names, err := h.repoNamesFor(ctx, branches, knownRepoName)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	result := make([]*loamv1.WorkBranch, len(branches))
	for i, wb := range branches {
		result[i] = workBranchToProto(names[wb.RepoID], wb)
	}
	return connect.NewResponse(&loamv1.ListWorkBranchesResponse{
		WorkBranches: result,
		PageInfo:     &loamv1.PageInfo{Total: uint32(total)},
		Truncated:    int64(offset)+int64(len(branches)) < total,
	}), nil
}

// GetWorkBranch fetches a work branch's metadata -- no diff, no threads --
// separately from GetWorkBranchDiff to keep each response small
// (docs/cli-spec.md -> "show"). Gated by CapabilityWorkRead.
//
// The round is looked up via the same RoundStore.CurrentRound RequestReview
// and ReplyToThread already use, but unlike those callers a branch with no
// round yet (still DRAFT) is not an error here -- reviewstore.ErrNoCurrentRound
// just leaves the response's Round unset, matching UpstreamPRURL's
// absent-vs-present convention below.
func (h *Handler) GetWorkBranch(ctx context.Context, req *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkRead); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repoRow, wb, err := h.resolveWorkBranch(ctx, req.Msg.GetRepo(), req.Msg.GetWorkBranch())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	round, err := h.rounds.CurrentRound(ctx, wb.ID)
	if err != nil && !errors.Is(err, reviewstore.ErrNoCurrentRound) {
		return nil, h.errors.ToConnectErr(fmt.Errorf("looking up current review round for work branch %s/%s: %w", req.Msg.GetRepo(), req.Msg.GetWorkBranch(), err))
	}
	resp := &loamv1.GetWorkBranchResponse{WorkBranch: workBranchToProto(repoRow.Name, wb)}
	if err == nil {
		resp.Round = &loamv1.GetWorkBranchResponse_Round{Number: uint32(round.Number), RequestedBy: round.RequestedBy}
	}
	return connect.NewResponse(resp), nil
}

// GetWorkBranchDiff fetches a work branch's unified diff against its
// target, separately from GetWorkBranch to keep both responses small
// (docs/cli-spec.md -> "diff"). Gated by CapabilityWorkRead. See
// DiffComputer's doc comment: implemented by internal/gitdiff.Computer
// (loam-fwk), wired in cmd/server/main.go. mapDiffComputerErr classifies
// the errors that surface here.
func (h *Handler) GetWorkBranchDiff(ctx context.Context, req *connect.Request[loamv1.GetWorkBranchDiffRequest]) (*connect.Response[loamv1.GetWorkBranchDiffResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkRead); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	_, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	diff, err := h.diff.Diff(ctx, wb)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapDiffComputerErr(err, fmt.Sprintf("computing diff for work branch %s/%s", repo, name)))
	}
	return connect.NewResponse(&loamv1.GetWorkBranchDiffResponse{Diff: diff}), nil
}

// resolveWorkBranch resolves repo and name to the enrolled repo row and the
// named work branch within it, the (repo, work_branch) identity every RPC
// but List and Create is keyed on. An empty repo or name is rejected before
// either store is consulted.
func (h *Handler) resolveWorkBranch(ctx context.Context, repo, name string) (reposstore.Repo, workbranchstore.WorkBranch, error) {
	if repo == "" || name == "" {
		return reposstore.Repo{}, workbranchstore.WorkBranch{}, fmt.Errorf("repo and work branch are required: %w", handler.ErrInvalidArgument)
	}
	repoRow, err := h.repos.GetRepoByName(ctx, repo)
	if err != nil {
		return reposstore.Repo{}, workbranchstore.WorkBranch{}, mapRepoStoreErr(err, fmt.Sprintf("repo %s", repo))
	}
	wb, err := h.workBranches.GetByName(ctx, repoRow.ID, name)
	if err != nil {
		return reposstore.Repo{}, workbranchstore.WorkBranch{}, mapWorkBranchStoreErr(err, fmt.Sprintf("work branch %s/%s", repo, name))
	}
	return repoRow, wb, nil
}

// repoNamesFor resolves the repo name for every distinct RepoID among
// branches. When knownRepoName is non-empty (ListWorkBranches' request
// already named one repo, so every row shares it), it is used directly with
// no further store calls; otherwise each distinct repo id is resolved via
// RepoStore.GetRepoByID once and cached, since an unfiltered list can span
// every enrolled repo.
func (h *Handler) repoNamesFor(ctx context.Context, branches []workbranchstore.WorkBranch, knownRepoName string) (map[uuid.UUID]string, error) {
	names := make(map[uuid.UUID]string, len(branches))
	for _, wb := range branches {
		if _, ok := names[wb.RepoID]; ok {
			continue
		}
		if knownRepoName != "" {
			names[wb.RepoID] = knownRepoName
			continue
		}
		repoRow, err := h.repos.GetRepoByID(ctx, wb.RepoID)
		if err != nil {
			return nil, fmt.Errorf("resolving repo name for work branch %s: %w", wb.Name, err)
		}
		names[wb.RepoID] = repoRow.Name
	}
	return names, nil
}

// authorIdentifier resolves the caller's agent identity for
// CreateWorkBranch's author column. Starting a work branch is an
// agent-only operation in practice (docs/web-spec.md -> ProposalService
// lists only GetWorkBranch, GetWorkBranchDiff, ListComments, and
// RequestReview as operations the admin reaches as superuser --
// CreateWorkBranch is not among them), so a caller with no resolvable
// agent identity (an admin, or a defence-in-depth gap in identity
// resolution) is rejected here rather than silently attributed to nothing.
func authorIdentifier(ctx context.Context) (string, error) {
	identity, ok := httpauth.IdentityFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("starting a work branch requires an agent identity: %w", handler.ErrInvalidArgument)
	}
	return identity.Identifier(), nil
}

// requestReviewActor resolves the review round's requested_by value: the
// caller's agent identity, or adminRoundActor for the admin's send-back
// path (no agent identity to render).
func requestReviewActor(ctx context.Context) string {
	if identity, ok := httpauth.IdentityFromContext(ctx); ok {
		return identity.Identifier()
	}
	return adminRoundActor
}

// mapRepoStoreErr maps a reposstore error to the handler.Err* sentinel
// ErrorMapper recognizes, prefixing context.
func mapRepoStoreErr(err error, context string) error {
	if errors.Is(err, reposstore.ErrNotFound) {
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	}
	return fmt.Errorf("resolving %s: %w", context, err)
}

// mapWorkBranchStoreErr maps a workbranchstore transition/lookup error to
// the handler.Err* sentinel ErrorMapper recognizes, prefixing context.
// workbranchstore.ErrIllegalTransition covers both "wrong current state"
// and (for UpdateState's reviewable transitions) "title/description not
// yet set" -- the store's guarded UPDATE checks both in the same atomic
// statement, so this handler cannot and does not try to tell them apart;
// both are a failed precondition from the caller's point of view. err is
// wrapped alongside handler.ErrFailedPrecondition (Go 1.20+'s multi-%w),
// rather than discarded: err already names the work branch id and the
// attempted verb (workbranchstore's own "%s work branch %s: %w"
// wrapping in transitionErr), and dropping it left the message an
// operator or agent sees terminate in the generic "handler: failed
// precondition" with nothing pointing at the actual cause -- exactly the
// defect loam-blc fixed for mapDiffComputerErr, reproduced here
// (loam-dq0o). errors.Is(mapped, workbranchstore.ErrIllegalTransition) and
// errors.Is(mapped, handler.ErrFailedPrecondition) both still hold.
func mapWorkBranchStoreErr(err error, context string) error {
	switch {
	case errors.Is(err, workbranchstore.ErrNotFound):
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	case errors.Is(err, workbranchstore.ErrIllegalTransition):
		return fmt.Errorf("%s: %w: %w", context, err, handler.ErrFailedPrecondition)
	default:
		return fmt.Errorf("%s: %w", context, err)
	}
}

// mapDiffComputerErr maps a DiffComputer error to the handler.Err*
// sentinel ErrorMapper recognizes, prefixing context. gitdiff.ErrRefMissing
// and gitdiff.ErrNoMergeBase both map to ErrFailedPrecondition: the request
// (repo, work branch) is valid -- resolveWorkBranch already confirmed the
// work branch itself exists before Diff is ever called -- but the mirror's
// current state (a missing ref, or target and name sharing no merge base)
// does not support computing the range. Both cases wrap err itself
// alongside handler.ErrFailedPrecondition (Go 1.20+'s multi-%w), rather
// than discarding it: err already names the missing ref or the ref pair
// with no merge base (internal/gitdiff's own %w wrapping), and dropping it
// left the message an operator or agent sees terminate in the generic
// "handler: failed precondition" with nothing pointing at the actual
// cause (loam-blc). errors.Is(mapped, gitdiff.ErrRefMissing) and
// errors.Is(mapped, handler.ErrFailedPrecondition) both still hold.
// gitdiff.ErrMirrorMissing has deliberately no case here: a bare mirror
// missing on disk for an enrolled repo is an operational fault, not a
// caller-fixable precondition, so it falls through to the default branch
// below and ErrorMapper's own CodeInternal-and-log path -- the same "loud
// failure over silent wrong behavior" choice notImplementedDiffComputer
// made for every error before this bead, now narrowed to just the cases
// that are genuinely someone's operational problem rather than the
// caller's.
func mapDiffComputerErr(err error, context string) error {
	switch {
	case errors.Is(err, gitdiff.ErrRefMissing), errors.Is(err, gitdiff.ErrNoMergeBase):
		return fmt.Errorf("%s: %w: %w", context, err, handler.ErrFailedPrecondition)
	default:
		return fmt.Errorf("%s: %w", context, err)
	}
}

// mapRefWriterErr maps a WorkBranchRefWriter failure to the handler.Err*
// sentinel ErrorMapper recognizes.
//
// gitref.ErrTargetMissing is the one caller-visible precondition here, and
// it is genuinely reachable without anything being broken: CreateWorkBranch
// validates `from` against repo_target_branches, while the ref lives in the
// mirror, so a repo enrolled moments ago whose first sync has not landed
// yet has the row and not the ref. "Try again once the repo has synced" is
// a precondition, not an internal error, so it maps to
// ErrFailedPrecondition with err wrapped alongside (Go 1.20+ multi-%w) so
// the message still names the branch -- the same shape mapDiffComputerErr
// uses, and for the same reason loam-blc established there.
//
// gitref.ErrMirrorMissing and gitref.ErrRefExists deliberately have no case:
// a missing mirror for an enrolled repo is an operational fault, and a
// colliding ref means randomWorkBranchName produced a name whose ref
// already exists -- neither is anything the calling agent can act on, so
// both fall through to ErrorMapper's CodeInternal-and-log path.
func mapRefWriterErr(err error, context string) error {
	if errors.Is(err, gitref.ErrTargetMissing) {
		return fmt.Errorf("%s: %w: %w", context, err, handler.ErrFailedPrecondition)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// mapRequestReviewErr is mapWorkBranchStoreErr's RequestReview-specific
// sibling: unlike UpdateWorkBranch's SetTitleDescription guard (which
// rejects only a terminal state), UpdateState's reviewable-transition guard
// conflates "wrong current state" and "title or description not yet set"
// into the same workbranchstore.ErrIllegalTransition (both checked in one
// atomic WHERE clause -- internal/db/queries/work_branches.sql). The CLI
// renders err.Message() directly to the caller (docs/cli-spec.md -> Exit
// Codes & Errors), so leaving both causes to print the same generic string
// would mislead an operator; before is the work branch's state as read
// BEFORE the attempted transition (resolveWorkBranch), which is everything
// needed to tell them apart without a second query. err is wrapped
// alongside the hand-written message and handler.ErrFailedPrecondition
// (Go 1.20+'s multi-%w), rather than discarded: err already names the
// work branch id and the attempted verb (workbranchstore's own "%s work
// branch %s: %w" wrapping), and dropping it left errors.Is(mapped,
// workbranchstore.ErrIllegalTransition) false -- the same defect loam-blc
// and loam-dq0o fixed elsewhere in this file (loam-jv8f).
func mapRequestReviewErr(err error, before workbranchstore.WorkBranch, context string) error {
	if errors.Is(err, workbranchstore.ErrIllegalTransition) {
		return fmt.Errorf("%s: %s: %w: %w", context, requestReviewPreconditionMessage(before), err, handler.ErrFailedPrecondition)
	}
	return mapWorkBranchStoreErr(err, context)
}

// requestReviewPreconditionMessage renders the specific, human-readable
// reason RequestReview's ErrIllegalTransition applies to before (the work
// branch's state as read before the attempted transition): a terminal
// state, an already-reviewable branch with an existing round (the
// self-heal in selfHealInterruptedRequestReview already ruled out the
// no-round case before this is ever reached), or a missing title/
// description on the only other states UpdateState's guard allows into
// reviewable (draft, reviewed).
func requestReviewPreconditionMessage(before workbranchstore.WorkBranch) string {
	switch before.State {
	case workbranchstore.StateComplete, workbranchstore.StateClosed:
		return fmt.Sprintf("work branch is %s, a terminal state -- review cannot be requested", before.State)
	case workbranchstore.StateReviewable:
		return "work branch is already reviewable with an open review round"
	default:
		if derefOr(before.Title, "") == "" || derefOr(before.Description, "") == "" {
			return "work branch has no title or description set -- both are required before review can be requested"
		}
		return fmt.Sprintf("work branch is %s, not eligible for this review transition", before.State)
	}
}

// hasTargetBranch reports whether branch is among targets.
func hasTargetBranch(targets []reposstore.TargetBranch, branch string) bool {
	for _, t := range targets {
		if t.Branch == branch {
			return true
		}
	}
	return false
}

// randomWorkBranchName generates a work branch's randomly generated,
// meaning-carrying-nothing name (docs/cli-spec.md -> "start": "The name is
// randomly generated"; example output: "wb-9c2f1a"), never caller- or
// store-supplied otherwise.
func randomWorkBranchName() (string, error) {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return "wb-" + hex.EncodeToString(suffix[:]), nil
}

// listWorkBranchesStateFilter resolves ListWorkBranches' optional state
// filter: unset defaults to reviewable (docs/cli-spec.md -> "list":
// "--state <state> ... defaults to reviewable"); an explicitly present but
// WORK_BRANCH_STATE_UNSPECIFIED value is rejected as a bad filter value
// (docs/cli-spec.md -> "list" -> "Errors: exit 2 on a bad filter value")
// rather than silently treated the same as absent.
func listWorkBranchesStateFilter(state *loamv1.WorkBranchState) (workbranchstore.State, error) {
	if state == nil {
		return workbranchstore.StateReviewable, nil
	}
	mapped := protoToState(*state)
	if mapped == "" {
		return "", fmt.Errorf("state: unspecified is not a valid filter value: %w", handler.ErrInvalidArgument)
	}
	return mapped, nil
}

// pageLimitOffset resolves a request's optional Page to the (limit, offset)
// workbranchstore.Store.List needs: limit 0 (unset) becomes
// defaultListLimit, matching proto's Page.limit contract ("0 means 'use the
// server default'").
func pageLimitOffset(page *loamv1.Page) (int32, int32) {
	limit := int32(defaultListLimit)
	if page.GetLimit() > 0 {
		limit = int32(page.GetLimit())
	}
	return limit, int32(page.GetOffset())
}

// derefOr returns *s, or fallback if s is nil.
func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// stateToProto maps a workbranchstore.State to its proto enum value.
func stateToProto(s workbranchstore.State) loamv1.WorkBranchState {
	switch s {
	case workbranchstore.StateDraft:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT
	case workbranchstore.StateReviewable:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE
	case workbranchstore.StateReviewed:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED
	case workbranchstore.StateComplete:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE
	case workbranchstore.StateClosed:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED
	default:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_UNSPECIFIED
	}
}

// protoToState maps a proto WorkBranchState enum value to its
// workbranchstore.State; WORK_BRANCH_STATE_UNSPECIFIED and any unrecognized
// value both map to the empty State, which callers treat as "no valid
// state" rather than "no filter" (the ListFilter.State sentinel meaning is
// established by filterColumns in workbranchstore, but ListWorkBranches
// always supplies a concrete default before calling List, so that sentinel
// is never reached through this handler).
func protoToState(s loamv1.WorkBranchState) workbranchstore.State {
	switch s {
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT:
		return workbranchstore.StateDraft
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE:
		return workbranchstore.StateReviewable
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED:
		return workbranchstore.StateReviewed
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE:
		return workbranchstore.StateComplete
	case loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED:
		return workbranchstore.StateClosed
	default:
		return ""
	}
}

// workBranchToProto converts a workbranchstore.WorkBranch to its proto
// representation, given the enrolled repo name the store's WorkBranch
// itself does not carry (it only holds RepoID).
func workBranchToProto(repoName string, wb workbranchstore.WorkBranch) *loamv1.WorkBranch {
	return &loamv1.WorkBranch{
		Repo:          repoName,
		Name:          wb.Name,
		Target:        wb.Target,
		Title:         derefOr(wb.Title, ""),
		Description:   derefOr(wb.Description, ""),
		State:         stateToProto(wb.State),
		Author:        wb.Author,
		UpstreamPrUrl: wb.UpstreamPRURL,
	}
}
