package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/rolestore"
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

// allCapabilities is the fixed vocabulary as a list, in the declaration
// order above. It is the SINGLE Go-side source of truth for "what the
// vocabulary contains": Valid, AllCapabilities, internal/handler/meta's
// admin command filter, and internal/handler/role's CreateRole/UpdateRole
// validation all derive from this one slice rather than restating the ten
// members. A second hand-written copy is exactly the failure mode this
// repo has been bitten by before (a prose inventory that went stale three
// times), and here it would be worse than stale prose: a copy that lost a
// member would silently start rejecting a legitimate operation an admin
// tried to grant.
//
// Unexported and never mutated; AllCapabilities hands out a clone so no
// caller can reorder or truncate the vocabulary for everyone else.
var allCapabilities = []Capability{
	CapabilityWorkStart, CapabilityWorkSet, CapabilityWorkRequestReview, CapabilityWorkReply,
	CapabilityWorkVerdict, CapabilityWorkRead, CapabilityGitClone, CapabilityGitPush,
	CapabilityGraphQuery, CapabilitySearch,
}

// AllCapabilities returns the fixed capability vocabulary in full, in the
// order the constants are declared above. Callers that must enumerate the
// vocabulary — internal/handler/meta (every command an admin superuser may
// run) and internal/handler/role (the set an admin may grant, and the list
// quoted back when a request names something outside it) — use this rather
// than rebuilding the list, so there is one place a future eleventh
// operation is added.
//
// It returns a fresh slice per call: the vocabulary is fixed, and a caller
// that sorted or appended to a shared backing array would change what
// every other caller sees.
func AllCapabilities() []Capability {
	return slices.Clone(allCapabilities)
}

// Valid reports whether c is one of the ten capabilities in the fixed
// vocabulary above. RequireCapability rejects anything else with
// errUnknownCapability instead of silently folding it into a permission
// denial; internal/handler/role rejects it with ErrInvalidArgument rather
// than storing it.
func (c Capability) Valid() bool {
	return slices.Contains(allCapabilities, c)
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
// (-> connect.CodePermissionDenied via ErrorMapper.ToConnectErr). So does an
// agent presenting a role the store does not recognize at all (loam-a8z):
// rolestore.ErrNotFound is rewrapped as ErrPermissionDenied rather than
// left to ErrorMapper's unmapped-and-logged CodeInternal default, which
// otherwise told the caller nothing and the operator only "internal
// error", for what is genuinely just a bad Loam-Agent-Role header.
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
		if errors.Is(err, rolestore.ErrNotFound) {
			return fmt.Errorf("resolving capabilities for role %s: %w: %w", identity.Role, err, ErrPermissionDenied)
		}
		return fmt.Errorf("resolving capabilities for role %s: %w", identity.Role, err)
	}
	if slices.Contains(granted, capability) {
		return nil
	}
	return fmt.Errorf("role %s lacks capability %s: %w", identity.Role, capability, ErrPermissionDenied)
}
