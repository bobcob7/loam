package httpauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAdminUser     = "admin"
	testAdminPassword = "s3cret-pass"
)

// pathGroup names the three composable wrappers under test, mirroring the
// path groups in docs/web-spec.md -> Hosting & Routing (static assets
// share AdminOnly with /loam.admin.v1.*, so it is not a distinct case).
type pathGroup int

const (
	groupAdmin pathGroup = iota
	groupCLI
	groupGit
)

func wrap(auth *httpauth.Auth, group pathGroup, next http.Handler) http.Handler {
	switch group {
	case groupAdmin:
		return auth.AdminOnly(next)
	case groupCLI:
		return auth.CLI(next)
	case groupGit:
		return auth.GitIdentity(next)
	default:
		panic("unknown path group")
	}
}

// observed records whether the wrapped handler ran and the context it
// saw, so a test can assert on the identity/admin state a downstream
// handler would observe.
type observed struct {
	called   bool
	isAdmin  bool
	identity httpauth.Identity
	hasID    bool
}

// serve wraps group's middleware around a recording handler and serves a
// request built from setReq, starting from context.Background().
func serve(t *testing.T, auth *httpauth.Auth, group pathGroup, setReq func(r *http.Request)) (*httptest.ResponseRecorder, *observed) {
	t.Helper()
	return serveWithContext(t, auth, group, context.Background(), setReq)
}

// serveWithContext is serve, but lets a test seed the request's starting
// context — used to prove GitIdentity clears an admin marker it did not
// itself set (FIX 3: a future mux must not be able to nest GitIdentity
// inside an admin wrapper and leak superuser status onto /git/*).
func serveWithContext(t *testing.T, auth *httpauth.Auth, group pathGroup, baseCtx context.Context, setReq func(r *http.Request)) (*httptest.ResponseRecorder, *observed) {
	t.Helper()
	result := &observed{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result.called = true
		result.isAdmin = httpauth.IsAdmin(r.Context())
		result.identity, result.hasID = httpauth.IdentityFromContext(r.Context())
	})
	handler := wrap(auth, group, next)
	req := httptest.NewRequest(http.MethodGet, "/whatever", nil).WithContext(baseCtx)
	if setReq != nil {
		setReq(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, result
}

func withBasicAuth(user, password string) func(r *http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(user, password) }
}

func withAgentHeaders(name, id, role string) func(r *http.Request) {
	return func(r *http.Request) {
		if name != "" {
			r.Header.Set("Loam-Agent-Name", name)
		}
		if id != "" {
			r.Header.Set("Loam-Agent-Id", id)
		}
		if role != "" {
			r.Header.Set("Loam-Agent-Role", role)
		}
	}
}

func compose(fns ...func(r *http.Request)) func(r *http.Request) {
	return func(r *http.Request) {
		for _, fn := range fns {
			fn(r)
		}
	}
}

func TestAuth_Matrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		group        pathGroup
		setReq       func(r *http.Request)
		wantStatus   int
		wantWWWAuth  bool
		wantCalled   bool
		wantAdmin    bool
		wantIdentity httpauth.Identity
		wantHasID    bool
		wantBody     string
	}{
		// --- AdminOnly: /loam.admin.v1.* and static/SPA ---
		{
			name:        "admin: no credentials -> 401 with WWW-Authenticate",
			group:       groupAdmin,
			setReq:      nil,
			wantStatus:  http.StatusUnauthorized,
			wantWWWAuth: true,
			wantCalled:  false,
		},
		{
			name:        "admin: wrong basic auth -> 401 with WWW-Authenticate",
			group:       groupAdmin,
			setReq:      withBasicAuth(testAdminUser, "wrong-password"),
			wantStatus:  http.StatusUnauthorized,
			wantWWWAuth: true,
			wantCalled:  false,
		},
		{
			name:       "admin: correct basic auth -> pass, isAdmin true",
			group:      groupAdmin,
			setReq:     withBasicAuth(testAdminUser, testAdminPassword),
			wantStatus: http.StatusOK,
			wantCalled: true,
			wantAdmin:  true,
		},
		{
			// Security-relevant negative case: agent identity headers must
			// never substitute for admin basic auth on admin paths.
			name:        "admin: agent identity headers alone -> REJECTED (401)",
			group:       groupAdmin,
			setReq:      withAgentHeaders("ada-lovelace", "7", "author"),
			wantStatus:  http.StatusUnauthorized,
			wantWWWAuth: true,
			wantCalled:  false,
		},
		{
			// A6: extraneous agent headers alongside a valid admin credential
			// must not leak into the identity context — the superuser carries
			// no agent identity on this path group either.
			name:  "admin: correct basic auth plus agent headers -> isAdmin true, no identity",
			group: groupAdmin,
			setReq: compose(
				withBasicAuth(testAdminUser, testAdminPassword),
				withAgentHeaders("ada-lovelace", "7", "author"),
			),
			wantStatus: http.StatusOK,
			wantCalled: true,
			wantAdmin:  true,
			wantHasID:  false,
		},
		// --- CLI: /loam.v1.* ---
		{
			name:       "cli: correct basic auth -> pass, isAdmin true (superuser)",
			group:      groupCLI,
			setReq:     withBasicAuth(testAdminUser, testAdminPassword),
			wantStatus: http.StatusOK,
			wantCalled: true,
			wantAdmin:  true,
		},
		{
			name:         "cli: agent identity headers only -> pass, identity set, isAdmin false",
			group:        groupCLI,
			setReq:       withAgentHeaders("ada-lovelace", "7", "author"),
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantAdmin:    false,
			wantIdentity: httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: "author"},
			wantHasID:    true,
		},
		{
			// loam-gcg: neither credential has no legitimate use-case, so
			// the request is rejected before it reaches a handler rather
			// than proceeding with an empty identity.
			name:        "cli: neither credential -> REJECTED (401), no use-case for anonymous access",
			group:       groupCLI,
			setReq:      nil,
			wantStatus:  http.StatusUnauthorized,
			wantWWWAuth: true,
			wantCalled:  false,
		},
		{
			// C3 (fail-closed, FIX 1): a presented-but-wrong admin credential
			// must not be silently downgraded to anonymous. It gets the same
			// 401 + WWW-Authenticate as AdminOnly, not a 200.
			name:        "cli: wrong basic auth, no agent headers -> REJECTED (401), fails closed",
			group:       groupCLI,
			setReq:      withBasicAuth(testAdminUser, "wrong-password"),
			wantStatus:  http.StatusUnauthorized,
			wantWWWAuth: true,
			wantCalled:  false,
		},
		{
			// The fail-closed rule is unconditional: presenting agent headers
			// alongside a wrong admin credential still gets rejected, it does
			// not fall back to the agent identity.
			name:  "cli: wrong basic auth plus agent headers -> still REJECTED (401), fails closed",
			group: groupCLI,
			setReq: compose(
				withBasicAuth(testAdminUser, "wrong-password"),
				withAgentHeaders("grace-hopper", "3", "reviewer"),
			),
			wantStatus:  http.StatusUnauthorized,
			wantWWWAuth: true,
			wantCalled:  false,
		},
		{
			// C6: a valid admin credential takes priority even when agent
			// headers are also present, and the superuser carries no agent
			// identity — handler beads code against this rule.
			name:  "cli: correct basic auth plus agent headers -> isAdmin true, no identity",
			group: groupCLI,
			setReq: compose(
				withBasicAuth(testAdminUser, testAdminPassword),
				withAgentHeaders("grace-hopper", "3", "reviewer"),
			),
			wantStatus: http.StatusOK,
			wantCalled: true,
			wantAdmin:  true,
			wantHasID:  false,
		},
		{
			// loam-gcg: incomplete agent headers are treated as absent, and
			// absent-with-no-admin-credential is now rejected too.
			name:        "cli: incomplete agent headers -> REJECTED (401), treated as absent",
			group:       groupCLI,
			setReq:      withAgentHeaders("ada-lovelace", "", "author"),
			wantStatus:  http.StatusUnauthorized,
			wantWWWAuth: true,
			wantCalled:  false,
		},
		{
			// A non-Basic Authorization scheme must not be treated as a
			// presented-but-wrong Basic credential: r.BasicAuth() reports
			// ok=false for it, so it falls through to agent-header handling
			// unchanged.
			name:  "cli: bearer token (non-basic scheme) -> falls through to agent identity",
			group: groupCLI,
			setReq: compose(
				func(r *http.Request) { r.Header.Set("Authorization", "Bearer some-opaque-token") },
				withAgentHeaders("ada-lovelace", "7", "author"),
			),
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantAdmin:    false,
			wantIdentity: httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: "author"},
			wantHasID:    true,
		},
		// --- GitIdentity: /git/* ---
		{
			name:         "git: agent identity headers -> pass, identity set",
			group:        groupGit,
			setReq:       withAgentHeaders("ada-lovelace", "7", "author"),
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantAdmin:    false,
			wantIdentity: httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: "author"},
			wantHasID:    true,
		},
		{
			// Security-relevant negative case: admin basic auth must never
			// grant access on /git/*, unlike /loam.v1.*.
			name:       "git: admin basic auth alone -> REJECTED (403)",
			group:      groupGit,
			setReq:     withBasicAuth(testAdminUser, testAdminPassword),
			wantStatus: http.StatusForbidden,
			wantCalled: false,
			wantBody:   "loam: forbidden: missing agent identity",
		},
		{
			// G6: admin basic auth is not merely insufficient alone (G2) but
			// INERT even alongside complete agent headers — proves there is
			// no superuser bypass on this path group at all, which
			// loam-ofg.17's role gates depend on.
			name:  "git: admin basic auth plus complete agent headers -> isAdmin false, identity set",
			group: groupGit,
			setReq: compose(
				withBasicAuth(testAdminUser, testAdminPassword),
				withAgentHeaders("ada-lovelace", "7", "author"),
			),
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantAdmin:    false,
			wantIdentity: httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: "author"},
			wantHasID:    true,
		},
		{
			name:       "git: no credentials -> 403 fail-fast",
			group:      groupGit,
			setReq:     nil,
			wantStatus: http.StatusForbidden,
			wantCalled: false,
			wantBody:   "loam: forbidden: missing agent identity",
		},
		{
			name:       "git: incomplete agent headers -> 403",
			group:      groupGit,
			setReq:     withAgentHeaders("ada-lovelace", "7", ""),
			wantStatus: http.StatusForbidden,
			wantCalled: false,
			wantBody:   "loam: forbidden: missing agent identity",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			auth := httpauth.New(testAdminUser, testAdminPassword)
			rec, got := serve(t, auth, tt.group, tt.setReq)
			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantWWWAuth {
				assert.NotEmpty(t, rec.Header().Get("WWW-Authenticate"), "expected a WWW-Authenticate challenge")
			}
			if tt.wantBody != "" {
				assert.Equal(t, tt.wantBody+"\n", rec.Body.String())
				assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"), "git's remote-curl silently discards this body unless Content-Type is text/plain")
			}
			require.Equal(t, tt.wantCalled, got.called, "next handler invocation mismatch")
			if !tt.wantCalled {
				return
			}
			assert.Equal(t, tt.wantAdmin, got.isAdmin)
			assert.Equal(t, tt.wantHasID, got.hasID)
			if tt.wantHasID {
				assert.Equal(t, tt.wantIdentity, got.identity)
				assert.Equal(t, tt.wantIdentity.Name+"-"+tt.wantIdentity.ID+"-"+tt.wantIdentity.Role, got.identity.Identifier())
			}
		})
	}
}

// TestGitIdentity_ClearsInheritedAdmin is FIX 3's regression test: even if
// a request arrives at GitIdentity already marked admin by some outer
// wrapper (not how loam-ofg.2 wires the mux today, but a mistake it must
// not be possible to make silently), the resolved context downstream must
// never read IsAdmin() true on /git/*.
func TestGitIdentity_ClearsInheritedAdmin(t *testing.T) {
	t.Parallel()
	auth := httpauth.New(testAdminUser, testAdminPassword)
	inheritedAdminCtx := httpauth.WithAdmin(context.Background())
	rec, got := serveWithContext(t, auth, groupGit, inheritedAdminCtx, withAgentHeaders("ada-lovelace", "7", "author"))
	assert.Equal(t, http.StatusOK, rec.Code)
	require.True(t, got.called)
	assert.False(t, got.isAdmin, "GitIdentity must clear an admin marker inherited from an ancestor context")
	assert.True(t, got.hasID)
}
