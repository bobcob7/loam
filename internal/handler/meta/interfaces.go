// Package meta implements loam.v1.MetaService: GetInstructions, the
// orientation RPC `loam instructions` calls (docs/cli-spec.md ->
// instructions). Unlike every other loam.v1 handler, GetInstructions is
// NEVER capability-gated (README -> Agent Identity and Roles): any
// identified caller -- admin or agent, regardless of role -- may call it,
// because an agent has to learn what it may do before it can be expected
// to respect a gate on anything else. This package therefore has no
// dependency on internal/handler.CapabilityChecker at all; it only reads
// the caller's identity (internal/httpauth) to resolve which commands and
// instructions to return, never to deny the call itself.
package meta

import (
	"context"

	"github.com/bobcob7/loam/internal/handler"
)

//go:generate go tool moq -out moq_test.go . RoleStore

// RoleStore resolves a role's granted capabilities and configured
// instructions text, defined here at the consumer per repo convention. It
// is the same underlying role store (internal/rolestore.Store) capability
// package's CapabilityChecker reads via its own, separately-defined
// RoleStore interface -- role_operations.operation is CHECK-constrained to
// the fixed capability vocabulary (0001_init.up.sql), so every value
// RoleCapabilities returns is already a valid handler.Capability.
type RoleStore interface {
	// RoleCapabilities returns the operations granted to role. An unknown
	// role returns an error wrapping internal/rolestore.ErrNotFound (as
	// internal/rolestore.Store.GetRole does); resolveCaller (meta.go)
	// specifically recognizes that and rewraps it as
	// handler.ErrPermissionDenied -- an unrecognized role is a denial, not
	// a not-found (loam-a8z) -- rather than letting it reach
	// ErrorMapper's unmapped-and-logged CodeInternal default. Any OTHER
	// error is forwarded unchanged.
	RoleCapabilities(ctx context.Context, role string) ([]handler.Capability, error)
	// RoleInstructions returns the instructions text configured for role
	// (roles.instructions, docs/persistence-spec.md "roles"). A built-in
	// role's instructions are no longer empty by default: migration
	// 0006_role_instructions_seed fills 'author' and 'reviewer' with
	// shipped policy text on a freshly migrated database (loam-0pj.17).
	// An admin can still replace that text (or a custom role's, which
	// ships with whatever CreateRole was given) at any time in the web
	// console -- see queries/roles.sql's UpdateRoleInstructions.
	RoleInstructions(ctx context.Context, role string) (string, error)
}
