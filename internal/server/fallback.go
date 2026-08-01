package server

import (
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
)

// groupFallback returns the handler Handler should use for path when no
// more specific pattern registered via RegisterCLI/RegisterAdmin/
// RegisterGit/RegisterSPA applies (loam-cjq). ServeMux's own longest-prefix
// matching cannot express "any /loam.v1.<unregistered service>/*" as a
// registered pattern -- a wildcard segment must be the ENTIRE path segment
// (net/http.ServeMux: "/b_{bucket}" is invalid), and "loam.v1." is a
// literal prefix WITHIN a segment ("loam.v1.MetaService"), not a segment
// boundary -- so this check runs in code, at the one point (Handler) where
// mux has already determined no registered service claims the request.
// ok is false for any path outside the three known groups, so genuine SPA
// routes fall through unchanged.
func (rt *Router) groupFallback(path string) (handler http.Handler, ok bool) {
	switch {
	case strings.HasPrefix(path, cliPathPrefix):
		return rt.auth.CLI(connectNotFoundHandler(cliPathPrefix, path)), true
	case strings.HasPrefix(path, adminPathPrefix):
		return rt.auth.AdminOnly(connectNotFoundHandler(adminPathPrefix, path)), true
	case strings.HasPrefix(path, gitPathPrefix):
		return rt.auth.GitIdentity(gitNotFoundHandler()), true
	default:
		return nil, false
	}
}

// connectNotFoundHandler answers any request under group (a registered
// service was not matched by mux) with a Connect CodeNotFound error,
// rendered in whichever of the three protocols connect-go handlers serve by
// default -- Connect, gRPC, or gRPC-Web (loam-i0v) -- the request's
// Content-Type indicates. This applies to both GET and POST alike -- there
// is no method guard here, unlike spaHandler's, since every request under
// an RPC group prefix is an API request, never a browser navigation.
//
// The error's code is always CodeNotFound, never CodeUnimplemented, on all
// three wires: a registered service answering an absent PROCEDURE would
// itself return Unimplemented, but here the whole SERVICE is unregistered,
// and CodeNotFound is what the Connect protocol path returned before this
// fix (loam-cjq). Keeping it, rather than switching to Unimplemented for
// gRPC/gRPC-Web, keeps the reported code consistent across every protocol
// instead of the bug this fixes: previously the fallback always wrote a
// bare HTTP 404, which a real gRPC client decoded as "unimplemented",
// silently discarding both the intended code and the message.
//
// connect.ErrorWriter (connectrpc.com/connect's own error_writer.go) does
// the protocol detection and wire encoding: it classifies the request from
// Content-Type/method exactly as a real connect.Handler would, and for the
// Connect protocol its JSON envelope and connectCodeToHTTP status mapping
// are byte-for-byte what a registered service's own error path emits --
// this is the same connect-go type a genuine client already knows how to
// decode, so there is no hand-rolled trailer or envelope encoding here.
func connectNotFoundHandler(group, path string) http.Handler {
	rpcErr := connect.NewError(connect.CodeNotFound, fmt.Errorf("no %s service registered for %s", group, path))
	errWriter := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = errWriter.Write(w, r, rpcErr)
	})
}

// gitNotFoundHandler answers any /git/* request that does not match a
// registered mirror handler with an ordinary git HTTP error
// (docs/git-spec.md: "Repo not enrolled -> 404 ... ordinary git HTTP
// errors"): plain text, never text/html, so it is never mistaken for the
// SPA. The real smart-HTTP handler (loam-ofg.16, blocked on this bead) owns
// the per-repository 404 once it exists; until then this is the only
// response any /git/* path produces.
func gitNotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 repository not found\n"))
	})
}
