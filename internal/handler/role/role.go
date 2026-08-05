package role

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"connectrpc.com/connect"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/rolestore"
)

// maxRoleNameLen bounds a role name. roles.name is unbounded `text`, so
// this is not a schema echo: a role name's only purpose is to travel back
// in a Loam-Agent-Role request header (internal/httpauth), and a name too
// long to carry there is a role no agent could ever present. 64 is
// comfortably above anything human-authored -- the built-ins are 6 and 8
// characters.
const maxRoleNameLen = 64

// Handler implements adminv1connect.RoleServiceHandler.
type Handler struct {
	roles  roleStore
	errors *handler.ErrorMapper
	logger *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ adminv1connect.RoleServiceHandler = (*Handler)(nil)

// New builds a Handler over the role store.
func New(roles roleStore, errors *handler.ErrorMapper, logger *slog.Logger) *Handler {
	return &Handler{roles: roles, errors: errors, logger: logger}
}

// ListRoles returns every role, built-in and admin-defined, ordered by
// name (docs/web-spec.md -> RoleService). Unpaginated, matching the proto:
// ListRolesRequest carries no Page, because roles are operator-authored
// configuration a handful of rows deep, not unbounded user data.
func (h *Handler) ListRoles(ctx context.Context, _ *connect.Request[adminv1.ListRolesRequest]) (*connect.Response[adminv1.ListRolesResponse], error) {
	if err := requireAdmin(ctx, "listing roles"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	roles, err := h.roles.ListRoles(ctx)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing roles: %w", err))
	}
	out := make([]*adminv1.Role, 0, len(roles))
	for _, role := range roles {
		out = append(out, roleToProto(role))
	}
	return connect.NewResponse(&adminv1.ListRolesResponse{Roles: out}), nil
}

// GetRole returns one role by name, or CodeNotFound if no such role
// exists. Absence is a real not-found here, unlike
// CredentialService.GetCredentialStatus's deliberate "no credential" answer
// for an unconfigured host: a Role has no "does not exist" representation
// to return -- every field would have to be fabricated -- and an admin
// asking about a role that is not there has made a typo, not discovered an
// unconfigured one.
func (h *Handler) GetRole(ctx context.Context, req *connect.Request[adminv1.GetRoleRequest]) (*connect.Response[adminv1.GetRoleResponse], error) {
	if err := requireAdmin(ctx, "reading a role"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	name, err := validateName(req.Msg.GetName())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	role, err := h.roles.GetRole(ctx, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapStoreErr(err, name))
	}
	return connect.NewResponse(&adminv1.GetRoleResponse{Role: roleToProto(role)}), nil
}

// CreateRole creates an admin-defined role with the operations and
// instructions the request carries (docs/web-spec.md -> RoleService).
//
// A request whose Role.builtin is true is REFUSED with
// CodeInvalidArgument rather than silently created as a normal role. The
// builtin flag is what makes DeleteRole refuse a role, so an admin who
// asked for one and got a deletable role back would hold a protection they
// do not have. Only migration 0001_init sets that flag, and the store's
// CreateRole statement does not accept it as a parameter at all -- this
// check exists so the caller is TOLD, rather than having the field quietly
// dropped. UpdateRole deliberately does the opposite; see there.
func (h *Handler) CreateRole(ctx context.Context, req *connect.Request[adminv1.CreateRoleRequest]) (*connect.Response[adminv1.CreateRoleResponse], error) {
	if err := requireAdmin(ctx, "creating a role"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	params, err := roleParams(req.Msg.GetRole())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	if req.Msg.GetRole().GetBuiltin() {
		return nil, h.errors.ToConnectErr(fmt.Errorf("creating role %s: builtin is assigned by Loam and cannot be requested (only the roles seeded by migration are built-in): %w", params.Name, handler.ErrInvalidArgument))
	}
	role, err := h.roles.CreateRole(ctx, params)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapStoreErr(err, params.Name))
	}
	h.logger.InfoContext(ctx, "admin created role", "role", role.Name, "operations", role.Operations)
	return connect.NewResponse(&adminv1.CreateRoleResponse{Role: roleToProto(role)}), nil
}

// UpdateRole replaces a role's granted operations and instruction text
// with what the request carries (docs/web-spec.md -> RoleService:
// "set the granted operations and instructions"). REPLACE, not merge: the
// request carries a whole Role, so the operations it names are the
// operations the role is to have -- a merge would leave no way to revoke
// one, which is half of what features/roles.feature's "Updating a role
// changes what its agents may do" exercises.
//
// # Built-in roles ARE updatable
//
// docs/web-spec.md restricts exactly one thing about a built-in: it
// "cannot be deleted". Nothing there, in the proto, or on this bead
// restricts updating one, and two independent things in the tree require
// that it stay allowed:
//
//   - Both built-ins are seeded with instructions set to the empty
//     string (0001_init.up.sql). If UpdateRole refused them, the author
//     and reviewer instruction text could never be set through any
//     surface, and features/roles.feature's "A role's instructions reach
//     its agents" -- which configures instructions on the *reviewer*, a
//     built-in -- would be unimplementable.
//   - An admin who tightens or loosens what a reviewer may do is doing the
//     job this service exists for. The built-in flag marks a role as
//     *shipped*, not as *frozen*.
//
// Consequently Role.builtin in the REQUEST is ignored rather than
// rejected, the opposite of CreateRole's treatment of it. The natural
// admin flow is read-modify-write -- GetRole, change operations,
// UpdateRole -- which round-trips builtin: true straight back for a
// built-in role. Rejecting it would break that flow for precisely the two
// roles this method must keep editable. The response always reports the
// stored truth, which this method never writes.
//
// Renaming is not expressible: UpdateRoleRequest carries one name, and it
// identifies the role. RoleService has no rename RPC (mirroring
// repos.name, which loam-54o.7 settled as immutable for the same reason --
// the name is the key every other surface holds).
func (h *Handler) UpdateRole(ctx context.Context, req *connect.Request[adminv1.UpdateRoleRequest]) (*connect.Response[adminv1.UpdateRoleResponse], error) {
	if err := requireAdmin(ctx, "updating a role"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	params, err := roleParams(req.Msg.GetRole())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	role, err := h.roles.UpdateRole(ctx, params)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapStoreErr(err, params.Name))
	}
	h.logger.InfoContext(ctx, "admin updated role", "role", role.Name, "builtin", role.Builtin, "operations", role.Operations)
	return connect.NewResponse(&adminv1.UpdateRoleResponse{Role: roleToProto(role)}), nil
}

// DeleteRole removes an admin-defined role. A built-in role is refused
// with CodeFailedPrecondition -- the request is well-formed and the role
// exists; its state does not permit deletion (docs/web-spec.md ->
// RoleService: "built-in roles cannot be deleted"; features/roles.feature:
// "Built-in roles cannot be deleted").
//
// The role is READ first, rather than leaning on the store's own
// `AND NOT builtin` predicate, because a bare zero-rows delete cannot tell
// "author is built-in" from "there is no role called author" -- and those
// are a failed precondition and a not-found respectively, which is the
// whole distinction an admin needs. The store's predicate remains as the
// backstop underneath: if this check ever regressed, the built-in would
// still survive and the admin would merely get the wrong reason.
//
// Agents whose role this deletes are not enumerated or warned. A missing
// role fails closed at the next request (the capability check cannot
// resolve it), which is the direction this system errs in everywhere
// else.
func (h *Handler) DeleteRole(ctx context.Context, req *connect.Request[adminv1.DeleteRoleRequest]) (*connect.Response[adminv1.DeleteRoleResponse], error) {
	if err := requireAdmin(ctx, "deleting a role"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	name, err := validateName(req.Msg.GetName())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	role, err := h.roles.GetRole(ctx, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapStoreErr(err, name))
	}
	if role.Builtin {
		return nil, h.errors.ToConnectErr(fmt.Errorf("role %s is built in and cannot be deleted: %w", name, handler.ErrFailedPrecondition))
	}
	if err := h.roles.DeleteRole(ctx, name); err != nil {
		return nil, h.errors.ToConnectErr(mapStoreErr(err, name))
	}
	h.logger.InfoContext(ctx, "admin deleted role", "role", name)
	return connect.NewResponse(&adminv1.DeleteRoleResponse{}), nil
}

// requireAdmin is defence in depth on top of the routing-level gate, not a
// replacement for it: the whole /loam.admin.v1.* path group is already
// wrapped in httpauth.Auth.AdminOnly before any request reaches a handler
// (docs/web-spec.md -> Auth), which is why internal/handler/repoadmin
// documents having no per-RPC gate on most of its surface.
//
// internal/handler/proposal and internal/handler/credential both added one
// anyway, and repoadmin's RemoveRepo followed on a narrow line: an RPC that
// destroys data irreversibly. That line alone would cover DeleteRole here
// and leave the other four uncovered. It is not the line this package
// draws, because it is not the strongest fact about this service.
//
// This package is the ONLY runtime writer of the roles and role_operations
// rows that internal/handler.CapabilityChecker reads on every gated RPC in
// the system. A wrongly-admitted CreateRole or UpdateRole does not damage a
// resource an admin can edit back -- it rewrites the policy every other
// gate consults, and it does so silently and permanently until someone
// thinks to re-read the role list. Granting git.push to a role hands push
// access to every agent already presenting it. That is a stronger claim
// than "irreversible", and it covers all three mutating RPCs, not just the
// destructive one.
//
// The two READS are gated on internal/handler/credential's reasoning
// rather than this one: the full role list is the authorization model of
// the server -- which operations exist, which roles hold them, which roles
// an agent could usefully impersonate given that Loam-Agent-Role is
// trusted, unauthenticated input (docs/web-spec.md -> Auth). Handing that
// to an unauthenticated caller is reconnaissance for exactly the
// impersonation the MVP's trusted-header model already makes cheap.
//
// httpauth.IsAdmin reads the flag AdminOnly itself sets, so this costs one
// context read per RPC and makes "only an admin can read or change the
// authorization policy" a property asserted by this package's own tests
// rather than one inherited from a wiring line in cmd/server that no test
// in this package can see.
func requireAdmin(ctx context.Context, operation string) error {
	if httpauth.IsAdmin(ctx) {
		return nil
	}
	return fmt.Errorf("%s requires the admin superuser: %w", operation, handler.ErrPermissionDenied)
}

// roleParams validates a Role message from a create/update request and
// converts it to the store's write params. It does NOT look at builtin --
// the two callers treat that field differently and each handles it itself.
func roleParams(role *adminv1.Role) (rolestore.RoleParams, error) {
	if role == nil {
		return rolestore.RoleParams{}, fmt.Errorf("role is required: %w", handler.ErrInvalidArgument)
	}
	name, err := validateName(role.GetName())
	if err != nil {
		return rolestore.RoleParams{}, err
	}
	operations, err := validateOperations(name, role.GetOperations())
	if err != nil {
		return rolestore.RoleParams{}, err
	}
	return rolestore.RoleParams{
		Name:         name,
		Instructions: role.GetInstructions(),
		Operations:   operations,
	}, nil
}

// validateName trims and checks a role name. The charset is deliberately
// narrow -- ASCII letters, digits, '-', '_', '.' -- because a role name's
// destination is a Loam-Agent-Role request header written into every
// clone's git config (docs/git-spec.md -> Identity on Git Operations). A
// name carrying whitespace, a control character, or a non-ASCII rune is a
// name no agent can present verbatim, so creating one would produce a role
// that looks configured and can never be used. Refusing it at creation is
// the only point at which that is still fixable.
func validateName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("role name is required: %w", handler.ErrInvalidArgument)
	}
	if len(name) > maxRoleNameLen {
		return "", fmt.Errorf("role name %q is longer than %d characters: %w", name, maxRoleNameLen, handler.ErrInvalidArgument)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return "", fmt.Errorf("role name %q contains %q: names may use only letters, digits, '-', '_', and '.', since the name travels verbatim in the Loam-Agent-Role header: %w", name, r, handler.ErrInvalidArgument)
		}
	}
	return name, nil
}

// validateOperations checks every requested operation against the fixed
// capability vocabulary and returns the granted set, de-duplicated and
// sorted.
//
// # The vocabulary is read, never restated
//
// The membership test is handler.Capability.Valid and the list quoted back
// in the rejection message is handler.AllCapabilities -- both reading the
// one slice in internal/handler/capability.go. This package does not hold
// a list of the ten operations anywhere, on purpose: a second copy would
// drift, and the failure would be silent in the worst direction (an
// operation an admin is entitled to grant, rejected as unknown, or -- if
// the copy grew a member -- an unknown string stored as a capability that
// no gate will ever honour). role_operations_operation_check
// (0001_init.up.sql) is a third statement of the same vocabulary that
// cannot be expressed in Go; see vocabulary_integration_test.go, which
// asserts it agrees with this one rather than trusting that it does.
//
// De-duplication is silent rather than an error: PRIMARY KEY (role_id,
// operation) would fail the whole transaction on a repeat, and an admin who
// sent "search" twice unmistakably meant to grant it once. Sorting makes
// the stored order match the ORDER BY every read path uses, so a created
// role and the same role read back compare equal.
func validateOperations(role string, operations []string) ([]string, error) {
	granted := make([]string, 0, len(operations))
	for _, operation := range operations {
		if !handler.Capability(operation).Valid() {
			return nil, fmt.Errorf("role %s: operation %q is not one of the %d operations Loam recognizes (%s): %w",
				role, operation, len(handler.AllCapabilities()), strings.Join(capabilityNames(), ", "), handler.ErrInvalidArgument)
		}
		if !slices.Contains(granted, operation) {
			granted = append(granted, operation)
		}
	}
	slices.Sort(granted)
	return granted, nil
}

// capabilityNames renders the fixed vocabulary as plain strings for the
// rejection message above, straight from handler.AllCapabilities.
func capabilityNames() []string {
	all := handler.AllCapabilities()
	names := make([]string, 0, len(all))
	for _, capability := range all {
		names = append(names, string(capability))
	}
	return names
}

// mapStoreErr maps the role store's sentinels onto this repo's handler
// sentinels, which internal/handler.ErrorMapper turns into Connect codes.
// Anything else is passed through untouched, so it reaches ErrorMapper's
// unmapped branch and is LOGGED before becoming CodeInternal -- a database
// failure must never be reported to an admin as "no such role".
func mapStoreErr(err error, name string) error {
	switch {
	case errors.Is(err, rolestore.ErrNotFound):
		return fmt.Errorf("role %s: %w", name, handler.ErrNotFound)
	case errors.Is(err, rolestore.ErrAlreadyExists):
		return fmt.Errorf("role %s already exists: %w", name, handler.ErrAlreadyExists)
	default:
		return err
	}
}

// roleToProto converts a store Role to its proto form. Operations arrive
// from the store already sorted (its own ORDER BY operation), so the wire
// order is deterministic without sorting again here.
func roleToProto(role rolestore.Role) *adminv1.Role {
	return &adminv1.Role{
		Name:         role.Name,
		Operations:   role.Operations,
		Instructions: role.Instructions,
		Builtin:      role.Builtin,
	}
}
