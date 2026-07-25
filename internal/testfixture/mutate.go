package testfixture

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
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

// identityEnv pins commit authorship so every mutation is attributable to
// the fixture rather than the ambient git config of whatever machine runs
// the tests.
var identityEnv = []string{"GIT_AUTHOR_NAME=loam-fixture", "GIT_AUTHOR_EMAIL=fixture@loam.test", "GIT_COMMITTER_NAME=loam-fixture", "GIT_COMMITTER_EMAIL=fixture@loam.test"}

// Rev resolves ref to a commit SHA within r.
func (r *Repo) Rev(ctx context.Context, tb testing.TB, ref string) string {
	tb.Helper()
	return runGitIn(ctx, tb, r.gitDir(), nil, nil, "rev-parse", ref)
}

// revOrDefaultBranch resolves branch if it exists, or falls back to
// defaultBranch's current tip -- the starting point used when a mutation
// helper is asked to advance a branch that has not been created yet.
func (r *Repo) revOrDefaultBranch(ctx context.Context, tb testing.TB, branch string) string {
	tb.Helper()
	if sha, ok := tryRevIn(ctx, r.gitDir(), branch); ok {
		return sha
	}
	return r.Rev(ctx, tb, defaultBranch)
}

// Advance commits one new, trivial change as a child of branch's current tip
// and fast-forwards branch to point at it, simulating ordinary upstream
// progress (docs/git-spec.md, "Target Advances & Catch-Up"). If branch does
// not exist yet, it is created starting from defaultBranch's current tip.
// Returns the new commit SHA.
func (r *Repo) Advance(ctx context.Context, tb testing.TB, branch string) string {
	tb.Helper()
	parent := r.revOrDefaultBranch(ctx, tb, branch)
	content := []byte(fmt.Sprintf("advance %s from %s at %s\n", branch, parent, time.Now().UTC().Format(time.RFC3339Nano)))
	sha := r.commitFile(ctx, tb, parent, advancePath, content, "advance "+branch)
	r.updateRef(ctx, tb, branch, sha, "")
	return sha
}

// Conflict diverges base and branch from their current common tip: base
// advances with one change to a shared file, and branch advances from the
// same starting point with a different change to the same line, so that
// `git merge-tree` between the two resulting tips reports a real conflict
// rather than a clean fast-forward or auto-merge. If branch does not exist
// yet, it is created starting from base's current tip. Returns (baseSHA,
// branchSHA).
func (r *Repo) Conflict(ctx context.Context, tb testing.TB, base, branch string) (string, string) {
	tb.Helper()
	common := r.Rev(ctx, tb, base)
	baseContent := []byte("# Changelog\n\n- initial: fixture seeded\n- " + base + ": advanced by upstream\n")
	branchContent := []byte("# Changelog\n\n- initial: fixture seeded\n- " + branch + ": advanced by work branch\n")
	baseSHA := r.commitFile(ctx, tb, common, conflictPath, baseContent, "advance "+base+": conflicting edit")
	branchSHA := r.commitFile(ctx, tb, common, conflictPath, branchContent, "advance "+branch+": conflicting edit")
	r.updateRef(ctx, tb, base, baseSHA, common)
	r.updateRef(ctx, tb, branch, branchSHA, "")
	return baseSHA, branchSHA
}

// ForcePush rewrites branch's history: it creates a brand-new, parentless
// commit sharing no ancestry with branch's previous tip, then forces branch
// to point at it. The previous tip becomes unreachable from any ref and has
// no valid merge base with the new one -- a history rewrite, not an
// incremental change. Returns the new commit SHA.
func (r *Repo) ForcePush(ctx context.Context, tb testing.TB, branch string) string {
	tb.Helper()
	content := []byte(fmt.Sprintf("force-push %s rewritten at %s\n", branch, time.Now().UTC().Format(time.RFC3339Nano)))
	sha := r.commitOrphan(ctx, tb, forcePushPath, content, "force-push "+branch+": history rewrite")
	r.updateRef(ctx, tb, branch, sha, "")
	return sha
}

// DeleteBranch removes branch's ref entirely, simulating the upstream
// deleting it (e.g. after merging).
func (r *Repo) DeleteBranch(ctx context.Context, tb testing.TB, branch string) {
	tb.Helper()
	runGitIn(ctx, tb, r.gitDir(), nil, nil, "update-ref", "-d", "refs/heads/"+branch)
}

// Rename moves oldPath to newPath, unchanged, on branch, committing the
// result as branch's new tip. Because the blob content is identical, git's
// rename detection (`git diff --name-status -M`) reports this as a rename,
// exercising the ingest pipeline's renamed-away path: the old path is
// dropped and the new path is (re-)parsed. Returns the new commit SHA.
func (r *Repo) Rename(ctx context.Context, tb testing.TB, branch, oldPath, newPath string) string {
	tb.Helper()
	gitDir := r.gitDir()
	parent := r.Rev(ctx, tb, branch)
	index := filepath.Join(tb.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	runGitIn(ctx, tb, gitDir, nil, env, "read-tree", parent+"^{tree}")
	blobSHA := r.lsTreeBlob(ctx, tb, parent, oldPath)
	runGitIn(ctx, tb, gitDir, nil, env, "update-index", "--force-remove", oldPath)
	runGitIn(ctx, tb, gitDir, nil, env, "update-index", "--add", "--cacheinfo", "100644,"+blobSHA+","+newPath)
	tree := runGitIn(ctx, tb, gitDir, nil, env, "write-tree")
	commitEnv := append(append([]string{}, env...), identityEnv...)
	sha := runGitIn(ctx, tb, gitDir, nil, commitEnv, "commit-tree", tree, "-p", parent, "-m", "rename "+oldPath+" -> "+newPath)
	r.updateRef(ctx, tb, branch, sha, parent)
	return sha
}

// lsTreeBlob resolves the blob SHA for path within ref's tree.
func (r *Repo) lsTreeBlob(ctx context.Context, tb testing.TB, ref, path string) string {
	tb.Helper()
	line := runGitIn(ctx, tb, r.gitDir(), nil, nil, "ls-tree", ref, "--", path)
	fields := strings.Fields(line)
	if len(fields) < 3 {
		tb.Fatalf("ls-tree %s -- %s: unexpected output %q", ref, path, line)
	}
	return fields[2]
}

// commitFile builds a commit that is parent's tree with path replaced by
// content, and commits it as a single-parent child of parent. It does not
// move any ref.
func (r *Repo) commitFile(ctx context.Context, tb testing.TB, parent, path string, content []byte, message string) string {
	tb.Helper()
	gitDir := r.gitDir()
	index := filepath.Join(tb.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	runGitIn(ctx, tb, gitDir, nil, env, "read-tree", parent+"^{tree}")
	tree := r.writeBlobAndTree(ctx, tb, env, path, content)
	commitEnv := append(append([]string{}, env...), identityEnv...)
	return runGitIn(ctx, tb, gitDir, nil, commitEnv, "commit-tree", tree, "-p", parent, "-m", message)
}

// commitOrphan builds a commit containing only path=content, with no
// parent, sharing no ancestry with anything already in the repo. It does
// not move any ref.
func (r *Repo) commitOrphan(ctx context.Context, tb testing.TB, path string, content []byte, message string) string {
	tb.Helper()
	gitDir := r.gitDir()
	index := filepath.Join(tb.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	tree := r.writeBlobAndTree(ctx, tb, env, path, content)
	commitEnv := append(append([]string{}, env...), identityEnv...)
	return runGitIn(ctx, tb, gitDir, nil, commitEnv, "commit-tree", tree, "-m", message)
}

// writeBlobAndTree hashes content into the object store, stages it at path
// in the index named by env's GIT_INDEX_FILE, and writes the resulting tree.
func (r *Repo) writeBlobAndTree(ctx context.Context, tb testing.TB, env []string, path string, content []byte) string {
	tb.Helper()
	gitDir := r.gitDir()
	blob := runGitIn(ctx, tb, gitDir, content, env, "hash-object", "-w", "--stdin")
	runGitIn(ctx, tb, gitDir, nil, env, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	return runGitIn(ctx, tb, gitDir, nil, env, "write-tree")
}

// updateRef sets refs/heads/<branch> to newSHA. If oldSHA is non-empty, git
// verifies branch's current value matches oldSHA first (a safety check, not
// a fast-forward check); an empty oldSHA force-updates or creates the ref
// unconditionally.
func (r *Repo) updateRef(ctx context.Context, tb testing.TB, branch, newSHA, oldSHA string) {
	tb.Helper()
	args := []string{"update-ref", "refs/heads/" + branch, newSHA}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	runGitIn(ctx, tb, r.gitDir(), nil, nil, args...)
}
