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
// resolve a repo or work branch: the caller's current directory is not
// inside a git clone at all (see docs/cli-spec.md -> Workspace). Command
// handlers never see this sentinel directly — resolveWorkBranchIdentity
// wraps it in a usage cliError (exit 2) unless an explicit argument covers
// the gap.
var errNotInClone = errors.New("not inside a repo directory")

// workspace implements WorkspaceResolver (see docs/cli-spec.md -> Workspace)
// from a directory snapshotted once at construction: it does not watch for
// the process's working directory changing mid-run. dir may be the clone
// root itself or any directory nested inside one (cli-spec: "run from
// inside a repo directory" is not limited to the root — an agent editing
// code is constantly in a subdirectory). root is always the clone's
// parent — the workspace root — never the clone directory itself, so
// StagingPath never lands inside a clone regardless of dir's depth. repo
// and workBranch resolve independently: an unparseable origin remote fails
// ResolveRepo without touching ResolveWorkBranch.
type workspace struct {
	root            string
	repo            string
	repoErr         error
	workBranch      string
	workBranchErr   error
	agentIdentifier string
}

// newWorkspace builds a workspace by locating dir's enclosing clone (if
// any) via lookup. agentIdentifier keys the staging path (see
// StagingPath) — normally cfg.Identifier().
func newWorkspace(dir, agentIdentifier string, lookup gitLookup) *workspace {
	cloneRoot, err := lookup.CloneRoot(dir)
	if err != nil {
		unresolved := fmt.Errorf("%s: %w: %w", dir, errNotInClone, err)
		return &workspace{root: dir, repoErr: unresolved, workBranchErr: unresolved, agentIdentifier: agentIdentifier}
	}
	repo, repoErr := inferRepo(cloneRoot, lookup)
	branch, branchErr := lookup.CurrentBranch(cloneRoot)
	if branchErr != nil {
		branchErr = fmt.Errorf("resolving work branch in %s: %w", cloneRoot, branchErr)
	}
	return &workspace{
		root:            filepath.Dir(cloneRoot),
		repo:            repo,
		repoErr:         repoErr,
		workBranch:      branch,
		workBranchErr:   branchErr,
		agentIdentifier: agentIdentifier,
	}
}

// inferRepo derives the enrolled repo identifier ("<group>/<repo_name>",
// see docs/cli-spec.md -> clone) from cloneRoot's origin remote, which
// `loam clone` guarantees points at
// <LOAM_SERVER_URL>/git/<group>/<repo_name>.git ("Clones the repo over
// smart HTTP from <LOAM_SERVER_URL>/git/<group>/<repo>.git"). It never
// falls back to the bare clone-directory name: that string is not an
// identifier any RPC accepts, and using it here would also split the
// staging key from an agent who instead passes the identifier explicitly
// (see StagingPath, which is keyed by whatever repo string a caller
// resolves with).
func inferRepo(cloneRoot string, lookup gitLookup) (string, error) {
	origin, err := lookup.OriginURL(cloneRoot)
	if err != nil {
		return "", fmt.Errorf("resolving repo from origin remote in %s: %w", cloneRoot, err)
	}
	repo, ok := repoFromOriginURL(origin)
	if !ok {
		return "", fmt.Errorf("origin remote %q in %s does not look like a Loam clone URL (expected .../git/<group>/<repo>.git)", origin, cloneRoot)
	}
	return repo, nil
}

// repoFromOriginURL extracts "<group>/<repo_name>" from a
// <LOAM_SERVER_URL>/git/<group>/<repo_name>.git origin URL (see
// docs/cli-spec.md -> clone). ok is false when origin does not contain the
// "/git/" marker loam clone always composes, or the remainder after it
// does not look like a "<group>/<repo_name>" pair.
func repoFromOriginURL(origin string) (repo string, ok bool) {
	const marker = "/git/"
	idx := strings.LastIndex(origin, marker)
	if idx < 0 {
		return "", false
	}
	repo = strings.TrimSuffix(origin[idx+len(marker):], ".git")
	group, name, hasSlash := strings.Cut(repo, "/")
	if !hasSlash || group == "" || name == "" {
		return "", false
	}
	return repo, true
}

// newWorkspaceResolver builds the real WorkspaceResolver from the process's
// current working directory and cfg's resolved agent identifier (see
// docs/cli-spec.md -> Workspace; Identifier keys the staging path).
// Unexported: only deps.go's NewProductionDeps and this package's own tests
// call it.
func newWorkspaceResolver(cfg Config) (WorkspaceResolver, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining working directory: %w", err)
	}
	return newWorkspace(dir, cfg.Identifier(), execGitLookup{}), nil
}

// ResolveRepo returns the repo inferred from the enclosing clone's origin
// remote, or an error (wrapping errNotInClone when there is no enclosing
// clone at all) otherwise.
func (w *workspace) ResolveRepo() (string, error) {
	if w.repoErr != nil {
		return "", w.repoErr
	}
	return w.repo, nil
}

// ResolveWorkBranch returns the work branch inferred from the enclosing
// clone's current git branch, or an error (wrapping errNotInClone when
// there is no enclosing clone at all) otherwise.
func (w *workspace) ResolveWorkBranch() (string, error) {
	if w.workBranchErr != nil {
		return "", w.workBranchErr
	}
	return w.workBranch, nil
}

// StagingPath returns the local staging path for repo/workBranch, keyed
// also by the calling agent's identifier, under the workspace root's
// .loam/ directory (see docs/cli-spec.md -> "Staging location") — the
// clone's parent, never inside the clone itself.
func (w *workspace) StagingPath(repo, workBranch string) string {
	return filepath.Join(w.root, ".loam", "staging", repo, workBranch, w.agentIdentifier)
}

// execGitLookup is the real gitLookup, backed by the git binary on PATH.
type execGitLookup struct{}

// CloneRoot implements gitLookup via `git rev-parse --show-toplevel`, which
// walks up from dir to find the enclosing working copy's root at any
// depth.
func (execGitLookup) CloneRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git working copy: %w", dir, err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("%s is not inside a git working copy: git reported an empty toplevel", dir)
	}
	return root, nil
}

// OriginURL implements gitLookup via `git remote get-url origin`.
func (execGitLookup) OriginURL(cloneRoot string) (string, error) {
	out, err := exec.Command("git", "-C", cloneRoot, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("reading origin remote in %s: %w", cloneRoot, err)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("origin remote in %s is empty", cloneRoot)
	}
	return url, nil
}

// CurrentBranch implements gitLookup via `git symbolic-ref --short HEAD`.
func (execGitLookup) CurrentBranch(cloneRoot string) (string, error) {
	out, err := exec.Command("git", "-C", cloneRoot, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("resolving current branch in %s: %w", cloneRoot, err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("resolving current branch in %s: no branch checked out", cloneRoot)
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
