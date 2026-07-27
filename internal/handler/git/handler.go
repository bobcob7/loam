package git

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
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

// repoSegmentPattern is the same allowlist loam-hs5 settled for a
// filesystem-joined identifier segment: it must start with an
// alphanumeric and contain only alphanumerics, '.', '_', or '-'. That
// makes '.', '..', and empty segments impossible BY CONSTRUCTION, rather
// than relying on a blacklist of known-bad substrings -- the same
// validate-then-reject shape loam-hs5's StagingPath guard uses, applied
// here because r.URL.Path is already percent-decoded by net/http (a
// request path containing "..%2f" arrives as a literal ".." path
// segment), so repoName is untrusted, attacker-controlled input joined
// straight into a filesystem path by mirrorpath.Dir.
var repoSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validRepoName reports whether repoName is exactly two '/'-delimited
// segments (docs/persistence-spec.md's "<group>/<repo_name>" convention),
// each matching repoSegmentPattern. It REJECTS anything else -- including
// a traversal segment like ".." or an empty segment from a doubled slash
// -- rather than sanitizing it, matching loam-hs5's settled reasoning:
// silently rewriting an invalid identifier into some other path the
// caller cannot predict is worse than a clean 404.
func validRepoName(repoName string) bool {
	segments := strings.Split(repoName, "/")
	if len(segments) != 2 {
		return false
	}
	for _, segment := range segments {
		if !repoSegmentPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

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
	// repo.Name (the store's trusted value), not req.repoName (the raw,
	// URL-derived string validRepoName merely rejected-if-malformed) is
	// what serveRPC must use for LOAM_REPO: loam-ofg.18's pre-receive hook
	// keys its policy decision on that value, so it must agree with
	// mirrorDir above about which string is trusted, never the
	// attacker-influenced request path.
	h.serveRPC(w, r, repo.Name, mirrorDir, req.service)
}

// parseGitRequest matches r against the three request shapes docs/git-
// spec.md "Endpoint & Protocol" defines under gitPathPrefix
// ("/git/<group>/<repo_name>.git/info/refs?service=...",
// ".../git-upload-pack", ".../git-receive-pack"), extracting the repo name
// as everything between the prefix and the first ".git/" segment boundary
// and validating it against validRepoName BEFORE returning it to the
// caller. ok is false for anything else, including an unrecognized
// ?service= value on info/refs -- dumb HTTP is not served (docs/git-
// spec.md) -- and including a repoName that fails validRepoName, so a
// path-traversal segment (e.g. r.URL.Path decoding "..%2f" into a literal
// ".." segment) is rejected here, before this handler ever calls the repo
// store or joins repoName into a filesystem path.
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
	if !validRepoName(repoName) {
		return gitRequest{}, false
	}
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
// serves elsewhere), the same content type internal/server's own
// gitNotFoundHandler fallback uses, though the two bodies are
// deliberately DIFFERENT fixed strings ("loam: repository not found\n"
// here vs. "404 repository not found\n" in fallback.go): this handler's
// own tests (TestBuildRouter_NilPool_GitStillHitsGroupFallback,
// cmd/server/registration_test.go) rely on that difference to prove a
// request reached this handler rather than the group-level fallback that
// runs when RegisterGit was never called at all.
func writeGitNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("loam: repository not found\n"))
}
