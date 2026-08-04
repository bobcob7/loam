package gitref

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
)

// Every test here runs a real git subprocess against a real bare mirror --
// there is no fake git in this package and there should not be, since the
// whole contract is "what git does with these refs".

// runGit runs a real git subcommand in dir (empty means the process's own
// cwd), failing t immediately on a nonzero exit.
//
// The environment is pinned rather than inherited: GIT_CONFIG_NOSYSTEM plus
// a temp HOME/XDG_CONFIG_HOME so no host or user gitconfig is read, and
// explicit GIT_AUTHOR_*/GIT_COMMITTER_* because git's own
// user@hostname guess works on a developer's laptop and FAILS on CI, which
// is exactly the gap that has made this project red before.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "unused-global-gitconfig"),
		"GIT_AUTHOR_NAME=gitref-test",
		"GIT_AUTHOR_EMAIL=gitref-test@example.invalid",
		"GIT_COMMITTER_NAME=gitref-test",
		"GIT_COMMITTER_EMAIL=gitref-test@example.invalid",
		"GIT_TERMINAL_PROMPT=0",
	}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// seedMirror builds a real bare mirror at mirrorpath.Dir(dataDir,
// "acme/widgets") with one commit on "main", and returns dataDir, the
// mirror path, and main's tip SHA.
func seedMirror(t *testing.T) (dataDir, mirrorDir, mainSHA string) {
	t.Helper()
	src := t.TempDir()
	runGit(t, src, "init", "--quiet", "--initial-branch=main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("hello\n"), 0o644))
	runGit(t, src, "add", "f.txt")
	runGit(t, src, "commit", "--quiet", "-m", "init")
	dataDir = t.TempDir()
	mirrorDir = mirrorpath.Dir(dataDir, "acme/widgets")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGit(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
	return dataDir, mirrorDir, runGit(t, "", "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
}

// refSHA reads a ref back from the mirror directly, returning "" when it
// does not exist, so a test never has to trust this package's own code for
// the "did it actually land" proof.
func refSHA(t *testing.T, mirrorDir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir="+mirrorDir, "rev-parse", "--verify", "--quiet", ref)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "HOME=" + t.TempDir()}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestCreateWorkBranchRef_CreatesTheReservedRefAtTheTargetTip is loam-5iu's
// core claim: after `work start`, the mirror carries the work branch's ref,
// under the reserved namespace, at exactly the target branch's tip -- and
// NOT at refs/heads/<name>, which would be an unregistered ref every push
// is rejected for and which the mirror fetch could prune.
func TestCreateWorkBranchRef_CreatesTheReservedRefAtTheTargetTip(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)

	require.NoError(t, New(dataDir).CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))

	assert.Equal(t, mainSHA, refSHA(t, mirrorDir, "refs/heads/loam-reserved/wb-9c2f1a"))
	assert.Empty(t, refSHA(t, mirrorDir, "refs/heads/wb-9c2f1a"), "the ref must NOT be created at the unreserved path")
}

// TestCreateWorkBranchRef_RefusesToMoveAnExistingRef proves the create is
// guarded. An unguarded update-ref would silently rewind a live work
// branch's history on a name collision, discarding the agent's commits with
// no error anywhere -- which is the same class of unrecoverable loss
// loam-cmq's prune was.
func TestCreateWorkBranchRef_RefusesToMoveAnExistingRef(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))
	// Advance the work branch as an agent's push would, so a second create
	// would visibly rewind it rather than be a no-op.
	runGit(t, "", "--git-dir="+mirrorDir, "branch", "other", "refs/heads/main")
	runGit(t, "", "--git-dir="+mirrorDir, "update-ref", "refs/heads/loam-reserved/wb-9c2f1a", mainSHA)
	advanced := runGit(t, "", "--git-dir="+mirrorDir, "commit-tree", "-m", "agent commit", "-p", mainSHA, mainSHA+"^{tree}")
	runGit(t, "", "--git-dir="+mirrorDir, "update-ref", "refs/heads/loam-reserved/wb-9c2f1a", advanced)
	require.NotEqual(t, mainSHA, advanced)

	err := c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefExists)
	assert.Equal(t, advanced, refSHA(t, mirrorDir, "refs/heads/loam-reserved/wb-9c2f1a"), "the existing ref must not have moved")
}

// TestCreateWorkBranchRef_TargetMissing_ReturnsErrTargetMissing covers the
// reachable-without-anything-being-broken case: CreateWorkBranch validates
// `from` against repo_target_branches, not against the mirror, so a repo
// whose first sync has not landed has the row and no ref.
func TestCreateWorkBranchRef_TargetMissing_ReturnsErrTargetMissing(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, _ := seedMirror(t)

	err := New(dataDir).CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "no-such-target")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetMissing)
	assert.Empty(t, refSHA(t, mirrorDir, "refs/heads/loam-reserved/wb-9c2f1a"), "no ref may be created when the target does not resolve")
}

// TestCreateWorkBranchRef_MirrorMissing_ReturnsErrMirrorMissing proves a
// mirror that is absent (or is a directory that is not a repository) is
// classified as the operational fault it is, rather than surfacing as an
// opaque git failure -- and, because addressing is via --git-dir and never
// -C, without escaping upward into an enclosing repository.
func TestCreateWorkBranchRef_MirrorMissing_ReturnsErrMirrorMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	require.NoError(t, os.MkdirAll(mirrorpath.Dir(dataDir, "acme/widgets"), 0o755))

	err := New(dataDir).CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMirrorMissing)
}

// TestDeleteWorkBranchRef_RemovesTheRef covers the rollback path's success
// case.
func TestDeleteWorkBranchRef_RemovesTheRef(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, _ := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))
	require.NotEmpty(t, refSHA(t, mirrorDir, "refs/heads/loam-reserved/wb-9c2f1a"))

	require.NoError(t, c.DeleteWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a"))

	assert.Empty(t, refSHA(t, mirrorDir, "refs/heads/loam-reserved/wb-9c2f1a"))
}

// TestDeleteWorkBranchRef_AbsentRefIsNotAnError pins the contract the
// compensating rollback depends on: it must be safe to run against a create
// that never landed, or a failed insert would report a misleading
// rollback error instead of the insert's own.
func TestDeleteWorkBranchRef_AbsentRefIsNotAnError(t *testing.T) {
	t.Parallel()
	dataDir, _, _ := seedMirror(t)

	assert.NoError(t, New(dataDir).DeleteWorkBranchRef(t.Context(), "acme/widgets", "wb-never-created"))
}

// TestResolveWorkBranchRef_ReturnsTheCurrentTip proves the happy path reads
// back exactly what the ref points at -- the same value refSHA's direct
// rev-parse would report, so this method is not trusted blind.
func TestResolveWorkBranchRef_ReturnsTheCurrentTip(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))

	sha, err := c.ResolveWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a")

	require.NoError(t, err)
	assert.Equal(t, mainSHA, sha)
	assert.Equal(t, refSHA(t, mirrorDir, "refs/heads/loam-reserved/wb-9c2f1a"), sha)
}

// TestResolveWorkBranchRef_MovesWithTheRef proves this is a LIVE read, not
// a cached one: after the ref is moved by a real push, the next resolve
// reports the NEW tip -- the property loam-cgg's whole comparison depends
// on (a work branch pushed to after acceptance must resolve as "moved").
func TestResolveWorkBranchRef_MovesWithTheRef(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))
	first, err := c.ResolveWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a")
	require.NoError(t, err)
	require.Equal(t, mainSHA, first)

	newTip := advanceRef(t, mirrorDir, "refs/heads/loam-reserved/wb-9c2f1a")

	second, err := c.ResolveWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a")
	require.NoError(t, err)
	assert.Equal(t, newTip, second)
	assert.NotEqual(t, first, second)
}

// TestResolveWorkBranchRef_RefMissing_ReturnsErrRefMissing proves a work
// branch ref that was never created (or was deleted) is reported as
// ErrRefMissing, distinguishable from CreateWorkBranchRef's
// ErrTargetMissing even though both come off the same underlying
// rev-parse classification.
func TestResolveWorkBranchRef_RefMissing_ReturnsErrRefMissing(t *testing.T) {
	t.Parallel()
	dataDir, _, _ := seedMirror(t)

	_, err := New(dataDir).ResolveWorkBranchRef(t.Context(), "acme/widgets", "wb-never-created")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMissing)
	assert.NotErrorIs(t, err, ErrTargetMissing)
}

// TestResolveWorkBranchRef_MirrorMissing_ReturnsErrMirrorMissing mirrors
// CreateWorkBranchRef's identically-named test: an absent or invalid
// mirror is an operational fault, not a "no such ref" answer.
func TestResolveWorkBranchRef_MirrorMissing_ReturnsErrMirrorMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	require.NoError(t, os.MkdirAll(mirrorpath.Dir(dataDir, "acme/widgets"), 0o755))

	_, err := New(dataDir).ResolveWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMirrorMissing)
}

// advanceRef moves ref to a new commit inside the bare mirror by cloning
// it, committing, and pushing back -- the only way to move a bare repo's
// ref to a genuinely new commit without hand-rolling plumbing. Returns the
// new tip SHA.
//
// The commit's blob content and message are keyed on ref itself rather than
// a fixed string, so two calls that advance two DIFFERENT refs back to back
// are guaranteed to produce different tree hashes -- and therefore
// different commit SHAs -- BY CONSTRUCTION. This is load-bearing, not
// cosmetic (loam-giq.14): with author/committer pinned identically by
// runGit's fixed environment, an unvarying parent, and (until this fix) a
// fixed message and content, a commit hash's only remaining input was the
// timestamp -- and two such commits landing inside the same wall-clock
// second, which the CI container does every time, produced the IDENTICAL
// SHA. That silently stopped
// TestAdvanceWorkBranchRef_RefusesWhenTheRefMovedUnderIt from exercising
// AdvanceWorkBranchRef's compare-and-swap guard at all. Do not revert this
// to a fixed string, and do not "fix" a future flake here with a sleep --
// that trades a fast flake for a slow one instead of removing the
// coincidence.
func advanceRef(t *testing.T, mirrorDir, ref string) string {
	t.Helper()
	require.True(t, strings.HasPrefix(ref, "refs/heads/"), "advanceRef needs a refs/heads/ ref, got %q", ref)
	tracking := "origin/" + strings.TrimPrefix(ref, "refs/heads/")
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "--quiet", mirrorDir, clone)
	runGit(t, clone, "checkout", "--quiet", "-B", "local-work", tracking)
	require.NoError(t, os.WriteFile(filepath.Join(clone, "advance.txt"), []byte("advanced: "+ref+"\n"), 0o644))
	runGit(t, clone, "add", "advance.txt")
	runGit(t, clone, "commit", "--quiet", "-m", "advance "+ref)
	runGit(t, clone, "push", "--quiet", "origin", "HEAD:"+ref)
	return refSHA(t, mirrorDir, ref)
}

// TestResolveUpstreamProposalRef_ReadsTheMirroredUpstreamBranch pins the
// ref path this method reads: refs/heads/loam/<name>, the branch proposal
// acceptance pushes upstream and the mirror fetch brings back -- NOT the
// reserved refs/heads/loam-reserved/<name> copy Loam owns. The two are
// seeded at DIFFERENT commits here on purpose: a method that read the
// reserved ref would return a real SHA and pass a weaker assertion, so the
// only thing that distinguishes them is that they disagree.
func TestResolveUpstreamProposalRef_ReadsTheMirroredUpstreamBranch(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))
	runGit(t, "", "--git-dir="+mirrorDir, "update-ref", refnames.UpstreamProposalBranch("wb-9c2f1a"), mainSHA)
	upstreamSHA := advanceRef(t, mirrorDir, refnames.UpstreamProposalBranch("wb-9c2f1a"))
	require.NotEqual(t, mainSHA, upstreamSHA, "the two refs must disagree, or this test cannot tell them apart")

	sha, err := c.ResolveUpstreamProposalRef(t.Context(), "acme/widgets", "wb-9c2f1a")

	require.NoError(t, err)
	assert.Equal(t, upstreamSHA, sha)
	workBranchSHA, err := c.ResolveWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a")
	require.NoError(t, err)
	assert.Equal(t, mainSHA, workBranchSHA, "the reserved work-branch ref must be untouched by an upstream read")
}

// TestResolveUpstreamProposalRef_RefMissing_ReturnsErrRefMissing covers the
// ordinary state of a work branch that was never accepted, and of one whose
// PR ended and whose upstream branch the poller deleted. The drift
// reconciler skips exactly this error, so it must not be reported as an
// empty SHA (which would read as "upstream differs from accepted_tip").
func TestResolveUpstreamProposalRef_RefMissing_ReturnsErrRefMissing(t *testing.T) {
	t.Parallel()
	dataDir, _, _ := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))

	sha, err := c.ResolveUpstreamProposalRef(t.Context(), "acme/widgets", "wb-9c2f1a")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMissing)
	assert.Empty(t, sha)
}

// TestAdvanceWorkBranchRef_MovesTheReservedRefToTheUpstreamCommit is the
// adoption's git half: the work branch ends up at the commit that arrived
// upstream, and the upstream ref itself is not touched (this method only
// ever writes the reserved namespace).
func TestAdvanceWorkBranchRef_MovesTheReservedRefToTheUpstreamCommit(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))
	runGit(t, "", "--git-dir="+mirrorDir, "update-ref", refnames.UpstreamProposalBranch("wb-9c2f1a"), mainSHA)
	upstreamSHA := advanceRef(t, mirrorDir, refnames.UpstreamProposalBranch("wb-9c2f1a"))

	require.NoError(t, c.AdvanceWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", mainSHA, upstreamSHA))

	assert.Equal(t, upstreamSHA, refSHA(t, mirrorDir, refnames.WorkBranch("wb-9c2f1a")))
	assert.Equal(t, upstreamSHA, refSHA(t, mirrorDir, refnames.UpstreamProposalBranch("wb-9c2f1a")))
}

// TestAdvanceWorkBranchRef_RefusesWhenTheRefMovedUnderIt is the whole
// safety argument for a second writer of work-branch refs: the swap is
// compared against the value the caller last read, so an agent push that
// landed in between is never overwritten.
//
// The ref is moved to a THIRD commit here, unrelated to the one being
// adopted, so a failure to compare would be visible as a lost commit rather
// than as a harmless no-op.
func TestAdvanceWorkBranchRef_RefusesWhenTheRefMovedUnderIt(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))
	adopting := advanceRef(t, mirrorDir, "refs/heads/main")
	agentPushed := advanceRef(t, mirrorDir, refnames.WorkBranch("wb-9c2f1a"))
	require.NotEqual(t, agentPushed, adopting)

	err := c.AdvanceWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", mainSHA, adopting)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMoved)
	assert.Equal(t, agentPushed, refSHA(t, mirrorDir, refnames.WorkBranch("wb-9c2f1a")), "a refused swap must leave the agent's push exactly where it is")
}

// TestAdvanceWorkBranchRef_MissingRef_ReturnsErrRefMissing proves this
// method never CREATES a work-branch ref: adopting a commit into a branch
// that has no ref would be conjuring a work branch out of an upstream push.
func TestAdvanceWorkBranchRef_MissingRef_ReturnsErrRefMissing(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)

	err := New(dataDir).AdvanceWorkBranchRef(t.Context(), "acme/widgets", "wb-never-created", mainSHA, mainSHA)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMissing)
	assert.Empty(t, refSHA(t, mirrorDir, refnames.WorkBranch("wb-never-created")))
}

// TestAdvanceWorkBranchRef_BlankSHAsAreRejectedBeforeGitSeesThem pins the
// two measured git behaviours errBlankSHA exists to keep out of reach: an
// empty NEW value deletes the ref (exit 0, no complaint), and an empty OLD
// value means "must not exist" rather than "any value". Both are asserted
// through the ref still holding its original commit afterwards, not merely
// through the error.
func TestAdvanceWorkBranchRef_BlankSHAsAreRejectedBeforeGitSeesThem(t *testing.T) {
	t.Parallel()
	dataDir, mirrorDir, mainSHA := seedMirror(t)
	c := New(dataDir)
	require.NoError(t, c.CreateWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", "main"))
	for _, tc := range []struct {
		name     string
		from, to string
	}{
		{name: "empty new value would delete the ref", from: mainSHA, to: ""},
		{name: "empty old value would not compare anything", from: "", to: mainSHA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.AdvanceWorkBranchRef(t.Context(), "acme/widgets", "wb-9c2f1a", tc.from, tc.to)
			require.Error(t, err)
			assert.ErrorIs(t, err, errBlankSHA)
			assert.Equal(t, mainSHA, refSHA(t, mirrorDir, refnames.WorkBranch("wb-9c2f1a")))
		})
	}
}
