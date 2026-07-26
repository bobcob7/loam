package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bobcob7/loam/internal/httpauth"
)

// cliPathPrefix, adminPathPrefix and gitPathPrefix are the three
// path-group prefixes in docs/web-spec.md -> Hosting & Routing. They are
// used only to fail fast (panic) if a caller ever registers a handler
// under the wrong group — a mis-wiring here would silently undo
// internal/httpauth's auth matrix, so RegisterCLI/RegisterAdmin/RegisterGit
// refuse a path that does not belong to their group rather than trust the
// caller.
const (
	cliPathPrefix   = "/loam.v1."
	adminPathPrefix = "/loam.admin.v1."
	gitPathPrefix   = "/git/"
)

// isHealthPath is the exhaustive allow-list for RegisterUnauthenticated.
// docs/server-spec.md calls /healthz and /readyz "the only such
// exemption"; enforcing that here means a future accidental call with any
// other pattern fails at process startup instead of quietly opening a new
// unauthenticated route.
func isHealthPath(pattern string) bool {
	switch pattern {
	case "/healthz", "/readyz", "GET /healthz", "GET /readyz":
		return true
	default:
		return false
	}
}

// Router builds the single mux described in docs/web-spec.md -> Hosting &
// Routing. It holds no package-level state; a process constructs exactly
// one from New in its composition root (cmd/server/main.go).
type Router struct {
	mux  *http.ServeMux
	auth *httpauth.Auth
}

// New builds an empty Router wrapping every registered handler with the
// auth regime for its path group, per auth. Callers add handlers with the
// RegisterXxx methods, then take the assembled http.Handler via Handler.
func New(auth *httpauth.Auth) *Router {
	return &Router{mux: http.NewServeMux(), auth: auth}
}

// RegisterCLI mounts handler at path behind httpauth.Auth.CLI, for the
// /loam.v1.* Connect service group (docs/web-spec.md -> Hosting &
// Routing). path is the value a generated loamv1connect.NewXxxServiceHandler
// constructor returns, e.g. "/loam.v1.WorkBranchService/" — callers pass
// that pair straight through:
//
//	router.RegisterCLI(loamv1connect.NewWorkBranchServiceHandler(impl))
//
// It panics if path does not start with "/loam.v1." — a programming error
// caught at composition-root wiring time, not a runtime condition.
func (rt *Router) RegisterCLI(path string, handler http.Handler) {
	if !strings.HasPrefix(path, cliPathPrefix) {
		panic(fmt.Sprintf("server: RegisterCLI: path %q must start with %q", path, cliPathPrefix))
	}
	rt.mux.Handle(path, rt.auth.CLI(handler))
}

// RegisterAdmin mounts handler at path behind httpauth.Auth.AdminOnly, for
// the /loam.admin.v1.* Connect service group (docs/web-spec.md -> Hosting
// & Routing). path is the value a generated
// adminv1connect.NewXxxServiceHandler constructor returns, e.g.
// "/loam.admin.v1.RepoAdminService/". It panics if path does not start
// with "/loam.admin.v1." (see RegisterCLI).
func (rt *Router) RegisterAdmin(path string, handler http.Handler) {
	if !strings.HasPrefix(path, adminPathPrefix) {
		panic(fmt.Sprintf("server: RegisterAdmin: path %q must start with %q", path, adminPathPrefix))
	}
	rt.mux.Handle(path, rt.auth.AdminOnly(handler))
}

// RegisterGit mounts handler at prefix behind httpauth.Auth.GitIdentity,
// for the /git/* smart-HTTP group (docs/web-spec.md -> Hosting & Routing,
// NOTES spec correction; the concrete handler is loam-ofg.16's). It
// panics if prefix does not start with "/git/" (see RegisterCLI).
func (rt *Router) RegisterGit(prefix string, handler http.Handler) {
	if !strings.HasPrefix(prefix, gitPathPrefix) {
		panic(fmt.Sprintf("server: RegisterGit: path %q must start with %q", prefix, gitPathPrefix))
	}
	rt.mux.Handle(prefix, rt.auth.GitIdentity(handler))
}

// RegisterUnauthenticated mounts handler at pattern with NO auth wrapper
// at all. pattern must be one of "/healthz", "/readyz" (with or without a
// leading "GET " method per Go 1.22+ mux patterns) — docs/server-spec.md
// -> Health: "the only such exemption". The exemption falls out of this
// method existing as routing, not out of any auth middleware special-
// casing a path string (docs/web-spec.md -> Auth: "a routing fact ... not
// a special case inside this middleware") — loam-gcg deliberately kept
// internal/httpauth free of any path check for exactly this reason: a
// path check inside a middleware wrapper is an auth bypass keyed on an
// attacker-influenced string. RegisterUnauthenticated panics for any other
// pattern, so a future mistaken call cannot open an unintended
// unauthenticated route.
func (rt *Router) RegisterUnauthenticated(pattern string, handler http.Handler) {
	if !isHealthPath(pattern) {
		panic(fmt.Sprintf("server: RegisterUnauthenticated: %q is not a recognized health path (docs/server-spec.md: /healthz and /readyz are the only exemption)", pattern))
	}
	rt.mux.Handle(pattern, handler)
}

// Handler returns the assembled http.Handler for the single HTTP listener
// (docs/server-spec.md -> Process Model). Building and running the
// *http.Server around it, and coordinating its shutdown with the rest of
// the process, is NewHTTPServer's and loam-ofg.21's job respectively.
//
// The returned value is deliberately opaque (http.HandlerFunc, not the
// *http.ServeMux itself): the mux underlying this Router is only ever
// mutated by RegisterCLI/RegisterAdmin/RegisterGit/RegisterUnauthenticated/
// RegisterSPA, each of which enforces which auth wrapper a path group gets.
// Returning the concrete *http.ServeMux would let a type assertion recover
// it and register routes directly, unwrapped by any of those guards —
// defeating the whole structural guarantee this package exists to
// establish.
func (rt *Router) Handler() http.Handler {
	return http.HandlerFunc(rt.mux.ServeHTTP)
}
