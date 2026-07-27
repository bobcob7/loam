package repoadmin

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/reposstore"
)

// SetTargetBranches replaces repo's eligible target branches and
// designates indexed_branch (docs/web-spec.md -> RepoAdminService
// "SetTargetBranches"). Removing a branch from the set only affects
// future eligibility: this method never touches work_branches (its
// `target` column is a plain string, not an FK into
// repo_target_branches), so an existing work branch targeting a
// now-delisted branch keeps its full recorded target and lifecycle
// untouched -- docs/web-spec.md: "Removing a target branch only affects
// *eligibility* -- existing work branches keep their recorded target and
// full lifecycle". Changing indexed_branch enqueues a FULL ingest job for
// the new branch (docs/web-spec.md: "triggers a full ingest of the new
// branch").
func (h *Handler) SetTargetBranches(ctx context.Context, req *connect.Request[adminv1.SetTargetBranchesRequest]) (*connect.Response[adminv1.SetTargetBranchesResponse], error) {
	name := req.Msg.GetRepo()
	targetBranches := req.Msg.GetTargetBranches()
	indexedBranch := req.Msg.GetIndexedBranch()
	if name == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set target branches: empty repo identifier: %w", handler.ErrInvalidArgument))
	}
	if len(targetBranches) == 0 {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set target branches: at least one target branch is required: %w", handler.ErrInvalidArgument))
	}
	if indexedBranch == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set target branches: empty indexed_branch: %w", handler.ErrInvalidArgument))
	}
	if !slices.Contains(targetBranches, indexedBranch) {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set target branches: indexed_branch %q must be one of target_branches: %w", indexedBranch, handler.ErrInvalidArgument))
	}
	repoRow, err := h.store.GetRepoByName(ctx, name)
	if err != nil {
		if errors.Is(err, reposstore.ErrNotFound) {
			return nil, h.errors.ToConnectErr(fmt.Errorf("repo %s: %w", name, handler.ErrNotFound))
		}
		return nil, h.errors.ToConnectErr(fmt.Errorf("resolving repo %s: %w", name, err))
	}
	existing, err := h.store.ListTargetBranches(ctx, repoRow.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set target branches for repo %s: listing existing: %w", name, err))
	}
	if err := h.reconcileTargetBranches(ctx, repoRow.ID, name, existing, targetBranches); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	indexedChanged := repoRow.IndexedBranch != indexedBranch
	updated, err := h.store.UpdateRepo(ctx, repoRow.ID, reposstore.UpdateRepoParams{
		UpstreamURL:   repoRow.UpstreamURL,
		ForgeHost:     repoRow.ForgeHost,
		IndexedBranch: indexedBranch,
	})
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set target branches for repo %s: updating indexed branch: %w", name, err))
	}
	if indexedChanged {
		if err := h.ingest.Enqueue(ctx, repoRow.ID, indexedBranch, ingest.KindFull); err != nil {
			h.logger.ErrorContext(ctx, "set target branches: failed to enqueue full ingest for new indexed branch", "repo", name, "indexed_branch", indexedBranch, "error", err)
		}
	}
	targets, err := h.store.ListTargetBranches(ctx, repoRow.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("set target branches for repo %s: listing target branches for response: %w", name, err))
	}
	return connect.NewResponse(&adminv1.SetTargetBranchesResponse{Repo: toEnrolledRepo(updated, targets)}), nil
}

// reconcileTargetBranches adds every branch in wanted not already in
// existing, and removes every existing branch not in wanted -- the set
// difference SetTargetBranches' "replace" semantics need, without ever
// touching a branch present in both (preserving its ingested_ref/
// ingested_at, per AddTargetBranch's own idempotency doc comment).
func (h *Handler) reconcileTargetBranches(ctx context.Context, repoID uuid.UUID, name string, existing []reposstore.TargetBranch, wanted []string) error {
	existingSet := make(map[string]bool, len(existing))
	for _, target := range existing {
		existingSet[target.Branch] = true
	}
	wantedSet := make(map[string]bool, len(wanted))
	for _, branch := range wanted {
		wantedSet[branch] = true
	}
	for _, branch := range wanted {
		if existingSet[branch] {
			continue
		}
		if _, err := h.store.AddTargetBranch(ctx, repoID, branch); err != nil {
			return fmt.Errorf("set target branches for repo %s: adding target branch %s: %w", name, branch, err)
		}
	}
	for _, target := range existing {
		if wantedSet[target.Branch] {
			continue
		}
		if err := h.store.RemoveTargetBranch(ctx, repoID, target.Branch); err != nil {
			return fmt.Errorf("set target branches for repo %s: removing target branch %s: %w", name, target.Branch, err)
		}
	}
	return nil
}
