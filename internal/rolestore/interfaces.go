// Package rolestore implements the seam over roles and role_operations
// that loam.v1.MetaService (RoleCapabilities, RoleInstructions),
// internal/handler's CapabilityChecker, and loam.admin.v1.RoleService
// (internal/handler/role) need (docs/persistence-spec.md "roles",
// "role_operations").
//
// It began read-only -- GetRole alone, for resolving a trusted
// Loam-Agent-Role header value to its granted operations and instructions
// text -- and its own doc comment recorded that the admin CRUD surface
// (ListRoles, CreateRole, UpdateRole, DeleteRole; docs/web-spec.md ->
// RoleService) belonged to loam-ofg.13 rather than being invented ahead of
// a consumer. loam-ofg.13 is that consumer, so those four now live here,
// alongside the read they share a table with.
//
// The write methods own their own transactions (Store.inTx): a role and
// its operations are one authorization record, and this package -- not its
// caller -- is where "created with all of its operations, or not created"
// is enforced.
//
// What this package still does NOT do is validate an operation string
// against the fixed capability vocabulary. That vocabulary is
// internal/handler's (Capability, AllCapabilities); importing an
// RPC-boundary package from a store package would invert the layering the
// "interfaces at the consumer" convention establishes, so validation stays
// at the handler and role_operations_operation_check (0001_init.up.sql) is
// the database's own backstop underneath it. See Role.Operations and
// RoleParams.Operations.
package rolestore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . querier

// querier is the minimal Postgres surface Store needs: sqlc's generated
// DBTX, plus Begin for the write methods' transactions. Defined here at
// the consumer, per repo convention, so Store can be unit-tested against a
// moq mock instead of a live pool; *pgxpool.Pool satisfies it in
// production without modification (its Begin returns pgx.Tx).
//
// Begin is on THIS interface rather than a second one because every
// production implementation is the same pool: splitting it would buy a
// narrower seam nothing narrower actually implements, at the cost of two
// constructor arguments that must always be the same value.
type querier interface {
	gen.DBTX
	// Begin opens a transaction the caller must commit or roll back.
	Begin(ctx context.Context) (pgx.Tx, error)
}
