package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bobcob7/loam/internal/httpauth"
)

// The two smart-HTTP service names docs/git-spec.md -> "Endpoint &
// Protocol" names as the only requests the git transport makes: the
// info/refs discovery request's ?service= query value, and the trailing
// path segment of the two POST endpoints.
const (
	gitServiceUploadPack  = "git-upload-pack"
	gitServiceReceivePack = "git-receive-pack"
)

// GitRoleGate is the capability/role layer docs/git-spec.md -> "Operations
// & Role Gates" requires on top of the identity internal/httpauth.
// GitIdentity already resolved into the request context. It is a plain
// http.Handler wrapper (not a Connect interceptor), constructed once and
// composed around loam-ofg.16's real /git/* handler the same way
// internal/httpauth.Auth's methods wrap a path group:
//
//	router.RegisterGit(prefix, gate.Middleware(gitHandler))
//
// GitRoleGate holds no request state; a process constructs exactly one
// from NewGitRoleGate in its composition root.
type GitRoleGate struct {
	checker *CapabilityChecker
	logger  *slog.Logger
}

// NewGitRoleGate builds a GitRoleGate backed by checker (the same
// CapabilityChecker every loam.v1/loam.admin.v1 handler already uses) and
// logger, used only to record a role-store failure before it is collapsed
// to a generic 500 -- an infrastructure failure must never disappear
// silently, but it must also never be reported to the git client as if it
// were a policy decision (see Middleware).
func NewGitRoleGate(checker *CapabilityChecker, logger *slog.Logger) *GitRoleGate {
	return &GitRoleGate{checker: checker, logger: logger}
}

// Middleware wraps next with the capability check. It determines the
// capability a request requires from the request's method, path suffix,
// and (for info/refs) its ?service= query parameter alone -- docs/git-
// spec.md's "Operations & Role Gates" table -- and, on denial, writes the
// HTTP response itself and returns without ever calling next. This is what
// makes the gate fail-fast: the check runs, and the response is written,
// strictly before next (loam-ofg.16's handler, which forks the actual git
// process) is invoked at all, so an unauthorized probe never causes a
// git-upload-pack/git-receive-pack process to spawn.
//
// A request shape outside the three the git smart-HTTP protocol defines
// (GET .../info/refs?service=git-upload-pack|git-receive-pack, POST
// .../git-upload-pack, POST .../git-receive-pack) is denied closed rather
// than guessed at or passed through -- docs/git-spec.md -> "Endpoint &
// Protocol" states the dumb HTTP protocol is not served, so nothing else
// is a legitimate request under this prefix.
//
// Middleware reuses CapabilityChecker.RequireCapability as-is, including
// its admin-superuser bypass: that bypass can never actually fire here
// because internal/httpauth.GitIdentity unconditionally clears any
// inherited admin marker before this wrapper ever sees the request (see
// GitIdentity's own godoc) -- "no admin basic auth on /git/*" is enforced
// one layer down, not duplicated here.
func (g *GitRoleGate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capability, ok := gitOperationCapability(r)
		if !ok {
			writeGitForbidden(w, "loam: unrecognized git operation")
			return
		}
		if err := g.checker.RequireCapability(r.Context(), capability); err != nil {
			if errors.Is(err, ErrPermissionDenied) {
				writeGitForbidden(w, gitRoleGateReason(r.Context(), capability))
				return
			}
			g.logger.Error("git role gate: capability check failed", "capability", capability, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gitOperationCapability maps a request to the Capability docs/git-spec.md
// -> "Operations & Role Gates" requires for it. ok is false for any method/
// path/query combination outside the three request shapes the smart-HTTP
// protocol defines.
func gitOperationCapability(r *http.Request) (Capability, bool) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
		return gitServiceCapability(r.URL.Query().Get("service"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/"+gitServiceUploadPack):
		return CapabilityGitClone, true
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/"+gitServiceReceivePack):
		return CapabilityGitPush, true
	default:
		return "", false
	}
}

// gitServiceCapability maps the info/refs discovery request's ?service=
// value to the capability it requires.
func gitServiceCapability(service string) (Capability, bool) {
	switch service {
	case gitServiceUploadPack:
		return CapabilityGitClone, true
	case gitServiceReceivePack:
		return CapabilityGitPush, true
	default:
		return "", false
	}
}

// writeGitForbidden writes a plain-text 403 carrying reason as its body --
// never a bare status code with an empty body -- so a rejected git
// operation surfaces something a human can act on. docs/git-spec.md pins
// an exact "loam:"-prefixed reason format for pre-receive ref-policy
// rejections (e.g. "loam: refs/heads/main is read-only (target branch)"),
// but does not pin one for this HTTP-layer capability gate; the messages
// here follow the same "loam: " prefix and style deliberately, as the
// closest documented precedent, rather than inventing an unrelated shape.
//
// The Content-Type header is load-bearing, not cosmetic: git's own HTTP
// client (remote-curl) only echoes a non-2xx response body back to the
// user (as a "remote: ..." line ahead of its own "fatal: ..." line) when
// the body's Content-Type is text/plain -- confirmed against real git
// 2.50.1 for clone, ls-remote, and push. application/octet-stream or
// text/html (or no header at all) makes git discard the body entirely,
// silently downgrading every "loam: ..." reason here to a bare, opaque
// 403 on the wire. Do not drop or "clean up" this header.
func writeGitForbidden(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, "%s\n", reason)
}

// gitRoleGateReason renders the human-actionable reason for a capability
// denial: the resolved role and the specific capability it lacks, so an
// operator reading git's stderr knows exactly which role to grant it to
// (docs/web-spec.md -> RoleService). The no-identity case is defensive --
// internal/httpauth.GitIdentity never lets a request reach next without a
// resolved identity in the real mux, so this only fires if some future
// caller wires Middleware in front of something other than GitIdentity.
//
// identity.Role is rendered with %q, not %s, deliberately: it is asserted
// from a request header (docs/git-spec.md -> "Identity on Git Operations"
// -- MVP identity is trusted, not verified), so an attacker-controlled
// value could otherwise contain a literal newline. %q escapes it (and
// wraps it in quotes), which stops a forged role value from injecting a
// second "remote: loam: ..." line into git's stderr to spoof a different
// (e.g. accepting) response. Do not simplify this back to %s.
func gitRoleGateReason(ctx context.Context, capability Capability) string {
	identity, ok := httpauth.IdentityFromContext(ctx)
	if !ok {
		return "loam: forbidden: missing agent identity"
	}
	return fmt.Sprintf("loam: role %q may not %s (missing %s capability)", identity.Role, gitOperationVerb(capability), capability)
}

// gitOperationVerb renders capability as the verb a human reads in a
// rejection reason.
func gitOperationVerb(capability Capability) string {
	switch capability {
	case CapabilityGitClone:
		return "clone/fetch"
	case CapabilityGitPush:
		return "push"
	default:
		return string(capability)
	}
}
