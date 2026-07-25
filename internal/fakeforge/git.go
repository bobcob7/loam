package fakeforge

import (
	"net/http"
	"net/http/cgi"
	"strings"
)

// isReceivePackRequest reports whether r is part of a git push over smart
// HTTP: the pack-data POST to .../git-receive-pack, or the ref
// advertisement GET with ?service=git-receive-pack that precedes it.
func isReceivePackRequest(r *http.Request) bool {
	return strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
}

// authenticatedGitHandler serves bare repos over git smart HTTP
// (git-upload-pack and git-receive-pack, via "git http-backend" run as a
// CGI subprocess) gated by HTTP Basic auth. Per docs/sync-spec.md → Upstream
// Transport and Forgejo's convention, the token is the Basic password and
// any username is accepted; missing or invalid credentials fail the same
// way a real forge fails, with a 401 before the git process ever runs. A
// token registered read-only (AddReadOnlyToken) may fetch but is denied
// with a 403 on any push, mirroring the read-ok-write-denied distinction
// sync-spec's Upstream Transport section calls out for CheckRepo's
// receive-pack probe.
func (s *Server) authenticatedGitHandler() http.Handler {
	backend := &cgi.Handler{
		Path: s.gitPath,
		Args: []string{"http-backend"},
		Root: gitPathPrefix,
		Dir:  s.reposRoot(),
		Env:  []string{"GIT_PROJECT_ROOT=" + s.reposRoot(), "GIT_HTTP_EXPORT_ALL=1"},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.gitPath == "" {
			writeJSONError(w, http.StatusServiceUnavailable, errGitUnavailable)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || !s.hasToken(password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="fakeforge"`)
			writeJSONError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		if isReceivePackRequest(r) && s.tokenReadOnly(password) {
			writeJSONError(w, http.StatusForbidden, errNoWriteAccess)
			return
		}
		backend.ServeHTTP(w, r)
	})
}
