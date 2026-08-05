package server

import (
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/bobcob7/loam/internal/httpauth"
	"go.opentelemetry.io/otel/trace"
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
	mux        *http.ServeMux
	auth       *httpauth.Auth
	rpcOptions []connect.HandlerOption
}

// New builds an empty Router wrapping every registered handler with the
// auth regime for its path group, per auth. Callers add handlers with the
// RegisterXxx methods, then take the assembled http.Handler via Handler.
//
// tracerProvider is the OpenTelemetry provider the RPC spans described in
// tracing.go are created from — internal/telemetry hands one out, and it is
// never nil there even when telemetry is disabled (it is upstream's no-op).
// A nil provider is accepted anyway and degrades to the no-op, so a test
// that only cares about routing need not thread telemetry through. It is a
// parameter rather than a global because internal/telemetry deliberately
// installs no OpenTelemetry global; see that package's doc comment.
//
// It panics if the instrumentation cannot be constructed. That is a
// composition-root wiring failure in the same class as RegisterCLI's
// wrong-prefix panic: it cannot depend on request data, so it fails at
// process startup or never.
func New(auth *httpauth.Auth, tracerProvider trace.TracerProvider) *Router {
	options, err := rpcOptions(tracerProvider)
	if err != nil {
		panic(fmt.Sprintf("server: New: building RPC instrumentation: %v", err))
	}
	return &Router{mux: http.NewServeMux(), auth: auth, rpcOptions: options}
}

// RegisterCLI mounts handler at path behind httpauth.Auth.CLI, for the
// /loam.v1.* Connect service group (docs/web-spec.md -> Hosting &
// Routing). path is the value a generated loamv1connect.NewXxxServiceHandler
// constructor returns, e.g. "/loam.v1.WorkBranchService/" — callers pass
// that pair straight through:
//
//	router.RegisterCLI(loamv1connect.NewWorkBranchServiceHandler(impl, router.RPCOptions()...))
//
// RPCOptions() carries this process's RPC tracing (tracing.go); pass it to
// every constructor registered here. Unlike the auth wrapper, this method
// cannot apply it for the caller — connect interceptors are handler
// CONSTRUCTION options and there is no seam for attaching one to a built
// http.Handler — so a constructor called without them routes and
// authenticates correctly but produces no span.
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
// "/loam.admin.v1.RepoAdminService/". Pass router.RPCOptions()... to the
// constructor, for the reason spelled out on RegisterCLI. It panics if path
// does not start with "/loam.admin.v1." (see RegisterCLI).
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
//
// Handlers mounted here are also deliberately NOT TRACED, and the natural
// instinct — wrap Handler()'s mux in otelhttp so nothing can be missed — is
// exactly what would break it. /healthz and /readyz are polled on a tight
// liveness interval; a span each would drown real traffic and spend
// collector budget on the two requests carrying the least information in
// the system. Because the RPC instrumentation lives at the connect layer
// (see rpcOptions in tracing.go) and these are plain http.Handlers, the
// exemption is structural: there is no options set they could receive and
// no path string anywhere that excludes them.
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
// Before deferring to the mux, it checks whether the request would only
// match the mux's least-specific registered pattern ("/", or none at all) —
// i.e. no RegisterCLI/RegisterAdmin/RegisterGit call claimed it — and, if
// so, whether the path falls under one of the three service-group prefixes
// (/loam.v1., /loam.admin.v1., /git/) anyway. If it does, that request gets
// the group's own 404 in the group's own wire format and auth regime
// (loam-cjq), instead of silently falling through to the SPA's index.html.
// A path already claimed by a more specific pattern is entirely unaffected
// — mux.Handler's own precedence rules already routed it to that service's
// handler before this check ever runs, so an unknown *procedure* within a
// *registered* service keeps getting that service's own 404 exactly as
// before.
//
// The peek deliberately discards the *http.Handler mux.Handler(r) returns
// and re-dispatches through rt.mux.ServeHTTP instead of serving that
// handler directly — do not "simplify" this into a single call. Per
// net/http's ServeMux.Handler godoc, "it does not populate named path
// wildcards, so r.PathValue will always return the empty string" on the
// request passed to Handler; only a subsequent mux.ServeHTTP populates
// them on r. Serving the peeked handler would silently break
// r.PathValue for every wildcard-pattern registration this Router
// carries — most notably the "/git/{repo...}" shape loam-ofg.16 is
// expected to register — with no test failure pointing at this line to
// explain why.
//
// The returned value is deliberately opaque (a plain http.HandlerFunc, not
// the *http.ServeMux itself): the mux underlying this Router is only ever
// mutated by RegisterCLI/RegisterAdmin/RegisterGit/RegisterUnauthenticated/
// RegisterSPA, each of which enforces which auth wrapper a path group gets.
// Returning the concrete *http.ServeMux would let a type assertion recover
// it and register routes directly, unwrapped by any of those guards —
// defeating the whole structural guarantee this package exists to
// establish.
func (rt *Router) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := rt.mux.Handler(r); pattern != "" && pattern != "/" {
			rt.mux.ServeHTTP(w, r)
			return
		}
		if fallback, ok := rt.groupFallback(r.URL.Path); ok {
			fallback.ServeHTTP(w, r)
			return
		}
		rt.mux.ServeHTTP(w, r)
	})
}
