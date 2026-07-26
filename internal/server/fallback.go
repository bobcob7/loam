package server

import (
	"encoding/json"
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

// connectWireErrorBody mirrors the Connect protocol's unary error envelope
// (connectrpc.com/connect's unexported connectWireError, same field
// names/tags) so a hand-written 404 for an unregistered service is
// byte-for-byte the shape a connect-go client already knows how to
// unmarshal -- never an html catch-all page.
type connectWireErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// connectNotFoundHandler answers any request under group (a registered
// service was not matched by mux) with a Connect CodeNotFound envelope:
// JSON body, Content-Type application/json, HTTP status per the Connect
// protocol's code-to-HTTP mapping (404 for CodeNotFound). This applies to
// both GET and POST alike -- there is no method guard here, unlike
// spaHandler's, since every request under an RPC group prefix is an API
// request, never a browser navigation.
func connectNotFoundHandler(group, path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codeText, _ := connect.CodeNotFound.MarshalText()
		body, err := json.Marshal(connectWireErrorBody{
			Code:    string(codeText),
			Message: fmt.Sprintf("no %s service registered for %s", group, path),
		})
		if err != nil {
			body = []byte(`{"code":"not_found"}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
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
