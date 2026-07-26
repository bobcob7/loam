package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// RegisterSPA mounts the embedded admin single-page app as the catch-all
// for "everything else" in docs/web-spec.md -> Hosting & Routing, behind
// httpauth.Auth.AdminOnly. A request whose path resolves to a real file
// under distFS (e.g. "/assets/app.js") gets that file; any other GET/HEAD
// falls back to index.html so the SPA's client-side router can take over.
//
// This is registered as the mux's least-specific pattern ("/"), so any
// path already claimed by a more specific RegisterCLI/RegisterAdmin/
// RegisterGit registration (e.g. "/loam.v1.WorkBranchService/") is routed
// there first by http.ServeMux's longest-prefix-match rule and never
// reaches this handler at all — an unrecognized procedure *within* a
// registered service therefore still gets that service's own 404, not the
// SPA's index.html. RegisterSPA has no special-case logic for that; it
// falls out of registration order and mux prefix matching, the same
// "exemption is a routing fact" principle as the health endpoints.
//
// It panics if distFS has no index.html at the root — a fail-fast check
// on the composition root's wiring, matching RegisterCLI/RegisterAdmin/
// RegisterGit's panic-on-misuse contract.
func (rt *Router) RegisterSPA(distFS fs.FS) {
	if _, err := fs.Stat(distFS, "index.html"); err != nil {
		panic("server: RegisterSPA: embedded SPA filesystem has no index.html: " + err.Error())
	}
	rt.mux.Handle("/", rt.auth.AdminOnly(spaHandler(distFS)))
}

// spaHandler implements the file-or-index-fallback behavior described on
// RegisterSPA.
func spaHandler(distFS fs.FS) http.Handler {
	fileServer := http.FileServerFS(distFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if isRealFile(distFS, r.URL.Path) {
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, distFS)
	})
}

// isRealFile reports whether urlPath names an existing, non-directory
// entry in distFS, so spaHandler can distinguish a genuine static asset
// request from an unknown client-side route that must fall back to
// index.html.
func isRealFile(distFS fs.FS, urlPath string) bool {
	rel := strings.TrimPrefix(path.Clean(urlPath), "/")
	if rel == "" || rel == "." {
		return false
	}
	info, err := fs.Stat(distFS, rel)
	return err == nil && !info.IsDir()
}

// serveIndex writes distFS's index.html with a 200, the SPA fallback
// response for the app's root and every unknown client-side route.
func serveIndex(w http.ResponseWriter, distFS fs.FS) {
	data, err := fs.ReadFile(distFS, "index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
