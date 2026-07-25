package fakeforge

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AdvanceOptions configures AdvanceBranch and ForcePushBranch's synthesized
// commit when the caller does not need control over its exact content.
type AdvanceOptions struct {
	Path    string // file to write; defaults to "ADVANCE.txt"
	Content []byte // file content; defaults to a timestamped line
	Message string // commit message; defaults to "advance <branch>"
}

// ForcePushOptions configures ForcePushBranch. If ToRef is set, branch is
// force-updated to that ref/sha verbatim (any content it already carries).
// If ToRef is empty, a synthetic commit diverging from the branch's current
// history is generated and force-pushed, simulating an amend/rebase.
type ForcePushOptions struct {
	ToRef   string
	Path    string
	Content []byte
	Message string
}

// AdvanceBranch simulates an ordinary upstream push: it appends one commit
// to branch's current tip. repo must already contain branch.
func (s *Server) AdvanceBranch(ctx context.Context, repo, branch string, opts AdvanceOptions) error {
	repoDir := s.repoDir(repo)
	if err := s.requireRepo(repoDir); err != nil {
		return fmt.Errorf("advancing %s/%s: %w", repo, branch, err)
	}
	if err := s.requireBranch(ctx, repoDir, branch); err != nil {
		return fmt.Errorf("advancing %s/%s: %w", repo, branch, err)
	}
	tmp, err := s.newWorkingClone(ctx, repoDir)
	if err != nil {
		return fmt.Errorf("advancing %s/%s: %w", repo, branch, err)
	}
	defer os.RemoveAll(tmp)
	if _, err := s.runGit(ctx, tmp, "checkout", branch); err != nil {
		return fmt.Errorf("advancing %s/%s: checkout: %w", repo, branch, err)
	}
	message := opts.Message
	if message == "" {
		message = "advance " + branch
	}
	if err := s.writeAndCommit(ctx, tmp, opts.Path, opts.Content, message); err != nil {
		return fmt.Errorf("advancing %s/%s: %w", repo, branch, err)
	}
	if _, err := s.runGit(ctx, tmp, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("advancing %s/%s: pushing: %w", repo, branch, err)
	}
	return nil
}

// ForcePushBranch simulates a rewritten upstream history: branch is reset
// to opts.ToRef (if set) or to a synthesized divergent commit, and pushed
// with --force. repo must already contain branch.
func (s *Server) ForcePushBranch(ctx context.Context, repo, branch string, opts ForcePushOptions) error {
	repoDir := s.repoDir(repo)
	if err := s.requireRepo(repoDir); err != nil {
		return fmt.Errorf("force-pushing %s/%s: %w", repo, branch, err)
	}
	if err := s.requireBranch(ctx, repoDir, branch); err != nil {
		return fmt.Errorf("force-pushing %s/%s: %w", repo, branch, err)
	}
	tmp, err := s.newWorkingClone(ctx, repoDir)
	if err != nil {
		return fmt.Errorf("force-pushing %s/%s: %w", repo, branch, err)
	}
	defer os.RemoveAll(tmp)
	target := opts.ToRef
	if target == "" {
		out, err := s.runGit(ctx, tmp, "rev-parse", "refs/remotes/origin/"+branch+"^")
		if err != nil {
			return fmt.Errorf("force-pushing %s/%s: resolving base: %w", repo, branch, err)
		}
		base := strings.TrimSpace(string(out))
		if _, err := s.runGit(ctx, tmp, "checkout", "--detach", base); err != nil {
			return fmt.Errorf("force-pushing %s/%s: checkout base: %w", repo, branch, err)
		}
		message := opts.Message
		if message == "" {
			message = "force-push " + branch
		}
		if err := s.writeAndCommit(ctx, tmp, opts.Path, opts.Content, message); err != nil {
			return fmt.Errorf("force-pushing %s/%s: %w", repo, branch, err)
		}
		target = "HEAD"
	}
	if _, err := s.runGit(ctx, tmp, "push", "--force", "origin", target+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("force-pushing %s/%s: pushing: %w", repo, branch, err)
	}
	return nil
}

// DeleteBranch simulates an upstream branch deletion.
func (s *Server) DeleteBranch(ctx context.Context, repo, branch string) error {
	repoDir := s.repoDir(repo)
	if err := s.requireRepo(repoDir); err != nil {
		return fmt.Errorf("deleting branch %s/%s: %w", repo, branch, err)
	}
	if _, err := s.runGit(ctx, "", "--git-dir="+repoDir, "update-ref", "-d", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("deleting branch %s/%s: %w", repo, branch, err)
	}
	return nil
}

// CreateCollidingBranch creates a branch named name (which must have the
// "wb-" prefix Loam uses for its own work branches) at fromRef, or at the
// repo's default branch tip if fromRef is empty. This simulates the forge
// independently growing a branch whose name collides with one of Loam's.
func (s *Server) CreateCollidingBranch(ctx context.Context, repo, name, fromRef string) error {
	if !strings.HasPrefix(name, "wb-") {
		return fmt.Errorf("creating branch %s/%s: %w", repo, name, errInvalidBranch)
	}
	repoDir := s.repoDir(repo)
	if err := s.requireRepo(repoDir); err != nil {
		return fmt.Errorf("creating branch %s/%s: %w", repo, name, err)
	}
	ref := fromRef
	if ref == "" {
		out, err := s.runGit(ctx, "", "--git-dir="+repoDir, "symbolic-ref", "HEAD")
		if err != nil {
			return fmt.Errorf("creating branch %s/%s: resolving default branch: %w", repo, name, err)
		}
		ref = strings.TrimSpace(string(out))
	}
	out, err := s.runGit(ctx, "", "--git-dir="+repoDir, "rev-parse", ref)
	if err != nil {
		return fmt.Errorf("creating branch %s/%s: resolving %s: %w", repo, name, ref, err)
	}
	sha := strings.TrimSpace(string(out))
	if _, err := s.runGit(ctx, "", "--git-dir="+repoDir, "update-ref", "refs/heads/"+name, sha); err != nil {
		return fmt.Errorf("creating branch %s/%s: %w", repo, name, err)
	}
	return nil
}

// MergePR merges a recorded PR's head branch into its target branch
// (fast-forward if possible, otherwise a real three-way merge via
// "git merge-tree", never touching a working tree) and marks it merged.
// This is the forge-side event a real merge produces; Loam's own sync
// observes the resulting target-branch advance independently.
func (s *Server) MergePR(ctx context.Context, repo string, number int) error {
	pr, err := s.lookupPR(repo, number)
	if err != nil {
		return err
	}
	repoDir := s.repoDir(repo)
	targetRef, headRef := "refs/heads/"+pr.targetBranch, "refs/heads/"+pr.headBranch
	targetSHA, err := s.resolveRef(ctx, repoDir, targetRef)
	if err != nil {
		return fmt.Errorf("merging pr %s#%d: resolving target: %w", repo, number, err)
	}
	headSHA, err := s.resolveRef(ctx, repoDir, headRef)
	if err != nil {
		return fmt.Errorf("merging pr %s#%d: resolving head: %w", repo, number, err)
	}
	if _, err := s.runGit(ctx, "", "--git-dir="+repoDir, "merge-base", "--is-ancestor", targetSHA, headSHA); err == nil {
		if _, err := s.runGit(ctx, "", "--git-dir="+repoDir, "update-ref", targetRef, headSHA, targetSHA); err != nil {
			return fmt.Errorf("merging pr %s#%d: fast-forwarding: %w", repo, number, err)
		}
		s.prs.setState(repo, number, "merged")
		return nil
	}
	treeOut, err := s.runGit(ctx, "", "--git-dir="+repoDir, "merge-tree", "--write-tree", targetSHA, headSHA)
	if err != nil {
		return fmt.Errorf("merging pr %s#%d: %w", repo, number, errMergeConflict)
	}
	treeSHA := strings.TrimSpace(strings.SplitN(string(treeOut), "\n", 2)[0])
	msg := fmt.Sprintf("Merge pull request #%d from %s into %s", number, pr.headBranch, pr.targetBranch)
	commitOut, err := s.runGit(ctx, "", "--git-dir="+repoDir, "commit-tree", treeSHA, "-p", targetSHA, "-p", headSHA, "-m", msg)
	if err != nil {
		return fmt.Errorf("merging pr %s#%d: creating merge commit: %w", repo, number, err)
	}
	commitSHA := strings.TrimSpace(string(commitOut))
	if _, err := s.runGit(ctx, "", "--git-dir="+repoDir, "update-ref", targetRef, commitSHA, targetSHA); err != nil {
		return fmt.Errorf("merging pr %s#%d: updating target: %w", repo, number, err)
	}
	s.prs.setState(repo, number, "merged")
	return nil
}

// ClosePR marks a recorded PR closed without merging, simulating someone
// closing it directly on the forge (as opposed to Client.ClosePR, which is
// Loam asking the forge to close a PR it opened).
func (s *Server) ClosePR(_ context.Context, repo string, number int) error {
	if _, err := s.lookupPR(repo, number); err != nil {
		return err
	}
	s.prs.setState(repo, number, "closed")
	return nil
}

func (s *Server) lookupPR(repo string, number int) (*prRecord, error) {
	pr, ok := s.prs.get(repo, number)
	if !ok {
		return nil, fmt.Errorf("pr %s#%d: %w", repo, number, errPRNotFound)
	}
	return pr, nil
}

func (s *Server) resolveRef(ctx context.Context, repoDir, ref string) (string, error) {
	out, err := s.runGit(ctx, "", "--git-dir="+repoDir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// newWorkingClone clones repoDir (all refs, not just one branch, so
// ForcePushBranch can resolve any ref within it) into a fresh scratch
// directory under the Server's work root. The caller must remove it.
func (s *Server) newWorkingClone(ctx context.Context, repoDir string) (string, error) {
	if err := os.MkdirAll(s.workRoot(), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(s.workRoot(), "ctl-*")
	if err != nil {
		return "", err
	}
	if _, err := s.runGit(ctx, "", "clone", repoDir, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("cloning: %w", err)
	}
	return tmp, nil
}

// writeAndCommit writes path (default "ADVANCE.txt") with content (default
// a timestamped line) into the working clone at dir and commits it.
func (s *Server) writeAndCommit(ctx context.Context, dir, path string, content []byte, message string) error {
	if path == "" {
		path = "ADVANCE.txt"
	}
	if content == nil {
		content = []byte("fakeforge event at " + time.Now().UTC().Format(time.RFC3339Nano) + "\n")
	}
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return err
	}
	if _, err := s.runGit(ctx, dir, "add", "-A"); err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	if _, err := s.runGit(ctx, dir, "commit", "-m", message); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

type advanceBranchRequest struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
}

type forcePushBranchRequest struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	ToRef   string `json:"to_ref,omitempty"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Message string `json:"message,omitempty"`
}

type branchRequest struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

type createBranchRequest struct {
	Repo string `json:"repo"`
	Name string `json:"name"`
	From string `json:"from,omitempty"`
}

type prActionRequest struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func contentBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func (s *Server) handleControlAdvanceBranch(w http.ResponseWriter, r *http.Request) {
	var req advanceBranchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	opts := AdvanceOptions{Path: req.Path, Content: contentBytes(req.Content), Message: req.Message}
	if err := s.AdvanceBranch(r.Context(), req.Repo, req.Branch, opts); err != nil {
		writeJSONError(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleControlForcePushBranch(w http.ResponseWriter, r *http.Request) {
	var req forcePushBranchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	opts := ForcePushOptions{ToRef: req.ToRef, Path: req.Path, Content: contentBytes(req.Content), Message: req.Message}
	if err := s.ForcePushBranch(r.Context(), req.Repo, req.Branch, opts); err != nil {
		writeJSONError(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleControlDeleteBranch(w http.ResponseWriter, r *http.Request) {
	var req branchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.DeleteBranch(r.Context(), req.Repo, req.Branch); err != nil {
		writeJSONError(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleControlCreateBranch(w http.ResponseWriter, r *http.Request) {
	var req createBranchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.CreateCollidingBranch(r.Context(), req.Repo, req.Name, req.From); err != nil {
		writeJSONError(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleControlMergePR(w http.ResponseWriter, r *http.Request) {
	var req prActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.MergePR(r.Context(), req.Repo, req.Number); err != nil {
		writeJSONError(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleControlClosePR(w http.ResponseWriter, r *http.Request) {
	var req prActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.ClosePR(r.Context(), req.Repo, req.Number); err != nil {
		writeJSONError(w, statusForErr(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// statusForErr maps a fake forge sentinel to the HTTP status code its
// wire representation should carry; unrecognized errors are a 500.
func statusForErr(err error) int {
	switch codeForError(err) {
	case "repo_not_found", "branch_not_found", "pr_not_found":
		return http.StatusNotFound
	case "repo_exists":
		return http.StatusConflict
	case "invalid_branch", "invalid_upstream":
		return http.StatusBadRequest
	case "unauthorized":
		return http.StatusUnauthorized
	case "no_write_access":
		return http.StatusForbidden
	case "merge_conflict":
		return http.StatusConflict
	case "git_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
