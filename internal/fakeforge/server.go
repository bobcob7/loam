// Package fakeforge is an in-process test double for an upstream git forge
// (per docs/testing-spec.md "The Three Test Doubles" and docs/sync-spec.md
// "Provider Interface"/"Upstream Transport"). A Server is a single
// net/http.Handler exposing three surfaces: bare repos over token-
// authenticated smart HTTP, a small provider REST API mirroring the real
// forge's six operations, and a test-only control API for scripting
// upstream events. Each Server owns its own temp storage; nothing is shared
// between instances.
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
// HTTP, the provider REST surface, and the test control API. Construct with
// New and release resources with Close.
type Server struct {
	logger  *slog.Logger
	root    string
	gitPath string
	mux     *http.ServeMux
	tokMu   sync.Mutex
	tokens  map[string]bool // token -> read-only
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
		tokens:  make(map[string]bool),
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

// AddToken registers token as valid, with full read and write access, for
// both git smart-HTTP basic auth and the provider REST API's Authorization
// header.
func (s *Server) AddToken(token string) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	s.tokens[token] = false
}

// AddReadOnlyToken registers token as valid for reads (git-upload-pack,
// ValidateToken) but denied for writes (git-receive-pack, CheckRepo's write
// probe), mirroring a real forge token missing push scope so callers can
// exercise the read-ok-write-denied distinction from sync-spec's Upstream
// Transport section without a real forge.
func (s *Server) AddReadOnlyToken(token string) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	s.tokens[token] = true
}

// hasToken reports whether token was registered with AddToken or
// AddReadOnlyToken.
func (s *Server) hasToken(token string) bool {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	_, ok := s.tokens[token]
	return ok
}

// tokenReadOnly reports whether token was registered with AddReadOnlyToken.
// It assumes hasToken(token) is already true; an unregistered token reads as
// full access here since callers gate on hasToken first.
func (s *Server) tokenReadOnly(token string) bool {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	return s.tokens[token]
}

func (s *Server) newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(gitPathPrefix+"/", s.authenticatedGitHandler())
	mux.HandleFunc("POST /provider/validate-token", s.handleValidateToken)
	mux.HandleFunc("POST /provider/check-repo", s.handleCheckRepo)
	mux.HandleFunc("POST /provider/create-pr", s.handleCreatePR)
	mux.HandleFunc("POST /provider/pr-state", s.handleGetPRState)
	mux.HandleFunc("POST /provider/close-pr", s.handleProviderClosePR)
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
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
