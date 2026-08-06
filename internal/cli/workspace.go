package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// errInvalidStagingKey is the underlying cause when a repo or work-branch
// key given to StagingPath is not safe to join onto the staging root: it
// contains characters outside the allowed class, an empty or "."/".."
// segment, a segment exceeding maxStagingKeySegmentLength, or (workBranch
// only) an interior separator. Command handlers see this wrapped in a
// usage cliError (exit 2) via StagingPath's caller.
var errInvalidStagingKey = errors.New("invalid staging key")

// maxStagingKeySegmentLength bounds a single "/"-delimited segment of a
// staging key, matching the common filesystem NAME_MAX (255 bytes) so an
// oversized key fails fast with a clear usage error instead of an opaque
// OS error once StagingPath's result is used to create a directory.
const maxStagingKeySegmentLength = 255

// stagingKeySegmentPattern is the allowed character class for one
// "/"-delimited segment of a staging key: it must start with a letter or
// digit (never ".", "-", or "_"), which by construction rules out an empty
// segment, a bare "." or ".." segment, and any segment beginning with a
// separator-adjacent character — no blacklist of specific traversal
// strings is needed because those shapes simply cannot match.
var stagingKeySegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateStagingKey checks that key is safe to use as one or more path
// segments under the staging root. label names the key in error messages
// ("repo" or "work branch"). allowNested permits key to contain "/"
// (repo's legitimate "<group>/<repo_name>" shape, per docs/cli-spec.md ->
// clone); workBranch never legitimately contains one, so allowNested is
// false there and any "/" is rejected outright rather than parsed as
// segments. Every segment (or the whole key, when allowNested is false)
// must match stagingKeySegmentPattern and not exceed
// maxStagingKeySegmentLength. An invalid key is always an error, never
// sanitized: silently rewriting attacker input would hide the attempt and
// could collide two distinct keys onto the same path.
func validateStagingKey(label, key string, allowNested bool) error {
	if key == "" {
		return fmt.Errorf("%s key %w: empty", label, errInvalidStagingKey)
	}
	if !allowNested && strings.Contains(key, "/") {
		return fmt.Errorf("%s key %w: %q must not contain %q", label, errInvalidStagingKey, key, "/")
	}
	for _, segment := range strings.Split(key, "/") {
		if len(segment) > maxStagingKeySegmentLength {
			return fmt.Errorf("%s key %w: a segment of %q exceeds %d characters", label, errInvalidStagingKey, key, maxStagingKeySegmentLength)
		}
		if !stagingKeySegmentPattern.MatchString(segment) {
			return fmt.Errorf("%s key %w: %q must consist of %q-delimited segments starting with a letter or digit, followed by letters, digits, %q, %q, or %q", label, errInvalidStagingKey, key, "/", ".", "_", "-")
		}
	}
	return nil
}

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
	legacyRoot      string
}

// newWorkspace builds a workspace by locating dir's enclosing clone (if
// any) via lookup. agentIdentifier keys the staging path (see
// stagingRel) — normally cfg.Identifier().
//
// root — the directory holding .loam/ — is passed IN rather than derived
// from dir, and that is the whole point (loam-rgyg). It used to be
// computed here as the enclosing clone's parent, falling back to dir
// itself when there was no enclosing clone, which made the staging area a
// function of the caller's working directory: two invocations one `cd`
// apart could address two disjoint staging areas under the same repo /
// work-branch / agent key, each seeing only its own staged comments. That
// is a data-loss shape on a workflow whose final step (`work verdict`) is
// irreversible — a verdict issued from the wrong directory published none
// of the reviewer's findings and reported `"published": 0` as a success.
// Repo and work-branch INFERENCE still depends on dir, correctly: it
// answers "which branch am I looking at", a question about where you are.
// Where the staged bytes live is not.
func newWorkspace(dir, agentIdentifier, root string, lookup gitLookup) *workspace {
	cloneRoot, err := lookup.CloneRoot(dir)
	if err != nil {
		unresolved := fmt.Errorf("%s: %w: %w", dir, errNotInClone, err)
		return &workspace{root: root, legacyRoot: dir, repoErr: unresolved, workBranchErr: unresolved, agentIdentifier: agentIdentifier}
	}
	repo, repoErr := inferRepo(cloneRoot, lookup)
	branch, branchErr := lookup.CurrentBranch(cloneRoot)
	if branchErr != nil {
		branchErr = fmt.Errorf("resolving work branch in %s: %w", cloneRoot, branchErr)
	}
	return &workspace{
		root:            root,
		legacyRoot:      filepath.Dir(cloneRoot),
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

// envLoamHome names the directory that holds the CLI's .loam/ state
// directory — staged comments above all. It defaults to the user's home
// directory (see resolveWorkspaceRoot) and exists so an operator can put
// staged review comments somewhere else deliberately, by configuration,
// rather than by accident of which directory a command happened to run in.
const envLoamHome = "LOAM_HOME"

// resolveWorkspaceRoot returns the directory under which .loam/ lives:
// $LOAM_HOME when set, otherwise the user's home directory.
//
// It deliberately does NOT consult the working directory. A cwd-derived
// root is what let one reviewer's staged comments split across two
// directories addressed by the same command (loam-rgyg); a root that
// depends only on the environment is the same for every invocation of
// every command in a session, which is the property staged comments need
// between `work comment` and the irreversible `work verdict` that
// publishes them.
//
// A relative $LOAM_HOME is made absolute here, against the working
// directory of the process that resolves it. Leaving it relative would
// reintroduce exactly the defect this function exists to remove.
func resolveWorkspaceRoot() (string, error) {
	if dir := os.Getenv(envLoamHome); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolving %s=%q to an absolute path: %w", envLoamHome, dir, err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining the home directory for the %s/ state directory (set %s to choose one explicitly): %w", loamDirName, envLoamHome, err)
	}
	return home, nil
}

// newWorkspaceResolver builds the real WorkspaceResolver from the process's
// current working directory (for repo and work-branch inference), cfg's
// resolved agent identifier, and the configured workspace root (for
// staging). See docs/cli-spec.md -> Workspace. Unexported: only deps.go's
// NewProductionDeps and this package's own tests call it.
func newWorkspaceResolver(cfg Config) (WorkspaceResolver, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining working directory: %w", err)
	}
	root, err := resolveWorkspaceRoot()
	if err != nil {
		return nil, err
	}
	return newWorkspace(dir, cfg.Identifier(), root, execGitLookup{}), nil
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

// stagingRel returns the staging tree path for repo/workBranch keyed by
// the calling agent's identifier, RELATIVE to the staging root (see
// OpenStaging, which resolves it against an os.Root).
//
// repo and workBranch reach here from CLI positionals (see
// resolveWorkBranchIdentity): either typed explicitly by whoever invokes
// the CLI, or inferred from the local git clone's origin remote / current
// branch. Both are validated against an explicit character-class allowlist
// (validateStagingKey) before ever touching filepath.Join — repo may nest
// ("<group>/<repo_name>"; docs/cli-spec.md -> clone), workBranch may not.
// That allowlist alone is sufficient to keep every segment local, but the
// composed relative path is additionally verified with filepath.IsLocal as
// a second, structural layer: it catches anything the allowlist might miss
// (including a malicious agent identifier, which is not itself
// user-supplied on this path but costs nothing extra to guard) rather than
// relying solely on rejecting specific substrings.
//
// Both layers are purely LEXICAL and neither can see the filesystem:
// filepath.IsLocal's own godoc says it is "purely lexical" and "does not
// account for the effect of any symbolic links". They exist to turn a bad
// key into a precise usage error (exit 2) naming the offending key, and to
// guarantee two distinct keys never collapse onto one directory. They are
// NOT what keeps a write inside the workspace — OpenStaging's os.Root
// handles are, and they hold even for a key both layers accept.
func (w *workspace) stagingRel(repo, workBranch string) (string, error) {
	if err := validateStagingKey("repo", repo, true); err != nil {
		return "", newUsageCLIError(err.Error(), err)
	}
	if err := validateStagingKey("work branch", workBranch, false); err != nil {
		return "", newUsageCLIError(err.Error(), err)
	}
	rel := filepath.Join(repo, workBranch, w.agentIdentifier)
	if !filepath.IsLocal(rel) {
		err := fmt.Errorf("staging key %w: repo %q / work branch %q escapes the workspace", errInvalidStagingKey, repo, workBranch)
		return "", newUsageCLIError(err.Error(), err)
	}
	return rel, nil
}

// stagingPath composes the absolute staging path for repo/workBranch under
// the workspace root's .loam/ directory (see docs/cli-spec.md -> "Staging
// location") — the clone's parent, never inside the clone itself.
//
// Deliberately unexported, and deliberately NOT on WorkspaceResolver: a
// path string is not a safe capability. Anything holding one can reach the
// filesystem with plain os.MkdirAll/os.WriteFile, which follow symlinks
// and would reintroduce the escape OpenStaging closes. Package-internal
// callers use it only to name a location in diagnostics and tests;
// everyone else goes through OpenStaging's StagingArea handle.
func (w *workspace) stagingPath(repo, workBranch string) (string, error) {
	rel, err := w.stagingRel(repo, workBranch)
	if err != nil {
		return "", err
	}
	return filepath.Join(w.root, loamDirName, stagingDirName, rel), nil
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
