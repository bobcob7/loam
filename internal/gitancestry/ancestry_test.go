package gitancestry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLogger builds a discard-everything *slog.Logger, matching this
// repo's test-logger convention.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// runGit runs a real git command as an external fixture builder would --
// never through Checker -- isolated from the host's own git config, and
// carrying an explicit committer identity.
//
// Copied in shape from internal/gittransport/helpers_test.go's
// runVerificationGit, including its reason: the three isolation variables
// below cut this git off from every config file that would otherwise carry
// an identity, and without GIT_AUTHOR_*/GIT_COMMITTER_* `git commit` falls
// back to guessing username@hostname -- which succeeds with a warning on a
// developer laptop and fails outright on a CI runner with "Please tell me
// who you are", making the gap invisible locally.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=loam-test", "GIT_AUTHOR_EMAIL=loam-test@example.invalid",
		"GIT_COMMITTER_NAME=loam-test", "GIT_COMMITTER_EMAIL=loam-test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// commit writes fileName into dir and commits it, returning the new
// commit's SHA.
func commit(t *testing.T, dir, fileName string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("content\n"), 0o644))
	runGit(t, dir, "add", fileName)
	runGit(t, dir, "commit", "--quiet", "-m", "add "+fileName)
	return runGit(t, dir, "rev-parse", "HEAD")
}

// mirror is one test's bare mirror plus the ordinary clone used to author
// commits into it, matching the production shape: the mirror holds
// refs/heads/main (the target branch) and refs/heads/wb (a work branch).
type mirror struct {
	dir   string
	clone string
}

// newMirror seeds a bare mirror with a single commit on main, plus a clone
// to author from.
func newMirror(t *testing.T) mirror {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	runGit(t, src, "init", "--quiet", "--initial-branch=main")
	commit(t, src, "base.txt")
	bare := filepath.Join(root, "widgets.git")
	runGit(t, root, "clone", "--quiet", "--bare", src, bare)
	clone := filepath.Join(root, "clone")
	runGit(t, root, "clone", "--quiet", bare, clone)
	return mirror{dir: bare, clone: clone}
}

// push publishes the clone's current HEAD to ref in the bare mirror.
func (m mirror) push(t *testing.T, ref string) {
	t.Helper()
	runGit(t, m.clone, "push", "--quiet", "origin", "HEAD:"+ref)
}

// TestContains_MergedTargetTip_ReportsTrue is the catch-up case this
// package exists for: a work branch that has merged the target's current
// tip contains it.
func TestContains_MergedTargetTip_ReportsTrue(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	runGit(t, m.clone, "checkout", "--quiet", "-b", "wb")
	commit(t, m.clone, "wb.txt")
	m.push(t, "refs/heads/wb")
	runGit(t, m.clone, "checkout", "--quiet", "main")
	commit(t, m.clone, "advance.txt")
	m.push(t, "refs/heads/main")
	runGit(t, m.clone, "checkout", "--quiet", "wb")
	runGit(t, m.clone, "merge", "--quiet", "--no-edit", "main")
	caughtUp := commit(t, m.clone, "after-merge.txt")
	m.push(t, "refs/heads/wb")
	got, err := New(testLogger()).Contains(t.Context(), m.dir, "", "refs/heads/main", caughtUp)
	require.NoError(t, err)
	assert.True(t, got, "a branch that merged the current target tip contains it")
}

// TestContains_BranchBehindTheTarget_ReportsFalse is the not-yet-caught-up
// case: a branch forked before the target advanced does not contain the
// new tip, and that is an ANSWER (nil error), not a failure.
func TestContains_BranchBehindTheTarget_ReportsFalse(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	runGit(t, m.clone, "checkout", "--quiet", "-b", "wb")
	behind := commit(t, m.clone, "wb.txt")
	m.push(t, "refs/heads/wb")
	runGit(t, m.clone, "checkout", "--quiet", "main")
	commit(t, m.clone, "advance.txt")
	m.push(t, "refs/heads/main")
	got, err := New(testLogger()).Contains(t.Context(), m.dir, "", "refs/heads/main", behind)
	require.NoError(t, err)
	assert.False(t, got)
}

// TestContains_SameCommit_ReportsTrue pins the CONTAINMENT semantics
// docs/git-spec.md asks for, as opposed to strict descent: a branch whose
// tip IS the target tip has plainly caught up, and that is the shape a
// fast-forward catch-up produces most often.
func TestContains_SameCommit_ReportsTrue(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	tip := runGit(t, m.clone, "rev-parse", "HEAD")
	got, err := New(testLogger()).Contains(t.Context(), m.dir, "", "refs/heads/main", tip)
	require.NoError(t, err)
	assert.True(t, got, "identity must count as containment, or a fast-forwarded branch never clears its flag")
}

// TestContains_UnrelatedHistories_ReportsFalseNotAnError proves two
// histories with no common ancestor are answered (false), not reported as
// a check failure -- git returns a plain exit 1 for that case.
func TestContains_UnrelatedHistories_ReportsFalseNotAnError(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	runGit(t, m.clone, "checkout", "--quiet", "--orphan", "orphan")
	runGit(t, m.clone, "rm", "-rq", "--cached", ".")
	unrelated := commit(t, m.clone, "orphan.txt")
	m.push(t, "refs/heads/orphan")
	got, err := New(testLogger()).Contains(t.Context(), m.dir, "", "refs/heads/main", unrelated)
	require.NoError(t, err)
	assert.False(t, got)
}

// TestContains_QuarantinedObjectIsInvisibleWithoutExtraObjectDir is the
// measurement the whole quarantine plumbing rests on: an object that
// exists only in another object store does not resolve against the bare
// mirror at all, and the failure is an ERROR rather than a quiet "false".
// Without this property a pre-receive-time catch-up check would silently
// report every caught-up push as still behind.
func TestContains_QuarantinedObjectIsInvisibleWithoutExtraObjectDir(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	unpushed := commit(t, m.clone, "unpushed.txt")
	_, err := New(testLogger()).Contains(t.Context(), m.dir, "", "refs/heads/main", unpushed)
	require.Error(t, err, "an object absent from the mirror must not be answered as 'not contained'")
	assert.ErrorIs(t, err, errCheckFailed)
}

// TestContains_ExtraObjectDirMakesTheUnlandedTipReadable proves the fix
// for the case above, and is the exact production shape: the pushed tip
// lives only in receive-pack's quarantine directory while the target tip
// lives in the mirror, and BOTH must be readable in the same invocation --
// which is why this uses GIT_ALTERNATE_OBJECT_DIRECTORIES (additive)
// rather than GIT_OBJECT_DIRECTORY (which would replace the mirror's own
// store and hide the target).
func TestContains_ExtraObjectDirMakesTheUnlandedTipReadable(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	base := runGit(t, m.clone, "rev-parse", "HEAD")
	unpushed := commit(t, m.clone, "unpushed.txt")
	extra := partialObjectDir(t, m.clone, unpushed, base)
	got, err := New(testLogger()).Contains(t.Context(), m.dir, extra, "refs/heads/main", unpushed)
	require.NoError(t, err)
	assert.True(t, got, "the unlanded tip descends from main, and both stores must be visible at once")
}

// partialObjectDir builds a genuine stand-in for receive-pack's quarantine:
// an object directory holding ONLY the objects newSHA adds on top of
// baseSHA, and nothing else. Copying the clone's whole objects directory
// would not do -- it contains the mirror's history too, so the test would
// still pass if the implementation REPLACED the mirror's object store
// (GIT_OBJECT_DIRECTORY) instead of adding to it
// (GIT_ALTERNATE_OBJECT_DIRECTORIES). With only the new objects here, the
// check can only succeed if BOTH stores are readable at once, which is
// exactly the production shape.
func partialObjectDir(t *testing.T, cloneDir, newSHA, baseSHA string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "quarantine")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	listing := runGit(t, cloneDir, "rev-list", "--objects", newSHA, "^"+baseSHA)
	require.NotEmpty(t, listing, "fixture: the new commit must add at least one object")
	for _, line := range strings.Split(listing, "\n") {
		sha, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		require.Len(t, sha, 40, "fixture: unexpected rev-list output %q", line)
		src := filepath.Join(cloneDir, ".git", "objects", sha[:2], sha[2:])
		content, err := os.ReadFile(src)
		require.NoError(t, err, "fixture: object %s must be loose in a fresh clone (everything older is packed)", sha)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, sha[:2]), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, sha[:2], sha[2:]), content, 0o444))
	}
	return dir
}

// TestContains_UnknownRef_IsAnErrorNotFalse proves a target ref that does
// not exist in the mirror is a check failure. Reporting it as "not
// contained" would be the more dangerous of the two: the flag would stay
// forever with no signal that the check never ran.
func TestContains_UnknownRef_IsAnErrorNotFalse(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	tip := runGit(t, m.clone, "rev-parse", "HEAD")
	_, err := New(testLogger()).Contains(t.Context(), m.dir, "", "refs/heads/does-not-exist", tip)
	require.Error(t, err)
	assert.ErrorIs(t, err, errCheckFailed)
}

// TestContains_MissingMirror_IsAnError proves a mirrorDir that is not a
// repository at all fails loudly.
func TestContains_MissingMirror_IsAnError(t *testing.T) {
	t.Parallel()
	_, err := New(testLogger()).Contains(t.Context(), filepath.Join(t.TempDir(), "absent.git"), "", "refs/heads/main", "HEAD")
	require.Error(t, err)
	assert.ErrorIs(t, err, errCheckFailed)
}

// TestContains_RevThatLooksLikeAnOption_IsRejectedBeforeGitRuns proves a
// leading dash never reaches git's argv, where it would be parsed as a
// flag rather than as a revision.
func TestContains_RevThatLooksLikeAnOption_IsRejectedBeforeGitRuns(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	c := New(testLogger())
	_, err := c.Contains(t.Context(), m.dir, "", "--help", "HEAD")
	require.ErrorIs(t, err, errUnsafeRev)
	_, err = c.Contains(t.Context(), m.dir, "", "refs/heads/main", "")
	require.ErrorIs(t, err, errUnsafeRev)
}

// TestContains_CanceledContext_ReportsCancellationNotAnAnswer proves a
// context canceled before the run surfaces as a check failure carrying
// context.Canceled, so a caller can tell "we shut down mid-check" from
// "git broke" -- and, crucially, never mistakes a killed subprocess for a
// "not contained" answer.
func TestContains_CanceledContext_ReportsCancellationNotAnAnswer(t *testing.T) {
	t.Parallel()
	m := newMirror(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := New(testLogger()).Contains(ctx, m.dir, "", "refs/heads/main", "refs/heads/main")
	require.Error(t, err)
	assert.ErrorIs(t, err, errCheckFailed)
	assert.ErrorIs(t, err, context.Canceled)
}
