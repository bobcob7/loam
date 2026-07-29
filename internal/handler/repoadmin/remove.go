package repoadmin

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// removeRepoListPageSize is how many work_branches rows RemoveRepo's
// guard check reads per page while enumerating blockers. workbranchstore
// has no "state != complete AND state != closed" filter (ListFilter's
// State field matches exactly one value or none at all), so this walks
// every page for the repo rather than requesting a filtered count --
// correct regardless of how many work branches a repo accumulates, at the
// cost of a few extra round trips for an admin-initiated, rare operation.
const removeRepoListPageSize = 200

// RemoveRepo implements docs/web-spec.md's RemoveRepo contract ("Fails
// with failed_precondition while any non-terminal work branch exists,
// enumerating each blocker ... else drops mirror + derived + metadata
// incl. history and deletes ingest jobs") in two parts.
//
// The GUARD is this method's own: resolving the repo, enumerating every
// non-terminal (not COMPLETE/CLOSED) work branch, and returning
// CodeFailedPrecondition with a typed RemovalBlocked error detail (never
// just a string message -- docs/web-spec.md: "travels as a typed Connect
// error detail so the UI renders it structurally") when any exist. That
// guard is also the ONLY confirmation step in this contract: the proto has
// no force/confirm field (RemoveRepoRequest carries exactly `string repo`)
// and none is invented here, because the dangerous case the guard already
// refuses -- open work branches carrying unmerged agent commits -- is
// precisely the case a force flag would exist to override. The spec's
// remedy is explicit and is not "pass force": "accept or close each, then
// remove."
//
// The DELETE is h.deleter's (the repoDeleter interface, interfaces.go):
// the repos row and, through its ON DELETE CASCADE chain,
// repo_target_branches, work_branches and their rounds/verdicts/threads/
// comments, ingest_jobs, and the derived graph/vector indexes, plus the
// bare mirror on disk. cmd/server/main.go wires the real
// internal/reporemove.Remover (loam-cwb); that package's DeleteRepo doc
// comment owns the ordering and partial-failure reasoning, and this method
// deliberately holds none of it.
//
// Nothing in either half touches the upstream forge. Removal unenrolls a
// repo FROM LOAM; the repository it mirrors is untouched, and this package
// has no forge client to touch it with.
func (h *Handler) RemoveRepo(ctx context.Context, req *connect.Request[adminv1.RemoveRepoRequest]) (*connect.Response[adminv1.RemoveRepoResponse], error) {
	if err := requireAdmin(ctx, "removing a repo"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	name := req.Msg.GetRepo()
	if name == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("remove repo: empty repo identifier: %w", handler.ErrInvalidArgument))
	}
	repoRow, err := h.store.GetRepoByName(ctx, name)
	if err != nil {
		if errors.Is(err, reposstore.ErrNotFound) {
			return nil, h.errors.ToConnectErr(fmt.Errorf("repo %s: %w", name, handler.ErrNotFound))
		}
		return nil, h.errors.ToConnectErr(fmt.Errorf("resolving repo %s: %w", name, err))
	}
	blockers, err := h.nonTerminalWorkBranches(ctx, repoRow.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("remove repo %s: enumerating work branches: %w", name, err))
	}
	if len(blockers) > 0 {
		return nil, removalBlockedError(name, blockers)
	}
	if err := h.deleter.DeleteRepo(ctx, repoRow.ID); err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("remove repo %s: %w", name, err))
	}
	return connect.NewResponse(&adminv1.RemoveRepoResponse{}), nil
}

// requireAdmin is defence in depth on top of the routing-level gate, not
// a replacement for it: the whole /loam.admin.v1.* path group is already
// wrapped in httpauth.Auth.AdminOnly before any request reaches a handler
// (docs/web-spec.md -> Auth), which is why this package's doc comment
// records having no per-RPC gate anywhere else.
//
// RemoveRepo is the one exception, for the same reason
// internal/handler/proposal's own requireAdmin exists (loam-ofg.14) and
// on the same narrow line: it is the only RPC in this package that
// destroys data irreversibly. Every other method here creates or edits an
// enrollment, and the worst outcome of a wrongly-admitted call is a state
// an admin can edit back. This one drops a repo's entire history -- work
// branches, review rounds, verdicts, threads, comments -- with no undo,
// and re-enrolling starts fresh (docs/web-spec.md). httpauth.IsAdmin reads
// the flag AdminOnly itself sets, so this costs one context read and makes
// "only an admin can unenroll a repo" a property asserted by this
// package's own tests rather than one inherited from a wiring line in
// cmd/server that no test in this package can see.
func requireAdmin(ctx context.Context, operation string) error {
	if httpauth.IsAdmin(ctx) {
		return nil
	}
	return fmt.Errorf("%s requires the admin superuser: %w", operation, handler.ErrPermissionDenied)
}

// nonTerminalWorkBranches returns every work branch on repoID whose state
// is neither COMPLETE nor CLOSED, paging through workbranchstore.List
// (which has no "not terminal" filter of its own) until every row for
// repoID has been seen.
func (h *Handler) nonTerminalWorkBranches(ctx context.Context, repoID uuid.UUID) ([]workbranchstore.WorkBranch, error) {
	var blockers []workbranchstore.WorkBranch
	offset := int32(0)
	for {
		page, total, err := h.workBranches.List(ctx, workbranchstore.ListFilter{RepoID: &repoID}, removeRepoListPageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, wb := range page {
			if wb.State != workbranchstore.StateComplete && wb.State != workbranchstore.StateClosed {
				blockers = append(blockers, wb)
			}
		}
		offset += int32(len(page))
		if len(page) == 0 || int64(offset) >= total {
			break
		}
	}
	return blockers, nil
}

// removalBlockedError builds the CodeFailedPrecondition connect.Error
// RemoveRepo returns when blockers is non-empty, with the typed
// RemovalBlocked detail docs/web-spec.md requires attached (never just
// encoded into the message). If detail construction itself fails (it
// cannot, in practice -- RemovalBlocked is a plain generated proto
// message with no oneof/any field that could reject marshaling -- but
// connect.NewErrorDetail's signature returns an error and this must not
// panic on it), the error is still returned with a message that
// enumerates every blocker in plain text, so a caller unable to read the
// structured detail is not left with no information at all.
func removalBlockedError(name string, blockers []workbranchstore.WorkBranch) *connect.Error {
	protoBlockers := make([]*adminv1.BlockedWorkBranch, len(blockers))
	names := make([]string, len(blockers))
	for i, wb := range blockers {
		protoBlockers[i] = &adminv1.BlockedWorkBranch{
			Name:  wb.Name,
			Title: stringOrEmpty(wb.Title),
			State: workBranchStateToProto(wb.State),
		}
		names[i] = fmt.Sprintf("%s(%s)", wb.Name, wb.State)
	}
	connErr := connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("remove repo %s: %d non-terminal work branch(es) block removal: %v", name, len(blockers), names))
	if detail, err := connect.NewErrorDetail(&adminv1.RemovalBlocked{Blockers: protoBlockers}); err == nil {
		connErr.AddDetail(detail)
	}
	return connErr
}

// workBranchStateToProto maps a workbranchstore.State to its proto enum,
// mirroring internal/handler/workbranch's own stateToProto (unexported
// there, so necessarily a second copy here, not a shared import).
func workBranchStateToProto(s workbranchstore.State) loamv1.WorkBranchState {
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
