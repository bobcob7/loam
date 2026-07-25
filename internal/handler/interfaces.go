package handler

import "context"

// RoleStore resolves the capabilities granted to a named role, backed by
// the same role store loam-ofg.11's MetaService and loam-ofg.13's
// RoleService read (docs/web-spec.md -> RoleService). Built-in roles
// (author, reviewer) and any admin-defined role are both resolved through
// this single seam.
type RoleStore interface {
	// RoleCapabilities returns the operations granted to role, from the
	// fixed vocabulary in this file (CapabilityWorkStart, ...). An unknown
	// role is the store's concern to reject; CapabilityChecker only
	// forwards whatever error it returns.
	RoleCapabilities(ctx context.Context, role string) ([]string, error)
}

//go:generate go tool moq -out moq_test.go . RoleStore
