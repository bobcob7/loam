package handler

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/reposstore"
)

// RoleStore resolves the capabilities granted to a named role, backed by
// the same role store loam-ofg.11's MetaService and loam-ofg.13's
// RoleService read (docs/web-spec.md -> RoleService). Built-in roles
// (author, reviewer) and any admin-defined role are both resolved through
// this single seam.
type RoleStore interface {
	// RoleCapabilities returns the operations granted to role, from the
	// fixed Capability vocabulary in capability.go (CapabilityWorkStart,
	// ...). An unknown role is the store's concern to reject: it returns an
	// error wrapping internal/rolestore.ErrNotFound (as
	// internal/rolestore.Store.GetRole does), which RequireCapability
	// specifically recognizes and rewraps as ErrPermissionDenied -- a
	// caller presenting an unrecognized role is a denial, not a not-found
	// (loam-a8z), and must not be able to distinguish existing role names
	// from non-existing ones by response code. Any OTHER error (a genuine
	// store failure) is forwarded unchanged, still headed for
	// ErrorMapper's unmapped-and-logged CodeInternal default.
	RoleCapabilities(ctx context.Context, role string) ([]Capability, error)
}

// ScopeStore is the repo-lookup surface ScopeResolver needs (see scope.go),
// defined here at the consumer per this package's own convention.
// *reposstore.Store satisfies it structurally in production; tests drive a
// moq mock.
type ScopeStore interface {
	// GetRepoByName resolves an enrolled repo's name to its full row,
	// including the id and indexed_branch ScopeResolver needs. Returns a
	// wrapped reposstore.ErrNotFound for a name that is not enrolled.
	GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error)
	// ListAllRepoNames returns every enrolled repo's name, unpaginated --
	// how ScopeResolver expands an empty QueryScope.repos into "all
	// enrolled repos".
	ListAllRepoNames(ctx context.Context) ([]string, error)
	// ListTargetBranches returns every branch enrolled as a target for
	// repoID, including each one's ingest provenance -- how
	// ScopeResolver.Ingested finds the row matching a repo's indexed
	// branch.
	ListTargetBranches(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error)
}

//go:generate go tool moq -out moq_test.go . RoleStore ScopeStore
