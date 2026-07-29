package meta

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
)

// Handler implements loamv1connect.MetaServiceHandler.
type Handler struct {
	roles  RoleStore
	errors *handler.ErrorMapper
	logger *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ loamv1connect.MetaServiceHandler = (*Handler)(nil)

// New builds a Handler over roles, mapping domain errors through errors.
// Unlike every other loam.v1 handler constructor in this repo, New takes
// no *handler.CapabilityChecker: GetInstructions is never capability-gated
// (see package doc), so there is nothing for one to check.
func New(roles RoleStore, errors *handler.ErrorMapper, logger *slog.Logger) *Handler {
	return &Handler{roles: roles, errors: errors, logger: logger}
}

// GetInstructions returns the static usage guide, the commands available
// to the caller's role, and that role's configured instructions text
// (docs/cli-spec.md -> instructions); when req.Msg.Command is set, the
// response's Commands is narrowed to that single command's entry.
//
// This RPC is NEVER capability-gated: any identified caller -- an admin
// basic-auth superuser or an agent presenting any role at all -- may call
// it (README -> Agent Identity and Roles: "instructions and whoami are
// always available and ungated"; docs/web-spec.md -> Auth: "ungated means
// they skip the capability check", not that the RPC is reachable
// anonymously -- internal/httpauth.Auth.CLI already rejects every request
// lacking a resolved caller before this handler ever runs). An admin
// caller has no role of its own, so it is treated as a superuser here too:
// every command is reported available, and RoleInstructions is empty
// (there is no admin "role" row to read instructions from).
func (h *Handler) GetInstructions(ctx context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
	granted, instructions, err := h.resolveCaller(ctx)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	commands := filterCommands(granted)
	if requested := req.Msg.GetCommand(); requested != "" {
		entry, ok := findCommand(commands, requested)
		if !ok {
			return nil, h.errors.ToConnectErr(fmt.Errorf("command %s: %w", requested, handler.ErrNotFound))
		}
		commands = []*loamv1.CommandInfo{entry}
	}
	return connect.NewResponse(&loamv1.GetInstructionsResponse{
		Usage:            usageText,
		Commands:         commands,
		RoleInstructions: instructions,
	}), nil
}

// resolveCaller resolves the granted capabilities and instructions text
// for the caller resolved from ctx by internal/httpauth: every capability
// and no instructions for an admin superuser, or the role store's answer
// for an agent identity. Defence-in-depth only (see doc comment above):
// internal/httpauth.Auth.CLI rejects every request with neither before it
// reaches a handler, so the "no resolved caller" branch below should be
// unreachable in production; it exists so a future wrapper regression
// fails loudly (ErrPermissionDenied) instead of resolving an empty role
// silently.
func (h *Handler) resolveCaller(ctx context.Context) (granted []handler.Capability, instructions string, err error) {
	if httpauth.IsAdmin(ctx) {
		// The fixed vocabulary in its entirety, read from its single
		// source of truth in internal/handler rather than restated here:
		// RequireCapability's own admin bypass means an admin's role is
		// never gated on any single RPC, so GetInstructions reports every
		// gated command as available to them too, rather than resolving a
		// role that does not exist for an admin
		// (internal/httpauth.IdentityFromContext: "ok is false ... for
		// every request on a path group the admin reached as superuser").
		return handler.AllCapabilities(), "", nil
	}
	identity, ok := httpauth.IdentityFromContext(ctx)
	if !ok {
		return nil, "", fmt.Errorf("no resolved caller: %w", handler.ErrPermissionDenied)
	}
	granted, err = h.roles.RoleCapabilities(ctx, identity.Role)
	if err != nil {
		return nil, "", fmt.Errorf("resolving capabilities for role %s: %w", identity.Role, err)
	}
	instructions, err = h.roles.RoleInstructions(ctx, identity.Role)
	if err != nil {
		return nil, "", fmt.Errorf("resolving instructions for role %s: %w", identity.Role, err)
	}
	return granted, instructions, nil
}
