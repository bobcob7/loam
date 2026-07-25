package testfixture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// advancePath and conflictPath are auxiliary, non-symbol-graph files used as
// the target of Advance and Conflict commits, keeping mutation plumbing
// separate from the fixture's meaningful Go/TypeScript/Python/Markdown
// content.
const (
	advancePath   = "ADVANCE.log"
	conflictPath  = "CHANGELOG.md"
	forcePushPath = "FORCE_PUSH.log"
)

// Rev resolves ref to a commit SHA within r.
func (r *Repo) Rev(ctx context.Context, ref string) (string, error) {
	sha, err := runGitIn(ctx, r.gitDir(), nil, nil, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref, err)
	}
	return sha, nil
}

// revOrDefaultBranch resolves branch if it exists (existed=true), or falls
// back to defaultBranch's current tip (existed=false) -- the starting point
// used when a mutation helper is asked to advance a branch that has not
// been created yet.
func (r *Repo) revOrDefaultBranch(ctx context.Context, branch string) (sha string, existed bool, err error) {
	if sha, ok := tryRevIn(ctx, r.gitDir(), branch); ok {
		return sha, true, nil
	}
	sha, err = r.Rev(ctx, defaultBranch)
	return sha, false, err
}

// Advance commits one new, trivial change as a child of branch's current tip
// and fast-forwards branch to point at it, simulating ordinary upstream
// progress (docs/git-spec.md, "Target Advances & Catch-Up"). If branch does
// not exist yet, it is created starting from defaultBranch's current tip.
// Returns the new commit SHA.
func (r *Repo) Advance(ctx context.Context, branch string) (string, error) {
	parent, existed, err := r.revOrDefaultBranch(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("advancing %s: %w", branch, err)
	}
	content := []byte(fmt.Sprintf("advance %s from %s at %s\n", branch, parent, time.Now().UTC().Format(time.RFC3339Nano)))
	sha, err := r.commitFile(ctx, parent, advancePath, content, "advance "+branch)
	if err != nil {
		return "", fmt.Errorf("advancing %s: %w", branch, err)
	}
	old := ""
	if existed {
		old = parent
	}
	if err := r.updateRef(ctx, branch, sha, old); err != nil {
		return "", fmt.Errorf("advancing %s: %w", branch, err)
	}
	return sha, nil
}

// Conflict diverges base and branch from base's current tip: base advances
// with one change to a shared file, and branch advances from its OWN
// current tip (preserving any history branch already has) with a different
// change to the same line, so that `git merge-tree` between the two
// resulting tips reports a real conflict rather than a clean fast-forward
// or auto-merge. If branch does not exist yet, it is created starting from
// defaultBranch's current tip, matching Advance. Returns (baseSHA,
// branchSHA).
func (r *Repo) Conflict(ctx context.Context, base, branch string) (string, string, error) {
	baseParent, err := r.Rev(ctx, base)
	if err != nil {
		return "", "", fmt.Errorf("diverging %s and %s: %w", base, branch, err)
	}
	branchParent, branchExisted, err := r.revOrDefaultBranch(ctx, branch)
	if err != nil {
		return "", "", fmt.Errorf("diverging %s and %s: %w", base, branch, err)
	}
	baseContent := []byte("# Changelog\n\n- initial: fixture seeded\n- " + base + ": advanced by upstream\n")
	branchContent := []byte("# Changelog\n\n- initial: fixture seeded\n- " + branch + ": advanced by work branch\n")
	baseSHA, err := r.commitFile(ctx, baseParent, conflictPath, baseContent, "advance "+base+": conflicting edit")
	if err != nil {
		return "", "", fmt.Errorf("diverging %s: %w", base, err)
	}
	branchSHA, err := r.commitFile(ctx, branchParent, conflictPath, branchContent, "advance "+branch+": conflicting edit")
	if err != nil {
		return "", "", fmt.Errorf("diverging %s: %w", branch, err)
	}
	if err := r.updateRef(ctx, base, baseSHA, baseParent); err != nil {
		return "", "", fmt.Errorf("diverging %s: %w", base, err)
	}
	oldBranchSHA := ""
	if branchExisted {
		oldBranchSHA = branchParent
	}
	if err := r.updateRef(ctx, branch, branchSHA, oldBranchSHA); err != nil {
		return "", "", fmt.Errorf("diverging %s: %w", branch, err)
	}
	return baseSHA, branchSHA, nil
}

// ForcePush rewrites branch's history: it creates a brand-new, parentless
// commit sharing no ancestry with branch's previous tip, then forces branch
// to point at it. The previous tip becomes unreachable from any ref and has
// no valid merge base with the new one -- a history rewrite, not an
// incremental change. Returns the new commit SHA.
func (r *Repo) ForcePush(ctx context.Context, branch string) (string, error) {
	content := []byte(fmt.Sprintf("force-push %s rewritten at %s\n", branch, time.Now().UTC().Format(time.RFC3339Nano)))
	sha, err := r.commitOrphan(ctx, forcePushPath, content, "force-push "+branch+": history rewrite")
	if err != nil {
		return "", fmt.Errorf("force-pushing %s: %w", branch, err)
	}
	if err := r.updateRef(ctx, branch, sha, ""); err != nil {
		return "", fmt.Errorf("force-pushing %s: %w", branch, err)
	}
	return sha, nil
}

// DeleteBranch removes branch's ref entirely, simulating the upstream
// deleting it (e.g. after merging).
func (r *Repo) DeleteBranch(ctx context.Context, branch string) error {
	if _, err := runGitIn(ctx, r.gitDir(), nil, nil, "update-ref", "-d", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("deleting branch %s: %w", branch, err)
	}
	return nil
}

// Rename moves oldPath to newPath, unchanged, on branch, committing the
// result as branch's new tip. Because the blob content is identical, git's
// rename detection (`git diff --name-status -M`) reports this as a rename,
// exercising the ingest pipeline's renamed-away path: the old path is
// dropped and the new path is (re-)parsed. Returns the new commit SHA.
func (r *Repo) Rename(ctx context.Context, branch, oldPath, newPath string) (string, error) {
	gitDir := r.gitDir()
	parent, err := r.Rev(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	index, cleanup, err := scratchIndex()
	if err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := runGitIn(ctx, gitDir, nil, env, "read-tree", parent+"^{tree}"); err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	blobSHA, err := r.lsTreeBlob(ctx, parent, oldPath)
	if err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	if _, err := runGitIn(ctx, gitDir, nil, env, "update-index", "--force-remove", oldPath); err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	if _, err := runGitIn(ctx, gitDir, nil, env, "update-index", "--add", "--cacheinfo", "100644,"+blobSHA+","+newPath); err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	tree, err := runGitIn(ctx, gitDir, nil, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	commitEnv := append(fixtureIdentityEnv(), env...)
	sha, err := runGitIn(ctx, gitDir, nil, commitEnv, "commit-tree", tree, "-p", parent, "-m", "rename "+oldPath+" -> "+newPath)
	if err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	if err := r.updateRef(ctx, branch, sha, parent); err != nil {
		return "", fmt.Errorf("renaming %s to %s: %w", oldPath, newPath, err)
	}
	return sha, nil
}

// lsTreeBlob resolves the blob SHA for path within ref's tree.
func (r *Repo) lsTreeBlob(ctx context.Context, ref, path string) (string, error) {
	line, err := runGitIn(ctx, r.gitDir(), nil, nil, "ls-tree", ref, "--", path)
	if err != nil {
		return "", fmt.Errorf("listing tree for %s at %s: %w", path, ref, err)
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", fmt.Errorf("ls-tree %s -- %s: unexpected output %q", ref, path, line)
	}
	return fields[2], nil
}

// commitFile builds a commit that is parent's tree with path replaced by
// content, and commits it as a single-parent child of parent. It does not
// move any ref.
func (r *Repo) commitFile(ctx context.Context, parent, path string, content []byte, message string) (string, error) {
	gitDir := r.gitDir()
	index, cleanup, err := scratchIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := runGitIn(ctx, gitDir, nil, env, "read-tree", parent+"^{tree}"); err != nil {
		return "", fmt.Errorf("reading tree for %s: %w", parent, err)
	}
	tree, err := r.writeBlobAndTree(ctx, env, path, content)
	if err != nil {
		return "", err
	}
	commitEnv := append(fixtureIdentityEnv(), env...)
	sha, err := runGitIn(ctx, gitDir, nil, commitEnv, "commit-tree", tree, "-p", parent, "-m", message)
	if err != nil {
		return "", fmt.Errorf("committing tree onto %s: %w", parent, err)
	}
	return sha, nil
}

// commitOrphan builds a commit containing only path=content, with no
// parent, sharing no ancestry with anything already in the repo. It does
// not move any ref.
func (r *Repo) commitOrphan(ctx context.Context, path string, content []byte, message string) (string, error) {
	gitDir := r.gitDir()
	index, cleanup, err := scratchIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + index}
	tree, err := r.writeBlobAndTree(ctx, env, path, content)
	if err != nil {
		return "", err
	}
	commitEnv := append(fixtureIdentityEnv(), env...)
	sha, err := runGitIn(ctx, gitDir, nil, commitEnv, "commit-tree", tree, "-m", message)
	if err != nil {
		return "", fmt.Errorf("committing orphan tree: %w", err)
	}
	return sha, nil
}

// writeBlobAndTree hashes content into the object store, stages it at path
// in the index named by env's GIT_INDEX_FILE, and writes the resulting
// tree.
func (r *Repo) writeBlobAndTree(ctx context.Context, env []string, path string, content []byte) (string, error) {
	gitDir := r.gitDir()
	blob, err := runGitIn(ctx, gitDir, content, env, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", fmt.Errorf("hashing blob for %s: %w", path, err)
	}
	if _, err := runGitIn(ctx, gitDir, nil, env, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path); err != nil {
		return "", fmt.Errorf("staging %s: %w", path, err)
	}
	tree, err := runGitIn(ctx, gitDir, nil, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("writing tree: %w", err)
	}
	return tree, nil
}

// updateRef sets refs/heads/<branch> to newSHA. If oldSHA is non-empty, git
// verifies branch's current value matches oldSHA first (a safety check, not
// a fast-forward check); an empty oldSHA force-updates or creates the ref
// unconditionally.
func (r *Repo) updateRef(ctx context.Context, branch, newSHA, oldSHA string) error {
	args := []string{"update-ref", "refs/heads/" + branch, newSHA}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	if _, err := runGitIn(ctx, r.gitDir(), nil, nil, args...); err != nil {
		return fmt.Errorf("updating refs/heads/%s: %w", branch, err)
	}
	return nil
}

// scratchIndex creates a fresh temp directory holding a throwaway git index
// file, for building a tree via read-tree/update-index/write-tree without
// disturbing the repository's real index (if any) or requiring a working
// tree. The returned cleanup removes the temp directory; callers must defer
// it.
func scratchIndex() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "fixture-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating scratch index dir: %w", err)
	}
	return filepath.Join(dir, "index"), func() { os.RemoveAll(dir) }, nil
}
