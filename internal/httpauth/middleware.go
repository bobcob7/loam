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
		if !a.validBasicAuth(r) {
			w.Header().Set("WWW-Authenticate", wwwAuthenticateRealm)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithAdmin(r.Context())))
	})
}

// CLI wraps /loam.v1.* (docs/web-spec.md -> Auth). Valid admin basic auth
// takes priority and marks the context admin, so the web UI reuses these
// same CLI services as a superuser; any other value in the Authorization
// header (missing, malformed, or simply wrong) falls back to the agent
// identity headers rather than rejecting the request, per the bead
// design's "otherwise fall back to agent identity headers". If neither is
// present the request still proceeds with no identity in context — the
// MVP trusts (does not require) the agent headers on this path group, as
// documented in the bead's acceptance criteria; a gated RPC then has
// nothing to authorize against and requireCapability denies it.
func (a *Auth) CLI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		switch {
		case a.validBasicAuth(r):
			ctx = WithAdmin(ctx)
		default:
			if identity, ok := agentIdentityFromHeaders(r); ok {
				ctx = WithIdentity(ctx, identity)
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GitIdentity wraps /git/* (docs/web-spec.md -> Auth, NOTES spec
// correction). Admin basic auth is never accepted here — even valid
// credentials confer nothing, since this wrapper never inspects the
// Authorization header — so a request presenting basic auth in place of
// agent identity headers is rejected exactly like one presenting no
// credentials at all. A request must carry all three Loam-Agent-* headers;
// otherwise it is rejected 403 (not 401 — docs/git-spec.md: "so
// unconfigured git clients fail fast instead of prompting for
// credentials"). Role-based enforcement on top of the resolved identity is
// loam-ofg.17's job; this wrapper only establishes identity.
func (a *Auth) GitIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := agentIdentityFromHeaders(r)
		if !ok {
			http.Error(w, "forbidden: missing agent identity", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
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

// validBasicAuth reports whether r carries the configured admin
// credentials, compared constant-time (docs/server-spec.md:
// "LOAM_ADMIN_PASSWORD ... compared constant-time").
func (a *Auth) validBasicAuth(r *http.Request) bool {
	user, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return constantTimeEqual(user, a.adminUser) && constantTimeEqual(password, a.adminPassword)
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
