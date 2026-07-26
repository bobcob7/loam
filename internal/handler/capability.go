package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/bobcob7/loam/internal/httpauth"
)

// Capability names one operation in the fixed capability vocabulary
// (docs/web-spec.md -> RoleService), at the command-group level. Defining
// it as its own type stops an arbitrary string variable (a repo name, a
// role name) from being passed where a capability is expected, and names
// the vocabulary in godoc at every RequireCapability call site. It does
// NOT, by itself, make a misspelled literal a compile error: an untyped
// string constant is implicitly assignable to a defined string type, so
// RequireCapability(ctx, "work.reqest_review") still compiles. See Valid
// and RequireCapability for the runtime check that catches that case.
type Capability string

// The fixed capability vocabulary (docs/web-spec.md -> RoleService), at
// the command-group level. instructions and whoami are always available
// and ungated, so they have no constant here.
const (
	CapabilityWorkStart         Capability = "work.start"
	CapabilityWorkSet           Capability = "work.set"
	CapabilityWorkRequestReview Capability = "work.request_review"
	CapabilityWorkReply         Capability = "work.reply"
	CapabilityWorkVerdict       Capability = "work.verdict"
	CapabilityWorkRead          Capability = "work.read"
	CapabilityGitClone          Capability = "git.clone"
	CapabilityGitPush           Capability = "git.push"
	CapabilityGraphQuery        Capability = "graph.query"
	CapabilitySearch            Capability = "search"
)

// Valid reports whether c is one of the ten capabilities in the fixed
// vocabulary above. RequireCapability rejects anything else with
// errUnknownCapability instead of silently folding it into a permission
// denial.
func (c Capability) Valid() bool {
	switch c {
	case CapabilityWorkStart, CapabilityWorkSet, CapabilityWorkRequestReview, CapabilityWorkReply,
		CapabilityWorkVerdict, CapabilityWorkRead, CapabilityGitClone, CapabilityGitPush,
		CapabilityGraphQuery, CapabilitySearch:
		return true
	default:
		return false
	}
}

// errUnknownCapability marks a capability argument outside the fixed
// vocabulary. It deliberately does NOT wrap ErrPermissionDenied: today a
// mistyped capability literal is otherwise indistinguishable, at the
// Connect status code, from a genuine authorization failure (both would
// map to CodePermissionDenied), and a handler test that merely asserts
// "denied" would not catch it. Left unmapped, ErrorMapper.ToConnectErr
// collapses this to CodeInternal and logs it — a loud, visible bug instead
// of a silent misdenial. Unexported: only RequireCapability constructs it,
// so it never needs to cross a package boundary.
var errUnknownCapability = errors.New("handler: unknown capability")

// CapabilityChecker enforces the capability vocabulary above against the
// caller resolved from context by internal/httpauth. Handler packages
// construct one (role store injected via NewCapabilityChecker) and call
// RequireCapability once at the top of each gated RPC with the specific
// capability, rather than reinventing the check.
type CapabilityChecker struct {
	roles RoleStore
}

// NewCapabilityChecker builds a CapabilityChecker backed by roles, the
// same role store loam-ofg.11's MetaService reads.
func NewCapabilityChecker(roles RoleStore) *CapabilityChecker {
	return &CapabilityChecker{roles: roles}
}

// RequireCapability resolves the caller from ctx — an admin superuser or
// an agent identity, both set by internal/httpauth — and confirms they may
// perform capability. capability must be one of the ten Capability
// constants above; RequireCapability validates this first and returns an
// error wrapping errUnknownCapability (-> connect.CodeInternal, logged via
// ErrorMapper.ToConnectErr) for anything outside the vocabulary, rather
// than risking it silently pass as, or fail as, a permission check. Admin
// basic-auth callers always bypass as superuser. An agent whose role lacks
// the capability, or a caller with no resolved identity at all
// (defence-in-depth: internal/httpauth.CLI itself now rejects every
// /loam.v1.* request lacking one, so this branch only matters if some
// future wrapper other than CLI ever reaches a handler without resolving
// an identity first), gets an error wrapping ErrPermissionDenied
// (-> connect.CodePermissionDenied via ErrorMapper.ToConnectErr).
func (c *CapabilityChecker) RequireCapability(ctx context.Context, capability Capability) error {
	if !capability.Valid() {
		return fmt.Errorf("capability %s: %w", capability, errUnknownCapability)
	}
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
