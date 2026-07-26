package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/reposstore"
)

// Handler implements loamv1connect.RepoServiceHandler.
type Handler struct {
	store        RepoStore
	capabilities *handler.CapabilityChecker
	errors       *handler.ErrorMapper
	logger       *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ loamv1connect.RepoServiceHandler = (*Handler)(nil)

// New builds a Handler over store, gating GetRepo with capabilities (the
// git.clone capability, per docs/web-spec.md -> RoleService) and mapping
// domain errors through errors.
func New(store RepoStore, capabilities *handler.CapabilityChecker, errors *handler.ErrorMapper, logger *slog.Logger) *Handler {
	return &Handler{store: store, capabilities: capabilities, errors: errors, logger: logger}
}

// GetRepo resolves an enrolled repo and its eligible work-branch target
// branches (docs/cli-spec.md -> clone: the pre-flight lookup that decides
// whether a repo is enrolled before `loam clone` ever touches the git
// smart-HTTP endpoint). Gated by CapabilityGitClone: an admin basic-auth
// caller bypasses as superuser (handler.CapabilityChecker.RequireCapability);
// an agent whose role lacks git.clone is denied CodePermissionDenied. A
// genuinely unenrolled repo maps to CodeNotFound via reposstore.ErrNotFound
// -- the DEMO M2 BLOCKER this handler's existence fixes: before this
// package existed, every /loam.v1.RepoService/* request fell through to
// the group-level 404 fallback (internal/server/fallback.go), which also
// answers CodeNotFound but for an entirely different, indistinguishable
// reason ("no service registered"). See this package's tests for the
// distinguishing log-line proof.
func (h *Handler) GetRepo(ctx context.Context, req *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityGitClone); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	name := req.Msg.GetRepo()
	if name == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("repo: empty repo identifier: %w", handler.ErrInvalidArgument))
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
	branches := make([]string, len(targets))
	for i, target := range targets {
		branches[i] = target.Branch
	}
	return connect.NewResponse(&loamv1.GetRepoResponse{
		Repo:           repoRow.Name,
		TargetBranches: branches,
	}), nil
}
