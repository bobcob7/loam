package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// stubRoleStore backs handler.NewCapabilityChecker with a fixed
// role->capability table mirroring docs/web-spec.md -> RoleService's two
// built-in roles, standing in for loam-ofg.13's real (not yet built) role
// store.
type stubRoleStore struct{}

func (stubRoleStore) RoleCapabilities(ctx context.Context, role string) ([]handler.Capability, error) {
	switch role {
	case "author":
		return []handler.Capability{handler.CapabilityGitClone, handler.CapabilityGitPush}, nil
	case "reviewer":
		return []handler.Capability{handler.CapabilityGitClone}, nil
	default:
		return nil, nil
	}
}

// gitProcessStub stands in for loam-ofg.16's real handler, which forks a
// git process. It records whether it ran at all, so tests can prove the
// role gate rejected a request before any (stand-in) git process would
// have been spawned -- the "403 fail-fast" half of this bead's title.
func gitProcessStub(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("git-ok"))
	})
}

// newGitTestRouter builds a real server.Router with the full /git/* chain
// this bead assembles: httpauth.Auth.GitIdentity (loam-ofg.3, already
// merged) wrapping handler.GitRoleGate (this bead) wrapping a stub
// standing in for loam-ofg.16's not-yet-built handler. This is the actual
// mux dispatch a live request travels through, not a hand-rolled call
// chain -- RegisterGit applies GitIdentity itself, so the gate argument
// below is exactly what a future ofg.16 wiring in cmd/server/main.go would
// pass.
func newGitTestRouter(t *testing.T, ran *bool) *server.Router {
	t.Helper()
	auth := httpauth.New(testAdminUser, testAdminPassword)
	checker := handler.NewCapabilityChecker(stubRoleStore{})
	gate := handler.NewGitRoleGate(checker, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	router := server.New(auth, noop.NewTracerProvider())
	router.RegisterGit(gitPrefix, gate.Middleware(gitProcessStub(ran)))
	return router
}

func withAgentHeaders(name, id, role string) func(r *http.Request) {
	return func(r *http.Request) {
		r.Header.Set(headerAgentName, name)
		r.Header.Set(headerAgentID, id)
		r.Header.Set(headerAgentRole, role)
	}
}

// TestRouter_GitRoleGate_EndToEnd drives the real mux end to end -- proving
// this bead's remaining acceptance criteria against the actual composition
// a request travels through, not internal/handler's gate in isolation.
func TestRouter_GitRoleGate_EndToEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		method     string
		path       string
		setReq     func(r *http.Request)
		wantStatus int
		wantRan    bool
	}{
		{
			// roles.feature: "A reviewer may clone the repo".
			name:       "reviewer clone via info/refs succeeds",
			method:     http.MethodGet,
			path:       "/git/acme/widgets.git/info/refs?service=git-upload-pack",
			setReq:     withAgentHeaders("ada", "7", "reviewer"),
			wantStatus: http.StatusOK,
			wantRan:    true,
		},
		{
			name:       "author push via git-receive-pack succeeds",
			method:     http.MethodPost,
			path:       "/git/acme/widgets.git/git-receive-pack",
			setReq:     withAgentHeaders("grace", "3", "author"),
			wantStatus: http.StatusOK,
			wantRan:    true,
		},
		{
			// docs/web-spec.md -> RoleService: reviewer "cannot ... push".
			name:       "reviewer push via git-receive-pack is denied",
			method:     http.MethodPost,
			path:       "/git/acme/widgets.git/git-receive-pack",
			setReq:     withAgentHeaders("ada", "7", "reviewer"),
			wantStatus: http.StatusForbidden,
			wantRan:    false,
		},
		{
			// docs/git-spec.md -> "Operations & Role Gates": "Admin basic
			// auth is not accepted on /git/*" -- a valid admin credential
			// alone (no agent headers at all) never reaches the gate.
			name:       "admin basic auth alone is rejected before the gate ever runs",
			method:     http.MethodPost,
			path:       "/git/acme/widgets.git/git-receive-pack",
			setReq:     withValidAdminAuth,
			wantStatus: http.StatusForbidden,
			wantRan:    false,
		},
		{
			// Stronger form of the above: even with a role that DOES carry
			// git.push, a bundled admin credential grants nothing extra,
			// and confers no bypass of the identity-driven role check.
			name:   "admin basic auth alongside a pushing role changes nothing",
			method: http.MethodPost,
			path:   "/git/acme/widgets.git/git-receive-pack",
			setReq: func(r *http.Request) {
				withValidAdminAuth(r)
				withAgentHeaders("grace", "3", "author")(r)
			},
			wantStatus: http.StatusOK,
			wantRan:    true,
		},
		{
			name:       "missing identity headers entirely is rejected before the gate ever runs",
			method:     http.MethodPost,
			path:       "/git/acme/widgets.git/git-receive-pack",
			setReq:     nil,
			wantStatus: http.StatusForbidden,
			wantRan:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ran := false
			router := newGitTestRouter(t, &ran)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.setReq != nil {
				tc.setReq(req)
			}
			rec := httptest.NewRecorder()
			router.Handler().ServeHTTP(rec, req)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantRan, ran, "stand-in git process invocation mismatch")
			if tc.wantStatus == http.StatusForbidden {
				// Real git 2.50.1 only surfaces a "remote: ..." line
				// (clone/ls-remote/push alike) when this header is
				// text/plain -- anything else silently drops the body,
				// which would make Demo M2's rejection assertions fail
				// against a real client while every Go-level test here
				// stayed green.
				assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain", "git's remote-curl discards the denial body unless Content-Type is text/plain")
			}
		})
	}
}

// TestRouter_GitRoleGate_MissingIdentityRejectionCarriesAReason proves
// Demo M2's second live rejection end to end against the real router: a
// push with no Loam-Agent-* headers at all (the CLI's clone writes them as
// http.extraHeader entries; stripping them reproduces exactly this
// request) never reaches the stand-in git process, is rejected 403 (not
// 401 -- docs/git-spec.md: "so unconfigured git clients fail fast"), and
// carries a body a human can act on.
func TestRouter_GitRoleGate_MissingIdentityRejectionCarriesAReason(t *testing.T) {
	t.Parallel()
	ran := false
	router := newGitTestRouter(t, &ran)
	req := httptest.NewRequest(http.MethodPost, "/git/acme/widgets.git/git-receive-pack", nil)
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, ran)
	assert.NotEmpty(t, rec.Body.String(), "a rejected push must carry a body a human can act on, not a bare 403")
	assert.Contains(t, rec.Body.String(), "loam: ", "Demo M2 asserts the rejection carries the documented loam:-prefixed reason")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain", "git's remote-curl silently discards this body unless Content-Type is text/plain")
}
