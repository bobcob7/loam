package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// Header names for the trusted agent identity, on the wire per
// docs/cli-spec.md / docs/git-spec.md -> Identity on Git Operations.
const (
	headerAgentName = "Loam-Agent-Name"
	headerAgentID   = "Loam-Agent-Id"
	headerAgentRole = "Loam-Agent-Role"
)

// wwwAuthenticateRealm is the WWW-Authenticate challenge sent with every
// 401 so a browser prompts for admin credentials (docs/web-spec.md ->
// Auth, "Login" screen).
const wwwAuthenticateRealm = `Basic realm="loam"`

// Auth builds the three composable http.Handler wrappers for the path
// groups in docs/web-spec.md -> Hosting & Routing. Credentials are
// injected via New; there is no package-level state, so a process can
// construct as many Auth values as it likes (it only ever needs one).
type Auth struct {
	adminUser     string
	adminPassword string
}

// New builds an Auth from the admin basic-auth credentials resolved by
// internal/config (LOAM_ADMIN_USER / LOAM_ADMIN_PASSWORD). It does not read
// the environment itself.
func New(adminUser, adminPassword string) *Auth {
	return &Auth{adminUser: adminUser, adminPassword: adminPassword}
}

// AdminOnly wraps /loam.admin.v1.* and every static/SPA path
// (docs/web-spec.md -> Auth). It accepts nothing but valid admin basic
// auth: absent, malformed, or wrong credentials get a 401 with
// WWW-Authenticate so the browser prompts; agent identity headers alone
// are never sufficient here, matching the security-relevant negative case
// in the bead's acceptance criteria. On success the request context is
// marked admin (see IsAdmin).
func (a *Auth) AdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, valid := a.checkBasicAuth(r); !valid {
			w.Header().Set("WWW-Authenticate", wwwAuthenticateRealm)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithAdmin(r.Context())))
	})
}

// CLI wraps /loam.v1.* (docs/web-spec.md -> Auth). Valid admin basic auth
// takes priority and marks the context admin (with no agent identity set
// alongside it), so the web UI reuses these same CLI services as a
// superuser. A *presented but wrong* Basic credential fails closed with
// the same 401 + WWW-Authenticate as AdminOnly — docs/web-spec.md says the
// CLI API never prompts for basic auth, so agents never send one, and
// silently downgrading a rejected admin credential to anonymous would turn
// a typo'd password into a much harder to diagnose CodePermissionDenied
// further down the stack instead of a 401. Only when no Basic credential
// was presented at all does the request fall back to the agent identity
// headers — and, per loam-gcg (decided by the repo owner 2026-07-25: no
// unauthorized requests without a use-case), all three of those headers
// must now be present. A request carrying neither a valid admin credential
// nor a complete set of Loam-Agent-* headers is rejected with the same 401
// + WWW-Authenticate, before it ever reaches a handler: every legitimate
// CLI client sets all three LOAM_AGENT_* env vars (docs/cli-spec.md:51-53),
// so there is no client this could break, and it decouples this guarantee
// from RequireCapability remembering to deny an empty identity on every
// RPC. This applies uniformly, including to the capability-ungated
// instructions/whoami RPCs (docs/web-spec.md -> RoleService) — ungated
// means they skip the capability check, not that they are reachable
// anonymously.
func (a *Auth) CLI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, valid := a.checkBasicAuth(r)
		if presented && !valid {
			w.Header().Set("WWW-Authenticate", wwwAuthenticateRealm)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := r.Context()
		switch {
		case valid:
			ctx = WithAdmin(ctx)
		default:
			identity, ok := agentIdentityFromHeaders(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", wwwAuthenticateRealm)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx = WithIdentity(ctx, identity)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GitIdentity wraps /git/* (docs/web-spec.md -> Auth, NOTES spec
// correction). Admin basic auth is never accepted here — even valid
// credentials confer nothing, since this wrapper never inspects the
// Authorization header at all — so a request presenting basic auth in
// place of agent identity headers is rejected exactly like one presenting
// no credentials. A request must carry all three Loam-Agent-* headers;
// otherwise it is rejected 403 (not 401 — docs/git-spec.md: "so
// unconfigured git clients fail fast instead of prompting for
// credentials"). It also defensively clears any admin marker already on
// the incoming context via withoutAdmin: nothing sets one before this
// wrapper today, but if a future mux ever nests GitIdentity inside an
// admin wrapper, RequireCapability's IsAdmin bypass must not silently
// grant every git role gate — this path never carries superuser status,
// regardless of wrapper ordering. Role-based enforcement on top of the
// resolved identity is loam-ofg.17's job; this wrapper only establishes
// identity.
func (a *Auth) GitIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := agentIdentityFromHeaders(r)
		if !ok {
			http.Error(w, "loam: forbidden: missing agent identity", http.StatusForbidden)
			return
		}
		ctx := WithIdentity(withoutAdmin(r.Context()), identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// agentIdentityFromHeaders reads the Loam-Agent-* headers into an
// Identity. ok is false unless all three are present, matching
// docs/cli-spec.md where LOAM_AGENT_NAME/_ID/_ROLE are all required.
func agentIdentityFromHeaders(r *http.Request) (Identity, bool) {
	name := r.Header.Get(headerAgentName)
	id := r.Header.Get(headerAgentID)
	role := r.Header.Get(headerAgentRole)
	if name == "" || id == "" || role == "" {
		return Identity{}, false
	}
	return Identity{Name: name, ID: id, Role: role}, true
}

// checkBasicAuth reports whether r carried an Authorization: Basic
// credential at all (presented) and, separately, whether it matched the
// configured admin credentials (valid), compared constant-time
// (docs/server-spec.md: "LOAM_ADMIN_PASSWORD ... compared constant-time").
// Callers that need to fail closed on a presented-but-wrong credential
// (CLI) check presented independently of valid; AdminOnly only needs
// valid. A non-Basic Authorization scheme (e.g. Bearer) leaves presented
// false, so it falls through to agent-header handling unchanged.
func (a *Auth) checkBasicAuth(r *http.Request) (presented, valid bool) {
	user, password, ok := r.BasicAuth()
	if !ok {
		return false, false
	}
	return true, constantTimeEqual(user, a.adminUser) && constantTimeEqual(password, a.adminPassword)
}

// constantTimeEqual compares two strings in constant time regardless of
// their length, by comparing fixed-size digests rather than the raw bytes
// (subtle.ConstantTimeCompare alone would short-circuit on length,
// leaking it).
func constantTimeEqual(a, b string) bool {
	digestA := sha256.Sum256([]byte(a))
	digestB := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(digestA[:], digestB[:]) == 1
}
