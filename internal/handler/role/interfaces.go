// Package role implements loam.admin.v1.RoleService (docs/web-spec.md ->
// RoleService): the admin's configuration of what each agent role may do
// and the instruction text loam.v1.MetaService.GetInstructions returns for
// it.
//
// # This package is the writer of the authorization policy
//
// Every gated RPC in the system asks internal/handler.CapabilityChecker
// whether the caller's role carries a capability, and CapabilityChecker
// reads the roles/role_operations rows THIS package writes. Nothing else
// in the tree writes them at runtime (migration 0001_init seeds the two
// built-ins and is the only other writer, ever). That makes a bug here
// different in kind from a bug in a sibling admin handler: it does not
// corrupt one resource, it changes the answer every other gate gives.
// Two consequences run through the whole package:
//
//  1. Every RPC re-asserts admin status (see requireAdmin), reads
//     included.
//  2. An operation outside the fixed vocabulary is REJECTED, never
//     stored -- and the vocabulary is read from internal/handler, not
//     restated here (see validateOperations).
package role

import (
	"context"

	"github.com/bobcob7/loam/internal/rolestore"
)

//go:generate go tool moq -out moq_test.go . roleStore

// roleStore is the internal/rolestore.Store surface this package's Handler
// needs, defined here at the consumer per repo convention. *rolestore.Store
// satisfies it structurally.
//
// It is the store's full surface rather than a narrowed one because
// RoleService is the CRUD surface over exactly that table: there is no
// method on rolestore.Store this package deliberately withholds from
// itself, unlike internal/handler/credential, which omits the one
// decrypting method so a token readback is unreachable. The store's own
// package doc records the layering that keeps validation out of it: it
// takes operation strings, and rejecting the ones outside the fixed
// vocabulary is this package's job, not its.
type roleStore interface {
	// ListRoles returns every role, built-in and admin-defined, ordered by
	// name, each with its granted operations attached.
	ListRoles(ctx context.Context) ([]rolestore.Role, error)
	// GetRole resolves a role by name, wrapping rolestore.ErrNotFound if
	// no such role exists.
	GetRole(ctx context.Context, name string) (rolestore.Role, error)
	// CreateRole creates an admin-defined (never built-in) role and its
	// granted operations in one transaction, wrapping
	// rolestore.ErrAlreadyExists if the name is taken.
	CreateRole(ctx context.Context, params rolestore.RoleParams) (rolestore.Role, error)
	// UpdateRole rewrites a role's instructions and REPLACES its granted
	// operations, in one transaction, wrapping rolestore.ErrNotFound if no
	// such role exists. It never writes the builtin flag.
	UpdateRole(ctx context.Context, params rolestore.RoleParams) (rolestore.Role, error)
	// DeleteRole removes a role by name, cascading to its operations,
	// wrapping rolestore.ErrNotFound if no such (non-built-in) role
	// exists.
	DeleteRole(ctx context.Context, name string) error
}
