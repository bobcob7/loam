package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// errNotInClone is the underlying cause when workspace inference cannot
// resolve a repo or work branch: the caller's current directory is not the
// root of a git clone (see docs/cli-spec.md -> Workspace). Command handlers
// never see this sentinel directly — resolveWorkBranchIdentity wraps it in a
// usage cliError (exit 2) unless an explicit argument covers the gap.
var errNotInClone = errors.New("not inside a repo directory")

// workspace implements WorkspaceResolver (see docs/cli-spec.md -> Workspace)
// from a directory snapshotted once at construction: it does not watch for
// the process's working directory changing mid-run. When dir is itself the
// root of a git clone, repo/workBranch are inferred and root is dir's
// parent (the workspace); otherwise dir is treated as the workspace root
// itself and inference always fails.
type workspace struct {
	root            string
	repo            string
	repoErr         error
	workBranch      string
	workBranchErr   error
	agentIdentifier string
}

// newWorkspace builds a workspace rooted at dir, using lookup to decide
// whether dir is a clone directory (see gitBranchLookup). agentIdentifier
// keys the staging path (see StagingPath) — normally cfg.Identifier().
func newWorkspace(dir, agentIdentifier string, lookup gitBranchLookup) *workspace {
	branch, err := lookup.CurrentBranch(dir)
	if err != nil {
		unresolved := fmt.Errorf("%s: %w: %w", dir, errNotInClone, err)
		return &workspace{root: dir, repoErr: unresolved, workBranchErr: unresolved, agentIdentifier: agentIdentifier}
	}
	return &workspace{
		root:            filepath.Dir(dir),
		repo:            filepath.Base(dir),
		workBranch:      branch,
		agentIdentifier: agentIdentifier,
	}
}

// NewWorkspaceResolver builds the real WorkspaceResolver from the process's
// current working directory and cfg's resolved agent identifier (see
// docs/cli-spec.md -> Workspace; Identifier keys the staging path).
func NewWorkspaceResolver(cfg Config) (WorkspaceResolver, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining working directory: %w", err)
	}
	return newWorkspace(dir, cfg.Identifier(), execGitBranchLookup{}), nil
}

// ResolveRepo returns the repo inferred from the clone directory name, or
// errNotInClone (wrapped) when the workspace was not built from a clone
// directory.
func (w *workspace) ResolveRepo() (string, error) {
	if w.repoErr != nil {
		return "", w.repoErr
	}
	return w.repo, nil
}

// ResolveWorkBranch returns the work branch inferred from the clone's
// current git branch, or errNotInClone (wrapped) under the same condition
// as ResolveRepo.
func (w *workspace) ResolveWorkBranch() (string, error) {
	if w.workBranchErr != nil {
		return "", w.workBranchErr
	}
	return w.workBranch, nil
}

// StagingPath returns the local staging path for repo/workBranch, keyed
// also by the calling agent's identifier, under the workspace root's
// .loam/ directory (see docs/cli-spec.md -> "Staging location").
func (w *workspace) StagingPath(repo, workBranch string) string {
	return filepath.Join(w.root, ".loam", "staging", repo, workBranch, w.agentIdentifier)
}

// execGitBranchLookup is the real gitBranchLookup, backed by the git binary
// on PATH. dir must be exactly the root of a git working copy (a dir/.git
// entry, file or directory, covering both a plain clone and a linked
// worktree) — a dir merely nested inside one does not count, matching
// docs/cli-spec.md's "inside a clone at /<repo_name>" (the repo root, not
// an arbitrary subdirectory).
type execGitBranchLookup struct{}

// CurrentBranch implements gitBranchLookup.
func (execGitBranchLookup) CurrentBranch(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", fmt.Errorf("%s is not a git working copy root: %w", dir, err)
	}
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolving current branch in %s: %w", dir, err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("resolving current branch in %s: no branch checked out", dir)
	}
	return branch, nil
}

// resolveWorkBranchIdentity resolves repo and work-branch identifiers for a
// command whose positional convention is "[repo] [work-branch]" (see
// docs/cli-spec.md -> Work Branches): an explicit positional argument
// always wins; an omitted one falls back to ws's inference. When neither an
// argument nor inference resolves an identifier, the result is a usage
// error (exit 2, per docs/cli-spec.md -> show).
func resolveWorkBranchIdentity(ws WorkspaceResolver, positional []string) (repo, workBranch string, err error) {
	repo, err = resolveIdentifier("repo", positionalAt(positional, 0), ws.ResolveRepo)
	if err != nil {
		return "", "", err
	}
	workBranch, err = resolveIdentifier("work branch", positionalAt(positional, 1), ws.ResolveWorkBranch)
	if err != nil {
		return "", "", err
	}
	return repo, workBranch, nil
}

// positionalAt returns positional[idx], or "" when idx is out of range —
// the "omitted" case for resolveIdentifier.
func positionalAt(positional []string, idx int) string {
	if idx < len(positional) {
		return positional[idx]
	}
	return ""
}

// resolveIdentifier returns explicit when it is non-empty; otherwise it
// falls back to infer, wrapping a failure as a usage error naming which
// identifier (label) could not be resolved.
func resolveIdentifier(label, explicit string, infer func() (string, error)) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	value, err := infer()
	if err != nil {
		return "", newUsageCLIError(fmt.Sprintf("cannot resolve %s: not inside a repo directory; pass it explicitly", label), err)
	}
	return value, nil
}
