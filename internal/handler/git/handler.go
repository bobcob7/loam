package git

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/reposstore"
)

// The two smart-HTTP service names the git protocol defines (mirrored,
// necessarily, from internal/handler.GitRoleGate's own unexported
// constants of the same name and value -- these are fixed by git's wire
// protocol, not a Loam convention, so duplicating the two string literals
// across the gate and this handler is not "a third spelling" of anything;
// there is nothing here for the two packages to share without exporting
// them purely for this, which docs/git-spec.md gives no other reason to
// do).
const (
	serviceUploadPack  = "git-upload-pack"
	serviceReceivePack = "git-receive-pack"
)

// gitPathPrefix is the mount point this handler is registered under
// (docs/git-spec.md "Endpoint & Protocol": "/git/<group>/<repo_name>.git").
const gitPathPrefix = "/git/"

// Handler serves the smart-HTTP transport for every enrolled repo's bare
// mirror under dataDir (docs/git-spec.md "Endpoint & Protocol"). Construct
// exactly one from New in the composition root; it holds no per-request
// state.
type Handler struct {
	dataDir string
	repos   RepoStore
	logger  *slog.Logger
}

// New builds a Handler rooted at dataDir (LOAM_DATA_DIR), resolving
// enrollment through repos (in production, *reposstore.Store) and logging
// subprocess failures through logger.
func New(dataDir string, repos RepoStore, logger *slog.Logger) *Handler {
	return &Handler{dataDir: dataDir, repos: repos, logger: logger}
}

// gitRequest is one parsed, in-vocabulary /git/* request: the repo name
// docs/persistence-spec.md's repos.name convention holds
// ("<group>/<repo_name>"), the service the request names, and whether it
// is the GET info/refs discovery request (as opposed to one of the two
// POST RPC endpoints).
type gitRequest struct {
	repoName   string
	service    string
	isInfoRefs bool
}

// ServeHTTP resolves the request shape and the repo, then dispatches to
// the ref-advertisement or RPC-streaming path. Any request outside the
// three shapes docs/git-spec.md "Endpoint & Protocol" defines, or naming a
// repo that is not enrolled, gets a 404 (docs/git-spec.md: "Repo not
// enrolled -> 404"). Per loam-ofg.17's review NOTES, most malformed /git/*
// shapes never reach here at all -- internal/handler.GitRoleGate, wrapping
// this handler in the mux, already answers them with a fail-closed 403
// because it cannot resolve a required capability for them. This 404
// path stays as the handler's own defense in depth (it is what a unit
// test exercising Handler directly, without the gate, sees) and as the
// genuine "enrolled shape, unenrolled repo" case the gate cannot decide
// -- it has no repo store of its own.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, ok := parseGitRequest(r)
	if !ok {
		writeGitNotFound(w)
		return
	}
	repo, err := h.repos.GetRepoByName(r.Context(), req.repoName)
	if err != nil {
		if errors.Is(err, reposstore.ErrNotFound) {
			writeGitNotFound(w)
			return
		}
		h.logger.ErrorContext(r.Context(), "git handler: resolving repo", "repo", req.repoName, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	mirrorDir := mirrorpath.Dir(h.dataDir, repo.Name)
	if req.isInfoRefs {
		h.serveInfoRefs(w, r, mirrorDir, req.service)
		return
	}
	h.serveRPC(w, r, req.repoName, mirrorDir, req.service)
}

// parseGitRequest matches r against the three request shapes docs/git-
// spec.md "Endpoint & Protocol" defines under gitPathPrefix
// ("/git/<group>/<repo_name>.git/info/refs?service=...",
// ".../git-upload-pack", ".../git-receive-pack"), extracting the repo name
// as everything between the prefix and the first ".git/" segment boundary.
// ok is false for anything else, including an unrecognized ?service= value
// on info/refs -- dumb HTTP is not served (docs/git-spec.md), and there is
// no repo name to look up without a ".git/" boundary at all.
func parseGitRequest(r *http.Request) (gitRequest, bool) {
	if !strings.HasPrefix(r.URL.Path, gitPathPrefix) {
		return gitRequest{}, false
	}
	rest := strings.TrimPrefix(r.URL.Path, gitPathPrefix)
	idx := strings.Index(rest, ".git/")
	if idx <= 0 {
		return gitRequest{}, false
	}
	repoName := rest[:idx]
	suffix := rest[idx+len(".git/"):]
	switch {
	case r.Method == http.MethodGet && suffix == "info/refs":
		service := r.URL.Query().Get("service")
		if service != serviceUploadPack && service != serviceReceivePack {
			return gitRequest{}, false
		}
		return gitRequest{repoName: repoName, service: service, isInfoRefs: true}, true
	case r.Method == http.MethodPost && suffix == serviceUploadPack:
		return gitRequest{repoName: repoName, service: serviceUploadPack}, true
	case r.Method == http.MethodPost && suffix == serviceReceivePack:
		return gitRequest{repoName: repoName, service: serviceReceivePack}, true
	default:
		return gitRequest{}, false
	}
}

// writeGitNotFound answers an unrecognized shape or an unenrolled repo
// with a plain-text 404 -- text/plain (not the JSON/html this process
// serves elsewhere), matching internal/server's own gitNotFoundHandler
// fallback so every /git/* 404 looks the same on the wire regardless of
// which layer produced it.
func writeGitNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("loam: repository not found\n"))
}
