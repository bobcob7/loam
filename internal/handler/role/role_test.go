package role

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/rolestore"
)

// adminCtx is the context every RPC in this package requires: the flag
// httpauth.Auth.AdminOnly sets on a request that passed admin basic auth.
func adminCtx(t *testing.T) context.Context {
	t.Helper()
	return httpauth.WithAdmin(t.Context())
}

// agentCtx is a caller that got through the CLI wrapper with a trusted
// agent identity but is NOT the admin -- the caller requireAdmin exists to
// refuse. It is a stronger negative than a bare t.Context(): it proves the
// gate checks for ADMIN, not merely for "some resolved caller".
func agentCtx(t *testing.T) context.Context {
	t.Helper()
	return httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "agent-1", ID: "id-1", Role: "author"})
}

// testDeps bundles the store mock, pre-configured so every method succeeds
// with a plausible answer. A test that overrides nothing exercises the
// happy path, so any failure it sees comes from the one thing it
// deliberately changed -- never from a nil-func panic on a collaborator it
// does not care about. That property is what makes mutation testing here
// meaningful: breaking a load-bearing line must turn a test red on an
// ASSERTION, which it cannot do if an unconfigured mock panics first.
type testDeps struct {
	store *roleStoreMock
	buf   bytes.Buffer
}

func customRole() rolestore.Role {
	return rolestore.Role{
		Name:         "release-captain",
		Instructions: "ship it",
		Builtin:      false,
		Operations:   []string{"git.clone", "work.read"},
	}
}

func builtinAuthor() rolestore.Role {
	return rolestore.Role{
		Name:       "author",
		Builtin:    true,
		Operations: []string{"git.clone", "git.push", "work.start"},
	}
}

func newTestDeps() *testDeps {
	d := &testDeps{}
	d.store = &roleStoreMock{
		ListRolesFunc: func(context.Context) ([]rolestore.Role, error) {
			return []rolestore.Role{builtinAuthor(), customRole()}, nil
		},
		GetRoleFunc: func(_ context.Context, name string) (rolestore.Role, error) {
			if name == "author" {
				return builtinAuthor(), nil
			}
			role := customRole()
			role.Name = name
			return role, nil
		},
		CreateRoleFunc: func(_ context.Context, params rolestore.RoleParams) (rolestore.Role, error) {
			return rolestore.Role{Name: params.Name, Instructions: params.Instructions, Operations: params.Operations}, nil
		},
		UpdateRoleFunc: func(_ context.Context, params rolestore.RoleParams) (rolestore.Role, error) {
			return rolestore.Role{Name: params.Name, Instructions: params.Instructions, Operations: params.Operations}, nil
		},
		DeleteRoleFunc: func(context.Context, string) error { return nil },
	}
	return d
}

func (d *testDeps) handler() *Handler {
	logger := slog.New(slog.NewJSONHandler(&d.buf, nil))
	return New(d.store, handler.NewErrorMapper(logger), logger)
}

// discardLogger is for the rare test that does not inspect log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func createReq(role *adminv1.Role) *connect.Request[adminv1.CreateRoleRequest] {
	return connect.NewRequest(&adminv1.CreateRoleRequest{Role: role})
}

func updateReq(role *adminv1.Role) *connect.Request[adminv1.UpdateRoleRequest] {
	return connect.NewRequest(&adminv1.UpdateRoleRequest{Role: role})
}

// connectCode extracts the Connect status code from err, failing the test
// if err is not a *connect.Error at all.
func connectCode(t *testing.T, err error) connect.Code {
	t.Helper()
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	return connectErr.Code()
}

// ---------------------------------------------------------------------
// The admin gate, on every RPC
// ---------------------------------------------------------------------

// TestEveryRPCRequiresTheAdmin proves the per-RPC gate covers all five
// RPCs, reads included, and that it rejects a caller who IS authenticated
// as an agent -- so it is testing for admin, not merely for a resolved
// identity. It also asserts the store is never reached: a denied caller
// must not cause a read of, let alone a write to, the authorization
// policy.
func TestEveryRPCRequiresTheAdmin(t *testing.T) {
	t.Parallel()
	role := &adminv1.Role{Name: "release-captain", Operations: []string{"search"}}
	calls := map[string]func(*Handler, context.Context) error{
		"ListRoles": func(h *Handler, ctx context.Context) error {
			_, err := h.ListRoles(ctx, connect.NewRequest(&adminv1.ListRolesRequest{}))
			return err
		},
		"GetRole": func(h *Handler, ctx context.Context) error {
			_, err := h.GetRole(ctx, connect.NewRequest(&adminv1.GetRoleRequest{Name: "author"}))
			return err
		},
		"CreateRole": func(h *Handler, ctx context.Context) error {
			_, err := h.CreateRole(ctx, createReq(role))
			return err
		},
		"UpdateRole": func(h *Handler, ctx context.Context) error {
			_, err := h.UpdateRole(ctx, updateReq(role))
			return err
		},
		"DeleteRole": func(h *Handler, ctx context.Context) error {
			_, err := h.DeleteRole(ctx, connect.NewRequest(&adminv1.DeleteRoleRequest{Name: "release-captain"}))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			err := call(d.handler(), agentCtx(t))
			require.Error(t, err, "a non-admin caller must be refused")
			assert.Equal(t, connect.CodePermissionDenied, connectCode(t, err))
			assert.Empty(t, d.store.ListRolesCalls(), "a denied caller must not reach the store")
			assert.Empty(t, d.store.GetRoleCalls(), "a denied caller must not reach the store")
			assert.Empty(t, d.store.CreateRoleCalls(), "a denied caller must not reach the store")
			assert.Empty(t, d.store.UpdateRoleCalls(), "a denied caller must not reach the store")
			assert.Empty(t, d.store.DeleteRoleCalls(), "a denied caller must not reach the store")
		})
	}
}

// ---------------------------------------------------------------------
// ListRoles / GetRole
// ---------------------------------------------------------------------

func TestListRoles_ReturnsEveryRoleWithItsOperationsAndBuiltinFlag(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().ListRoles(adminCtx(t), connect.NewRequest(&adminv1.ListRolesRequest{}))
	require.NoError(t, err)
	require.Len(t, d.store.ListRolesCalls(), 1)
	require.Len(t, resp.Msg.GetRoles(), 2)
	assert.Equal(t, "author", resp.Msg.GetRoles()[0].GetName())
	assert.True(t, resp.Msg.GetRoles()[0].GetBuiltin(), "the author role must report as built-in")
	assert.Equal(t, []string{"git.clone", "git.push", "work.start"}, resp.Msg.GetRoles()[0].GetOperations())
	assert.Equal(t, "release-captain", resp.Msg.GetRoles()[1].GetName())
	assert.False(t, resp.Msg.GetRoles()[1].GetBuiltin(), "an admin-defined role must not report as built-in")
	assert.Equal(t, "ship it", resp.Msg.GetRoles()[1].GetInstructions())
}

func TestListRoles_StoreFailure_IsInternalNotAnEmptyList(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.ListRolesFunc = func(context.Context) ([]rolestore.Role, error) {
		return nil, errors.New("connection refused")
	}
	_, err := d.handler().ListRoles(adminCtx(t), connect.NewRequest(&adminv1.ListRolesRequest{}))
	require.Error(t, err, "a database failure must not be reported as 'there are no roles'")
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
	assert.Contains(t, d.buf.String(), "unmapped handler error", "an unclassified error must be logged before it is collapsed")
}

func TestGetRole_ReturnsTheNamedRole(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().GetRole(adminCtx(t), connect.NewRequest(&adminv1.GetRoleRequest{Name: "author"}))
	require.NoError(t, err)
	require.Len(t, d.store.GetRoleCalls(), 1)
	assert.Equal(t, "author", d.store.GetRoleCalls()[0].Name)
	assert.Equal(t, "author", resp.Msg.GetRole().GetName())
	assert.True(t, resp.Msg.GetRole().GetBuiltin())
}

func TestGetRole_UnknownName_IsNotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetRoleFunc = func(context.Context, string) (rolestore.Role, error) {
		return rolestore.Role{}, rolestore.ErrNotFound
	}
	_, err := d.handler().GetRole(adminCtx(t), connect.NewRequest(&adminv1.GetRoleRequest{Name: "nope"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
}

func TestGetRole_StoreFailure_IsNotReportedAsNotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetRoleFunc = func(context.Context, string) (rolestore.Role, error) {
		return rolestore.Role{}, errors.New("connection refused")
	}
	_, err := d.handler().GetRole(adminCtx(t), connect.NewRequest(&adminv1.GetRoleRequest{Name: "author"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err), "a database failure must not be reported to an admin as 'no such role'")
}

func TestGetRole_EmptyName_IsInvalidArgument(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().GetRole(adminCtx(t), connect.NewRequest(&adminv1.GetRoleRequest{Name: "   "}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, d.store.GetRoleCalls(), "a request rejected as malformed must not reach the store")
}

// ---------------------------------------------------------------------
// The fixed capability vocabulary -- the crux of this service
// ---------------------------------------------------------------------

// TestCreateRole_AcceptsEveryOperationInTheFixedVocabulary is the positive
// control for the rejection test below: it grants the vocabulary IN FULL,
// read from its single source of truth, so a validator that had drifted
// into rejecting a legitimate operation fails here rather than passing
// vacuously. It is deliberately not a hand-written list of ten strings --
// that copy is the exact drift this package refuses to create.
func TestCreateRole_AcceptsEveryOperationInTheFixedVocabulary(t *testing.T) {
	t.Parallel()
	all := handler.AllCapabilities()
	requested := make([]string, 0, len(all))
	for _, capability := range all {
		requested = append(requested, string(capability))
	}
	d := newTestDeps()
	resp, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{Name: "everything", Operations: requested}))
	require.NoError(t, err, "every operation in the fixed vocabulary must be grantable")
	require.Len(t, d.store.CreateRoleCalls(), 1)
	assert.ElementsMatch(t, requested, d.store.CreateRoleCalls()[0].Params.Operations)
	assert.Len(t, resp.Msg.GetRole().GetOperations(), len(all))
}

// TestCreateRole_UnknownOperation_IsRejectedAndNotStored is the core of
// "fixed vocabulary": an operation outside the closed set must be refused,
// not written. The near-miss spelling matters -- "work.reqest_review" is
// the typo the Capability type itself cannot catch (an untyped constant
// converts silently), which is why the runtime check exists.
func TestCreateRole_UnknownOperation_IsRejectedAndNotStored(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"work.reqest_review", "admin.everything", "", "GIT.CLONE", "git.clone "} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			_, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{
				Name:       "sneaky",
				Operations: []string{"search", operation},
			}))
			require.Error(t, err, "an operation outside the fixed vocabulary must be refused")
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.Empty(t, d.store.CreateRoleCalls(), "an unknown operation must never reach the store")
		})
	}
}

func TestUpdateRole_UnknownOperation_IsRejectedAndNotStored(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().UpdateRole(adminCtx(t), updateReq(&adminv1.Role{
		Name:       "release-captain",
		Operations: []string{"git.push", "work.approve"},
	}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, d.store.UpdateRoleCalls(), "an unknown operation must never reach the store")
}

// TestUnknownOperationErrorNamesTheWholeVocabulary proves the rejection
// tells the admin what IS accepted, and that the list comes from the
// single source of truth rather than a copy: every capability
// handler.AllCapabilities reports must appear in the message.
func TestUnknownOperationErrorNamesTheWholeVocabulary(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{Name: "x", Operations: []string{"nope"}}))
	require.Error(t, err)
	for _, capability := range handler.AllCapabilities() {
		assert.Contains(t, err.Error(), string(capability), "the rejection must name every accepted operation")
	}
}

// TestCreateRole_DeduplicatesAndSortsOperations proves a repeated grant is
// collapsed rather than sent to the store, where PRIMARY KEY (role_id,
// operation) would fail the whole transaction.
func TestCreateRole_DeduplicatesAndSortsOperations(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{
		Name:       "release-captain",
		Operations: []string{"search", "git.clone", "search", "git.clone"},
	}))
	require.NoError(t, err)
	require.Len(t, d.store.CreateRoleCalls(), 1)
	assert.Equal(t, []string{"git.clone", "search"}, d.store.CreateRoleCalls()[0].Params.Operations)
}

// ---------------------------------------------------------------------
// CreateRole
// ---------------------------------------------------------------------

func TestCreateRole_PassesNameOperationsAndInstructionsToTheStore(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{
		Name:         "  release-captain  ",
		Operations:   []string{"git.push", "git.clone"},
		Instructions: "cut releases",
	}))
	require.NoError(t, err)
	require.Len(t, d.store.CreateRoleCalls(), 1)
	params := d.store.CreateRoleCalls()[0].Params
	assert.Equal(t, "release-captain", params.Name, "the name must be trimmed before it is stored")
	assert.Equal(t, []string{"git.clone", "git.push"}, params.Operations)
	assert.Equal(t, "cut releases", params.Instructions)
	assert.Equal(t, "release-captain", resp.Msg.GetRole().GetName())
	assert.Equal(t, "cut releases", resp.Msg.GetRole().GetInstructions())
}

// TestCreateRole_BuiltinRequested_IsRejected proves an admin cannot mint a
// role that reports as built-in. The flag is the only thing DeleteRole
// consults, so a forged one would hand out an undeletable role.
//
// It also pins the SHAPE of the refusal's wording, the way this file
// already pins it for the accepted-operation list and the built-in delete
// refusal below. The reason is specific rather than general: this message
// used to enumerate the built-ins ("only the author and reviewer roles
// shipped with the server are built-in") and that enumeration became a
// FALSE FACTUAL CLAIM, told to an admin about their own deployment, the
// moment migration 0009 seeded a third. Nothing caught it -- reverting the
// message to that wording left this package's tests green (loam-hi5o.31
// round 2 verified exactly that) -- because no assertion looked at the
// prose at all.
//
// The needle is the single word "migration", deliberately loose. It was
// "seeded by migration" for one round, and loam-hi5o.31's round-3 review
// reworded the message five ways to find that three CORRECT rewordings
// failed it -- "only migrations mark a role built-in", "the builtin flag
// is written by migrations alone", "only roles created by a migration are
// built-in". A pin that rejects every reasonable rephrasing of the thing
// it is protecting gets loosened or deleted by whoever hits it, taking the
// protection with it; that is the same dynamic as an over-eager needle in
// a match list, and it applies to assertions on prose too. One word still
// fails the enumeration, which named no mechanism at all, and matches the
// granularity of the "built in" pin further down this file.
func TestCreateRole_BuiltinRequested_IsRejected(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{
		Name:       "pseudo-builtin",
		Operations: []string{"search"},
		Builtin:    true,
	}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, d.store.CreateRoleCalls(), "a forged builtin request must not reach the store")
	assert.Contains(t, err.Error(), "migration",
		"the refusal must not name a fixed set of roles -- migrations decide which are built-in, and 0009 already added a third")
}

func TestCreateRole_DuplicateName_IsAlreadyExists(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.CreateRoleFunc = func(context.Context, rolestore.RoleParams) (rolestore.Role, error) {
		return rolestore.Role{}, rolestore.ErrAlreadyExists
	}
	_, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{Name: "author", Operations: []string{"search"}}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeAlreadyExists, connectCode(t, err))
}

func TestCreateRole_NoRoleMessage_IsInvalidArgument(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().CreateRole(adminCtx(t), createReq(nil))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
	assert.Empty(t, d.store.CreateRoleCalls())
}

// TestCreateRole_UnpresentableName_IsRejected proves a role name that
// could never travel in a Loam-Agent-Role header is refused at creation,
// the only point at which it is still fixable.
func TestCreateRole_UnpresentableName_IsRejected(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"space":        "release captain",
		"newline":      "release\ncaptain",
		"non-ascii":    "réviseur",
		"colon":        "role:admin",
		"far-too-long": strings.Repeat("a", maxRoleNameLen+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			_, err := d.handler().CreateRole(adminCtx(t), createReq(&adminv1.Role{Name: raw, Operations: []string{"search"}}))
			require.Error(t, err)
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.Empty(t, d.store.CreateRoleCalls())
		})
	}
}

// ---------------------------------------------------------------------
// UpdateRole
// ---------------------------------------------------------------------

// TestUpdateRole_ReplacesTheGrantedOperations is features/roles.feature's
// "Updating a role changes what its agents may do", at this layer: the
// operations the request names are the operations the store is told to
// hold, with the previous set neither merged nor preserved.
func TestUpdateRole_ReplacesTheGrantedOperations(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	resp, err := d.handler().UpdateRole(adminCtx(t), updateReq(&adminv1.Role{
		Name:         "release-captain",
		Operations:   []string{"git.clone", "git.push"},
		Instructions: "now you may push",
	}))
	require.NoError(t, err)
	require.Len(t, d.store.UpdateRoleCalls(), 1)
	params := d.store.UpdateRoleCalls()[0].Params
	assert.Equal(t, "release-captain", params.Name)
	assert.Equal(t, []string{"git.clone", "git.push"}, params.Operations)
	assert.Equal(t, "now you may push", params.Instructions)
	assert.Equal(t, []string{"git.clone", "git.push"}, resp.Msg.GetRole().GetOperations())
}

// TestUpdateRole_RevokingIsExpressibleAsAnEmptyOperationSet proves the
// replace semantics reach the degenerate case: a role can be stripped to
// no operations at all, which is what makes revocation possible through a
// surface that only ever sends whole Roles.
func TestUpdateRole_RevokingIsExpressibleAsAnEmptyOperationSet(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().UpdateRole(adminCtx(t), updateReq(&adminv1.Role{Name: "release-captain"}))
	require.NoError(t, err)
	require.Len(t, d.store.UpdateRoleCalls(), 1)
	assert.Empty(t, d.store.UpdateRoleCalls()[0].Params.Operations)
}

// TestUpdateRole_BuiltinRole_IsAllowed pins the judgement call: only
// DELETION is refused for a built-in. features/roles.feature's "A role's
// instructions reach its agents" configures instructions on the reviewer,
// which is a built-in seeded with none -- so refusing this would make that
// scenario unimplementable.
func TestUpdateRole_BuiltinRole_IsAllowed(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.UpdateRoleFunc = func(_ context.Context, params rolestore.RoleParams) (rolestore.Role, error) {
		return rolestore.Role{Name: params.Name, Instructions: params.Instructions, Operations: params.Operations, Builtin: true}, nil
	}
	resp, err := d.handler().UpdateRole(adminCtx(t), updateReq(&adminv1.Role{
		Name:         "reviewer",
		Operations:   []string{"work.read", "work.verdict"},
		Instructions: "review carefully",
		Builtin:      true,
	}))
	require.NoError(t, err, "a built-in role must remain updatable -- only deletion is refused")
	require.Len(t, d.store.UpdateRoleCalls(), 1)
	assert.Equal(t, "reviewer", d.store.UpdateRoleCalls()[0].Params.Name)
	assert.True(t, resp.Msg.GetRole().GetBuiltin(), "the response reports the stored truth, which this RPC never writes")
}

func TestUpdateRole_UnknownRole_IsNotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.UpdateRoleFunc = func(context.Context, rolestore.RoleParams) (rolestore.Role, error) {
		return rolestore.Role{}, rolestore.ErrNotFound
	}
	_, err := d.handler().UpdateRole(adminCtx(t), updateReq(&adminv1.Role{Name: "ghost", Operations: []string{"search"}}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
}

// ---------------------------------------------------------------------
// DeleteRole
// ---------------------------------------------------------------------

// TestDeleteRole_BuiltinRole_IsRefused is features/roles.feature's
// "Built-in roles cannot be deleted" at this layer. FailedPrecondition,
// not InvalidArgument: the request is well formed and the role exists --
// its state is what forbids the operation.
func TestDeleteRole_BuiltinRole_IsRefused(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().DeleteRole(adminCtx(t), connect.NewRequest(&adminv1.DeleteRoleRequest{Name: "author"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connectCode(t, err))
	assert.Empty(t, d.store.DeleteRoleCalls(), "a built-in role must never reach the store's delete")
	assert.Contains(t, err.Error(), "built in", "the admin must be told WHY, not just refused")
}

func TestDeleteRole_CustomRole_IsDeleted(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	_, err := d.handler().DeleteRole(adminCtx(t), connect.NewRequest(&adminv1.DeleteRoleRequest{Name: "release-captain"}))
	require.NoError(t, err)
	require.Len(t, d.store.DeleteRoleCalls(), 1)
	assert.Equal(t, "release-captain", d.store.DeleteRoleCalls()[0].Name)
}

// TestDeleteRole_UnknownRole_IsNotFoundNotFailedPrecondition proves the
// read-before-delete tells "no such role" apart from "that role is built
// in" -- the whole reason DeleteRole reads first instead of leaning on the
// store's own `AND NOT builtin` predicate.
func TestDeleteRole_UnknownRole_IsNotFoundNotFailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.GetRoleFunc = func(context.Context, string) (rolestore.Role, error) {
		return rolestore.Role{}, rolestore.ErrNotFound
	}
	_, err := d.handler().DeleteRole(adminCtx(t), connect.NewRequest(&adminv1.DeleteRoleRequest{Name: "ghost"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
	assert.Empty(t, d.store.DeleteRoleCalls())
}

// TestDeleteRole_DeletedConcurrently_IsNotFound covers the window between
// the builtin read and the delete: another admin removing the role first
// must surface as not-found, not as a success.
func TestDeleteRole_DeletedConcurrently_IsNotFound(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.DeleteRoleFunc = func(context.Context, string) error { return rolestore.ErrNotFound }
	_, err := d.handler().DeleteRole(adminCtx(t), connect.NewRequest(&adminv1.DeleteRoleRequest{Name: "release-captain"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeNotFound, connectCode(t, err))
}

func TestDeleteRole_StoreFailure_IsInternal(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.store.DeleteRoleFunc = func(context.Context, string) error { return errors.New("connection refused") }
	_, err := d.handler().DeleteRole(adminCtx(t), connect.NewRequest(&adminv1.DeleteRoleRequest{Name: "release-captain"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInternal, connectCode(t, err))
}

// ---------------------------------------------------------------------
// Conversion
// ---------------------------------------------------------------------

// TestRoleToProto_CarriesEveryFieldOfTheProtoRole guards the conversion
// against a dropped field: Role has exactly four fields, and a handler
// that forgot one would return a role whose operations or builtin flag
// silently read as empty.
func TestRoleToProto_CarriesEveryFieldOfTheProtoRole(t *testing.T) {
	t.Parallel()
	out := roleToProto(rolestore.Role{
		Name:         "release-captain",
		Operations:   []string{"git.clone"},
		Instructions: "ship it",
		Builtin:      true,
	})
	assert.Equal(t, "release-captain", out.GetName())
	assert.Equal(t, []string{"git.clone"}, out.GetOperations())
	assert.Equal(t, "ship it", out.GetInstructions())
	assert.True(t, out.GetBuiltin())
}

// TestNewHandlerSatisfiesTheGeneratedInterface is the wiring assertion: a
// *Handler built by New is what cmd/server registers, so it must satisfy
// the generated service interface at runtime as well as at compile time.
func TestNewHandlerSatisfiesTheGeneratedInterface(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	assert.NotNil(t, New(&roleStoreMock{}, handler.NewErrorMapper(logger), logger))
}
