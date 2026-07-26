package handler_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// countingHandler records whether it was invoked, and answers 200.
func countingHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
}

// TestGitRoleGate_Middleware is the table-driven capability/role-gate test
// docs/git-spec.md -> "Operations & Role Gates" and this bead's Definition
// of Done both call for: it drives every recognized smart-HTTP request
// shape against both built-in roles (docs/web-spec.md -> RoleService:
// author carries git.clone+git.push, reviewer carries git.clone only),
// plus the identity/role-store edge cases the gate must fail closed on.
func TestGitRoleGate_Middleware(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		method      string
		target      string
		ctx         func() context.Context
		roleCaps    []handler.Capability
		roleErr     error
		wantStatus  int
		wantCalled  bool
		wantBodyHas string
		wantStoreOK bool // whether the role store is expected to be consulted at all.
	}{
		{
			name:        "clone via info/refs: author may clone",
			method:      http.MethodGet,
			target:      "/git/acme/widgets.git/info/refs?service=git-upload-pack",
			ctx:         identityCtx("grace-hopper-3", "author"),
			roleCaps:    []handler.Capability{handler.CapabilityGitClone, handler.CapabilityGitPush},
			wantStatus:  http.StatusOK,
			wantCalled:  true,
			wantStoreOK: true,
		},
		{
			// roles.feature: "A reviewer may clone the repo".
			name:        "clone via info/refs: reviewer may clone",
			method:      http.MethodGet,
			target:      "/git/acme/widgets.git/info/refs?service=git-upload-pack",
			ctx:         identityCtx("ada-lovelace", "reviewer"),
			roleCaps:    []handler.Capability{handler.CapabilityGitClone},
			wantStatus:  http.StatusOK,
			wantCalled:  true,
			wantStoreOK: true,
		},
		{
			name:        "clone via POST git-upload-pack: reviewer may clone",
			method:      http.MethodPost,
			target:      "/git/acme/widgets.git/git-upload-pack",
			ctx:         identityCtx("ada-lovelace", "reviewer"),
			roleCaps:    []handler.Capability{handler.CapabilityGitClone},
			wantStatus:  http.StatusOK,
			wantCalled:  true,
			wantStoreOK: true,
		},
		{
			name:        "push via info/refs: author may push",
			method:      http.MethodGet,
			target:      "/git/acme/widgets.git/info/refs?service=git-receive-pack",
			ctx:         identityCtx("grace-hopper-3", "author"),
			roleCaps:    []handler.Capability{handler.CapabilityGitClone, handler.CapabilityGitPush},
			wantStatus:  http.StatusOK,
			wantCalled:  true,
			wantStoreOK: true,
		},
		{
			name:        "push via POST git-receive-pack: author may push",
			method:      http.MethodPost,
			target:      "/git/acme/widgets.git/git-receive-pack",
			ctx:         identityCtx("grace-hopper-3", "author"),
			roleCaps:    []handler.Capability{handler.CapabilityGitClone, handler.CapabilityGitPush},
			wantStatus:  http.StatusOK,
			wantCalled:  true,
			wantStoreOK: true,
		},
		{
			// docs/web-spec.md -> RoleService: "reviewer ... cannot ... push".
			name:        "push via POST git-receive-pack: reviewer denied",
			method:      http.MethodPost,
			target:      "/git/acme/widgets.git/git-receive-pack",
			ctx:         identityCtx("ada-lovelace", "reviewer"),
			roleCaps:    []handler.Capability{handler.CapabilityGitClone},
			wantStatus:  http.StatusForbidden,
			wantCalled:  false,
			wantBodyHas: `loam: role "reviewer" may not push (missing git.push capability)`,
			wantStoreOK: true,
		},
		{
			name:        "push via info/refs: reviewer denied",
			method:      http.MethodGet,
			target:      "/git/acme/widgets.git/info/refs?service=git-receive-pack",
			ctx:         identityCtx("ada-lovelace", "reviewer"),
			roleCaps:    []handler.Capability{handler.CapabilityGitClone},
			wantStatus:  http.StatusForbidden,
			wantCalled:  false,
			wantBodyHas: `loam: role "reviewer" may not push`,
			wantStoreOK: true,
		},
		{
			name:        "unrecognized git operation is denied closed",
			method:      http.MethodGet,
			target:      "/git/acme/widgets.git/HEAD",
			ctx:         identityCtx("grace-hopper-3", "author"),
			wantStatus:  http.StatusForbidden,
			wantCalled:  false,
			wantBodyHas: "loam: unrecognized git operation",
			wantStoreOK: false,
		},
		{
			name:        "info/refs with unrecognized service value is denied closed",
			method:      http.MethodGet,
			target:      "/git/acme/widgets.git/info/refs?service=git-upload-archive",
			ctx:         identityCtx("grace-hopper-3", "author"),
			wantStatus:  http.StatusForbidden,
			wantCalled:  false,
			wantBodyHas: "loam: unrecognized git operation",
			wantStoreOK: false,
		},
		{
			// Defence-in-depth: internal/httpauth.GitIdentity never lets a
			// request reach this gate without a resolved identity in the
			// real mux, but the gate must still fail closed if it ever did.
			name:        "no identity in context is denied closed",
			method:      http.MethodPost,
			target:      "/git/acme/widgets.git/git-upload-pack",
			ctx:         func() context.Context { return t.Context() },
			wantStatus:  http.StatusForbidden,
			wantCalled:  false,
			wantBodyHas: "loam: forbidden: missing agent identity",
			wantStoreOK: false,
		},
		{
			name:        "role store failure is a 500, not a policy 403",
			method:      http.MethodPost,
			target:      "/git/acme/widgets.git/git-upload-pack",
			ctx:         identityCtx("grace-hopper-3", "author"),
			roleErr:     errors.New("role store unreachable"),
			wantStatus:  http.StatusInternalServerError,
			wantCalled:  false,
			wantStoreOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			storeCalled := false
			store := &handler.RoleStoreMock{
				RoleCapabilitiesFunc: func(ctx context.Context, role string) ([]handler.Capability, error) {
					storeCalled = true
					return tt.roleCaps, tt.roleErr
				},
			}
			checker := handler.NewCapabilityChecker(store)
			gate := handler.NewGitRoleGate(checker, testLogger())
			called := false
			next := countingHandler(&called)
			req := httptest.NewRequest(tt.method, tt.target, nil).WithContext(tt.ctx())
			rec := httptest.NewRecorder()
			gate.Middleware(next).ServeHTTP(rec, req)
			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantCalled, called, "next handler invocation mismatch")
			assert.Equal(t, tt.wantStoreOK, storeCalled, "role store call mismatch")
			if tt.wantBodyHas != "" {
				assert.Contains(t, rec.Body.String(), tt.wantBodyHas)
			}
		})
	}
}

// identityCtx builds a context carrying a resolved agent identity, the
// same shape internal/httpauth.GitIdentity places on a request context
// before loam-ofg.16's handler (and, on top of it, this gate) ever sees
// the request.
func identityCtx(id, role string) func() context.Context {
	return func() context.Context {
		return httpauth.WithIdentity(context.Background(), httpauth.Identity{Name: "agent", ID: id, Role: role})
	}
}

// TestGitRoleGate_AdminBypassIsReusedDeliberately documents and pins down
// a load-bearing but non-obvious fact: GitRoleGate reuses
// CapabilityChecker.RequireCapability verbatim, including its admin-
// superuser bypass. On /git/* this bypass must never actually be
// reachable -- docs/git-spec.md -> "Operations & Role Gates": "Admin basic
// auth is not accepted on /git/*" -- and it isn't, because
// internal/httpauth.GitIdentity unconditionally clears any admin marker
// before a request reaches next (proven by
// TestGitIdentity_ClearsInheritedAdmin and TestAuth_Matrix's G6 case in
// internal/httpauth). This test exercises GitRoleGate in isolation, with
// an admin-marked context manufactured directly (bypassing GitIdentity),
// to make that reliance visible: it is GitIdentity's job to guarantee this
// input never occurs on /git/*, not this gate's.
func TestGitRoleGate_AdminBypassIsReusedDeliberately(t *testing.T) {
	t.Parallel()
	store := &handler.RoleStoreMock{
		RoleCapabilitiesFunc: func(ctx context.Context, role string) ([]handler.Capability, error) {
			t.Fatal("role store must not be consulted for an admin-marked context")
			return nil, nil
		},
	}
	checker := handler.NewCapabilityChecker(store)
	gate := handler.NewGitRoleGate(checker, testLogger())
	called := false
	next := countingHandler(&called)
	req := httptest.NewRequest(http.MethodPost, "/git/acme/widgets.git/git-receive-pack", nil)
	req = req.WithContext(httpauth.WithAdmin(t.Context()))
	rec := httptest.NewRecorder()
	gate.Middleware(next).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, called, "an admin-marked context bypasses the capability check by design; keeping it off /git/* is GitIdentity's job")
}

// TestGitRoleGate_DeniedRequestNeverInvokesNext is the fail-fast proof the
// bead calls for explicitly: next is a handler that fails the test the
// instant it is invoked, so this test can only pass if the capability
// check runs, and the denial response is written, strictly before next
// would ever be called -- exactly the property that stops an
// unauthenticated/unauthorized probe from ever causing loam-ofg.16 to fork
// a real git-upload-pack/git-receive-pack process. A regression that moved
// the capability check to run after next.ServeHTTP (or that called next
// unconditionally before checking) fails this test immediately, rather
// than merely producing a wrong status code that a less targeted
// assertion might miss.
func TestGitRoleGate_DeniedRequestNeverInvokesNext(t *testing.T) {
	t.Parallel()
	store := &handler.RoleStoreMock{
		RoleCapabilitiesFunc: func(ctx context.Context, role string) ([]handler.Capability, error) {
			return []handler.Capability{handler.CapabilityGitClone}, nil
		},
	}
	checker := handler.NewCapabilityChecker(store)
	gate := handler.NewGitRoleGate(checker, testLogger())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must never be invoked: the capability check must reject this request before any git process would be spawned")
	})
	ctx := httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "agent", ID: "9", Role: "reviewer"})
	req := httptest.NewRequest(http.MethodPost, "/git/acme/widgets.git/git-receive-pack", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	gate.Middleware(next).ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}
