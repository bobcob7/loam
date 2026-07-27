package repoadmin

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/reposstore"
)

// GetRepo resolves one enrolled repo's full admin-facing status
// (docs/web-spec.md -> RepoAdminService "GetRepo"): richer than the CLI's
// read-only loam.v1.RepoService.GetRepo, since it also reports sync
// status and the indexed branch's last ingested ref.
func (h *Handler) GetRepo(ctx context.Context, req *connect.Request[adminv1.GetRepoRequest]) (*connect.Response[adminv1.GetRepoResponse], error) {
	name := req.Msg.GetRepo()
	if name == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("repo admin: empty repo identifier: %w", handler.ErrInvalidArgument))
	}
	repoRow, err := h.store.GetRepoByName(ctx, name)
	if err != nil {
		if errors.Is(err, reposstore.ErrNotFound) {
			return nil, h.errors.ToConnectErr(fmt.Errorf("repo %s: %w", name, handler.ErrNotFound))
		}
		return nil, h.errors.ToConnectErr(fmt.Errorf("resolving repo %s: %w", name, err))
	}
	targets, err := h.store.ListTargetBranches(ctx, repoRow.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing target branches for repo %s: %w", name, err))
	}
	return connect.NewResponse(&adminv1.GetRepoResponse{Repo: toEnrolledRepo(repoRow, targets)}), nil
}

// ListRepos returns one page of enrolled repos with status
// (docs/web-spec.md -> RepoAdminService "ListRepos").
func (h *Handler) ListRepos(ctx context.Context, req *connect.Request[adminv1.ListReposRequest]) (*connect.Response[adminv1.ListReposResponse], error) {
	limit, offset := pageParams(req.Msg.GetPage())
	result, err := h.store.ListRepos(ctx, reposstore.Page{Limit: int(limit), Offset: int(offset)})
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing repos: %w", err))
	}
	repos := make([]*adminv1.EnrolledRepo, len(result.Repos))
	for i, repoRow := range result.Repos {
		targets, err := h.store.ListTargetBranches(ctx, repoRow.ID)
		if err != nil {
			return nil, h.errors.ToConnectErr(fmt.Errorf("listing target branches for repo %s: %w", repoRow.Name, err))
		}
		repos[i] = toEnrolledRepo(repoRow, targets)
	}
	return connect.NewResponse(&adminv1.ListReposResponse{
		Repos:    repos,
		PageInfo: &loamv1.PageInfo{Total: uint32(result.Total)},
	}), nil
}

// toEnrolledRepo converts a reposstore.Repo plus its target branches into
// the admin API's richer EnrolledRepo message (docs/web-spec.md:
// "{ repo, upstream_url, target_branches[], indexed_branch, SyncStatus
// sync, string ingested_ref }").
func toEnrolledRepo(repoRow reposstore.Repo, targets []reposstore.TargetBranch) *adminv1.EnrolledRepo {
	branches := make([]string, len(targets))
	ingestedRef := ""
	for i, target := range targets {
		branches[i] = target.Branch
		if target.Branch == repoRow.IndexedBranch && target.IngestedRef.Ok {
			ingestedRef = target.IngestedRef.Ref
		}
	}
	return &adminv1.EnrolledRepo{
		Repo:           repoRow.Name,
		UpstreamUrl:    repoRow.UpstreamURL,
		TargetBranches: branches,
		Sync: &adminv1.SyncStatus{
			State:        syncStateToProto(repoRow.SyncState),
			LastSyncedAt: timeOrEmpty(repoRow.LastSyncedAt),
			Error:        stringOrEmpty(repoRow.SyncError),
		},
		IndexedBranch: repoRow.IndexedBranch,
		IngestedRef:   ingestedRef,
	}
}
