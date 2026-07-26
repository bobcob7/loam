// Package rolestore implements the read seam over roles and
// role_operations that loam.v1.MetaService (RoleCapabilities,
// RoleInstructions) and internal/handler's CapabilityChecker need
// (docs/persistence-spec.md "roles", "role_operations").
//
// This is deliberately NOT the full RoleService store: ListRoles,
// CreateRole, UpdateRole, and DeleteRole (docs/web-spec.md -> RoleService,
// the admin CRUD surface) have no bead of their own yet under loam-54o and
// are loam-ofg.13's job, not this package's. GetRole is the one read this
// bead's MetaService.GetInstructions and RepoService.GetRepo's capability
// gate actually need: resolve a trusted Loam-Agent-Role header value to
// its granted operations and instructions text. Adding the write methods
// here now would be inventing a seam ahead of a consumer that needs it,
// against this repo's usual practice (see loam-ai4's reasoning for the
// same call on error-sentinel exports).
package rolestore

import (
	"github.com/bobcob7/loam/internal/db/gen"
)

//go:generate go tool moq -out moq_test.go . querier

// querier is the minimal Postgres surface Store needs: sqlc's generated
// DBTX. Defined here at the consumer, per repo convention, so Store can be
// unit-tested against a moq mock instead of a live pool; *pgxpool.Pool
// satisfies it in production without modification.
type querier interface {
	gen.DBTX
}
