package handler

import (
	"context"
	"fmt"
	"slices"

	"github.com/bobcob7/loam/internal/httpauth"
)

// The fixed capability vocabulary (docs/web-spec.md -> RoleService), at
// the command-group level. instructions and whoami are always available
// and ungated, so they have no constant here.
const (
	CapabilityWorkStart         = "work.start"
	CapabilityWorkSet           = "work.set"
	CapabilityWorkRequestReview = "work.request_review"
	CapabilityWorkReply         = "work.reply"
	CapabilityWorkVerdict       = "work.verdict"
	CapabilityWorkRead          = "work.read"
	CapabilityGitClone          = "git.clone"
	CapabilityGitPush           = "git.push"
	CapabilityGraphQuery        = "graph.query"
	CapabilitySearch            = "search"
)

// CapabilityChecker enforces the capability vocabulary above against the
// caller resolved from context by internal/httpauth. Handler packages
// construct one (role store injected via NewCapabilityChecker) and call
// RequireCapability once at the top of each gated RPC with the specific
// capability string, rather than reinventing the check.
type CapabilityChecker struct {
	roles RoleStore
}

// NewCapabilityChecker builds a CapabilityChecker backed by roles, the
// same role store loam-ofg.11's MetaService reads.
func NewCapabilityChecker(roles RoleStore) *CapabilityChecker {
	return &CapabilityChecker{roles: roles}
}

// RequireCapability resolves the caller from ctx — an admin superuser or
// an agent identity, both set by internal/httpauth — and confirms they
// may perform capability. Admin basic-auth callers always bypass as
// superuser. An agent whose role lacks the capability, or a caller with no
// resolved identity at all (defence-in-depth: internal/httpauth.CLI itself
// now rejects every /loam.v1.* request lacking one, so this branch only
// matters if some future wrapper other than CLI ever reaches a handler
// without resolving an identity first), gets an error wrapping
// ErrPermissionDenied (-> connect.CodePermissionDenied via
// ErrorMapper.ToConnectErr).
func (c *CapabilityChecker) RequireCapability(ctx context.Context, capability string) error {
	if httpauth.IsAdmin(ctx) {
		return nil
	}
	identity, ok := httpauth.IdentityFromContext(ctx)
	if !ok {
		return fmt.Errorf("capability %s: no resolved caller: %w", capability, ErrPermissionDenied)
	}
	granted, err := c.roles.RoleCapabilities(ctx, identity.Role)
	if err != nil {
		return fmt.Errorf("resolving capabilities for role %s: %w", identity.Role, err)
	}
	if slices.Contains(granted, capability) {
		return nil
	}
	return fmt.Errorf("role %s lacks capability %s: %w", identity.Role, capability, ErrPermissionDenied)
}
