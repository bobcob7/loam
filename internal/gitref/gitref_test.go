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
