// Package fakeforge is an in-process test double for an upstream git forge
// (per docs/testing-spec.md "The Three Test Doubles" and docs/sync-spec.md
// "Provider Interface"/"Upstream Transport"). A Server is a single
// net/http.Handler exposing five surfaces: bare repos over token-
// authenticated smart HTTP, a small provider REST API mirroring the real
// forge's six operations, one Forgejo-REST-shaped route for production
// callers holding a real *forge.Forgejo rather than a forge.Provider
// (forgejoapi.go), one GitHub-REST-shaped route for the same reason on
// *forge.GitHub's side (githubapi.go, loam-tmds.4), and a test-only
// control API for scripting upstream events. Each Server owns its own
// temp storage; nothing is shared between instances.
package fakeforge

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"log/slog"
)

const (
	gitPathPrefix  = "/git"
	gitCommitName  = "fakeforge-bot"
	gitCommitEmail = "fakeforge@example.invalid"
)

// Server is the fake forge: an http.Handler serving bare repos over smart
// HTTP, the provider REST surface, the Forgejo-shaped scope probe, and the
// test control API. Construct with New and release resources with Close.
type Server struct {
	logger  *slog.Logger
	root    string
	gitPath string
	mux     *http.ServeMux
	tokMu   sync.Mutex
	tokens  map[string]tokenScope
	prs     *prRegistry
	baseMu  sync.Mutex
	baseURL string
}

// New constructs a Server with its own temp storage. The git binary is
// resolved from PATH if available; if it is not, the git smart-HTTP surface
// responds with 503 while the provider REST and control API surfaces remain
// usable. Callers must call Close when done to remove the temp storage.
func New(logger *slog.Logger) (*Server, error) {
	root, err := os.MkdirTemp("", "fakeforge-*")
	if err != nil {
		return nil, fmt.Errorf("creating fakeforge storage: %w", err)
	}
	gitPath, _ := exec.LookPath("git")
	s := &Server{
		logger:  logger,
		root:    root,
		gitPath: gitPath,
		tokens:  make(map[string]tokenScope),
		prs:     newPRRegistry(),
	}
	s.mux = s.newMux()
	return s, nil
}

// Close removes the Server's temp storage. It is safe to call once.
func (s *Server) Close() error {
	if err := os.RemoveAll(s.root); err != nil {
		return fmt.Errorf("removing fakeforge storage %s: %w", s.root, err)
	}
	return nil
}

// ServeHTTP implements http.Handler, so a Server can be passed directly to
// httptest.NewServer.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// SetBaseURL records the externally reachable base URL (typically an
// httptest.Server's URL) so GitURL can build clone URLs for callers. It is
// optional; the provider REST surface derives PR URLs from the request's
// Host header instead.
func (s *Server) SetBaseURL(baseURL string) {
	s.baseMu.Lock()
	defer s.baseMu.Unlock()
	s.baseURL = strings.TrimRight(baseURL, "/")
}

// GitURL returns the smart-HTTP clone/push URL for repoName, relative to
// the base URL set with SetBaseURL. It panics if SetBaseURL was never
// called, since the result would otherwise be meaningless.
func (s *Server) GitURL(repoName string) string {
	s.baseMu.Lock()
	defer s.baseMu.Unlock()
	if s.baseURL == "" {
		panic("fakeforge: GitURL called before SetBaseURL")
	}
	return fmt.Sprintf("%s%s/%s.git", s.baseURL, gitPathPrefix, strings.Trim(repoName, "/"))
}

// tokenScope models a real forge token's scope as a SINGLE axis: whether
// it carries write:repository. Earlier this modeled push (git) and
// PR-opening (REST) as two independent bools -- loam-2uy found, verified
// live against Forgejo 9.0.3, that they are not independent there. Git
// push over HTTPS (the git-receive-pack ref advertisement) and
// POST .../pulls are gated on the identical write:repository scope: a
// read:repository token gets 403 on both, not one. There is no scope
// configuration that grants one and withholds the other, so a token
// either has write:repository (full push AND PR-opening) or it doesn't
// (neither).
type tokenScope struct {
	canWrite bool
}

// AddToken registers token as valid, with write:repository scope: full
// push and PR-opening access, for both git smart-HTTP basic auth and the
// provider REST API's Authorization header.
func (s *Server) AddToken(token string) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	s.tokens[token] = tokenScope{canWrite: true}
}

// AddReadOnlyToken registers token as valid for reads (git-upload-pack)
// but lacking write:repository scope, so it is denied for BOTH writes
// (git-receive-pack, CheckRepo's write probe) AND PR-opening
// (ValidateToken, the provider REST surface) -- mirroring a real
// read:repository-scoped forge token (loam-2uy), which fails identically
// on both surfaces rather than succeeding on one and failing the other.
func (s *Server) AddReadOnlyToken(token string) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	s.tokens[token] = tokenScope{canWrite: false}
}

// hasToken reports whether token was registered with any of the Add*Token
// constructors.
func (s *Server) hasToken(token string) bool {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	_, ok := s.tokens[token]
	return ok
}

// tokenReadOnly reports whether token is registered without write scope. It
// assumes hasToken(token) is already true; an unregistered token reads as
// full access here since callers gate on hasToken first.
func (s *Server) tokenReadOnly(token string) bool {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	return !s.tokens[token].canWrite
}

// tokenHasPRScope reports whether token is registered with PR-opening
// scope. It assumes hasToken(token) is already true. This is now the same
// underlying bit tokenReadOnly reads (see tokenScope's doc comment): kept
// as a separate, named predicate because callers (handleValidateToken,
// requireForgejoWriteScope) each read it for a distinct REST surface, not
// because it is a distinct axis.
func (s *Server) tokenHasPRScope(token string) bool {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	return s.tokens[token].canWrite
}

func (s *Server) newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(gitPathPrefix+"/", s.authenticatedGitHandler())
	// The Forgejo-REST-shaped PR-lifecycle routes, consumed by the REAL
	// *forge.Forgejo rather than by *fakeforge.Client — see forgejoapi.go
	// for why both surfaces exist and exactly how much of Forgejo's own
	// pulls API this one models.
	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/pulls", s.handleForgejoCreatePull)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/pulls", s.handleForgejoListPulls)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/pulls/{index}", s.handleForgejoGetPull)
	mux.HandleFunc("PATCH /api/v1/repos/{owner}/{repo}/pulls/{index}", s.handleForgejoPatchPull)
	// The GitHub-REST-shaped surface, consumed by the REAL *forge.GitHub
	// rather than by *fakeforge.Client — see githubapi.go. No version
	// prefix, matching GitHub's own paths (unlike Forgejo's /api/v1/...
	// above), so there is no path collision between the two dialects.
	mux.HandleFunc("GET /user", s.handleGitHubUser)
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", s.handleGitHubCreatePull)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", s.handleGitHubListPulls)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{index}", s.handleGitHubGetPull)
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/pulls/{index}", s.handleGitHubPatchPull)
	mux.HandleFunc("POST /provider/validate-token", s.handleValidateToken)
	mux.HandleFunc("POST /provider/create-pr", s.handleCreatePR)
	mux.HandleFunc("POST /provider/pr-state", s.handleGetPRState)
	mux.HandleFunc("POST /provider/close-pr", s.handleProviderClosePR)
	mux.HandleFunc("POST /provider/find-open-pr", s.handleFindOpenPR)
	mux.HandleFunc("POST /control/advance-branch", s.handleControlAdvanceBranch)
	mux.HandleFunc("POST /control/force-push-branch", s.handleControlForcePushBranch)
	mux.HandleFunc("POST /control/delete-branch", s.handleControlDeleteBranch)
	mux.HandleFunc("POST /control/create-branch", s.handleControlCreateBranch)
	mux.HandleFunc("POST /control/merge-pr", s.handleControlMergePR)
	mux.HandleFunc("POST /control/close-pr", s.handleControlClosePR)
	return mux
}

// reposRoot is the directory under which every seeded bare repo lives.
func (s *Server) reposRoot() string {
	return filepath.Join(s.root, "repos")
}

// workRoot is scratch space for ephemeral working clones used by seeding
// and the control API's history-mutating operations.
func (s *Server) workRoot() string {
	return filepath.Join(s.root, "work")
}

// repoDir resolves repoName (e.g. "acme/widgets", with or without a
// trailing ".git") to the bare repo's absolute path on disk.
func (s *Server) repoDir(repoName string) string {
	name := strings.Trim(repoName, "/")
	name = strings.TrimSuffix(name, ".git")
	return filepath.Join(s.reposRoot(), name+".git")
}

// requireRepo returns errRepoNotFound if repoDir does not exist on disk.
func (s *Server) requireRepo(repoDir string) error {
	if _, err := os.Stat(repoDir); err != nil {
		return errRepoNotFound
	}
	return nil
}

// requireBranch returns errBranchNotFound if branch does not exist in the
// bare repo at repoDir.
func (s *Server) requireBranch(ctx context.Context, repoDir, branch string) error {
	if _, err := s.runGit(ctx, "", "--git-dir="+repoDir, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
		return errBranchNotFound
	}
	return nil
}

// runGit executes git with args. When dir is empty the command runs with
// the process's own working directory (used for direct bare-repo plumbing
// via an explicit "--git-dir=" argument); otherwise dir is used as the
// working directory (used for ephemeral working clones).
func (s *Server) runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if s.gitPath == "" {
		return nil, errGitUnavailable
	}
	cmd := exec.CommandContext(ctx, s.gitPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+gitCommitName, "GIT_AUTHOR_EMAIL="+gitCommitEmail,
		"GIT_COMMITTER_NAME="+gitCommitName, "GIT_COMMITTER_EMAIL="+gitCommitEmail,
		"GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Warn("fakeforge: git subprocess failed", "args", args, "error", err, "output", strings.TrimSpace(string(out)))
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
