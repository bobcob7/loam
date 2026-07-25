package httpauth_test

import (
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

// capture records whether the wrapped handler ran and the context it saw,
// so a test can assert on the identity/admin state a downstream handler
// would observe.
type capture struct {
	called   bool
	isAdmin  bool
	identity httpauth.Identity
	hasID    bool
}

func serve(t *testing.T, auth *httpauth.Auth, group pathGroup, setReq func(r *http.Request)) (*httptest.ResponseRecorder, *capture) {
	t.Helper()
	cap := &capture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.called = true
		cap.isAdmin = httpauth.IsAdmin(r.Context())
		cap.identity, cap.hasID = httpauth.IdentityFromContext(r.Context())
	})
	handler := wrap(auth, group, next)
	req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	if setReq != nil {
		setReq(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, cap
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
			name:       "cli: neither credential -> MVP trusts through, no identity, isAdmin false",
			group:      groupCLI,
			setReq:     nil,
			wantStatus: http.StatusOK,
			wantCalled: true,
			wantAdmin:  false,
			wantHasID:  false,
		},
		{
			name:  "cli: wrong basic auth plus agent headers -> falls back to agent identity, not admin",
			group: groupCLI,
			setReq: compose(
				withBasicAuth(testAdminUser, "wrong-password"),
				withAgentHeaders("grace-hopper", "3", "reviewer"),
			),
			wantStatus:   http.StatusOK,
			wantCalled:   true,
			wantAdmin:    false,
			wantIdentity: httpauth.Identity{Name: "grace-hopper", ID: "3", Role: "reviewer"},
			wantHasID:    true,
		},
		{
			name:       "cli: incomplete agent headers -> treated as absent, no identity",
			group:      groupCLI,
			setReq:     withAgentHeaders("ada-lovelace", "", "author"),
			wantStatus: http.StatusOK,
			wantCalled: true,
			wantAdmin:  false,
			wantHasID:  false,
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
		},
		{
			name:       "git: no credentials -> 403 fail-fast",
			group:      groupGit,
			setReq:     nil,
			wantStatus: http.StatusForbidden,
			wantCalled: false,
		},
		{
			name:       "git: incomplete agent headers -> 403",
			group:      groupGit,
			setReq:     withAgentHeaders("ada-lovelace", "7", ""),
			wantStatus: http.StatusForbidden,
			wantCalled: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			auth := httpauth.New(testAdminUser, testAdminPassword)
			rec, cap := serve(t, auth, tt.group, tt.setReq)
			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantWWWAuth {
				assert.NotEmpty(t, rec.Header().Get("WWW-Authenticate"), "expected a WWW-Authenticate challenge")
			}
			require.Equal(t, tt.wantCalled, cap.called, "next handler invocation mismatch")
			if !tt.wantCalled {
				return
			}
			assert.Equal(t, tt.wantAdmin, cap.isAdmin)
			assert.Equal(t, tt.wantHasID, cap.hasID)
			if tt.wantHasID {
				assert.Equal(t, tt.wantIdentity, cap.identity)
				assert.Equal(t, tt.wantIdentity.Name+"-"+tt.wantIdentity.ID+"-"+tt.wantIdentity.Role, cap.identity.Identifier())
			}
		})
	}
}
