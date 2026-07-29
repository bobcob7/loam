package gitpushsuite

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/refnames"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// This file pins the one consequence of moving work-branch refs into
// refnames.ReservedNamespace that an agent can actually trip over, and it
// is a behavioural CHANGE, not an invariant: `loam clone`'s config write
// is now LOAD-BEARING FOR PUSHES. A hand-rolled clone -- `git clone
// <url>`, no `loam clone` -- can still fetch and commit, but its pushes
// no longer land anywhere the server accepts, because plain git aims
// `git push origin wb-9c2f1a` at refs/heads/wb-9c2f1a while the
// registered ref is refs/heads/loam-reserved/wb-9c2f1a.
//
// Both halves run through the full production chain (see fixture_test.go's
// newStack): real git client, real HTTP identity headers, real role gate,
// real compiled pre-receive hook, real unix policy socket, real bare
// mirror. The two subtests differ in EXACTLY the two `git config` lines
// `loam clone` writes, which is what makes this a test of the bootstrap
// rather than of git in general.

// seedReservedWorkBranchRef points the mirror's reserved work-branch ref at
// main, standing in for what `work start` does server-side (internal/
// handler/workbranch's CreateWorkBranch).
func seedReservedWorkBranchRef(t *testing.T, mirrorDir, name string) string {
	t.Helper()
	runGit(t, "", "--git-dir="+mirrorDir, "update-ref", refnames.WorkBranch(name), "refs/heads/main")
	return mirrorRefSHA(t, mirrorDir, refnames.WorkBranch(name))
}

// bootstrapRefspecs writes the two refspecs `loam clone` writes (internal/
// cli's bootstrapWorkBranchRefspecs), through the same `git config` /
// `git config --add` distinction: push single-valued, fetch APPENDED so
// the clone's own refspec survives.
func bootstrapRefspecs(t *testing.T, clonePath string) {
	t.Helper()
	runGit(t, clonePath, "config", "remote.origin.push", refnames.ClientPushRefspec)
	runGit(t, clonePath, "config", "--add", "remote.origin.fetch", refnames.ClientFetchRefspec)
}

// pushByBranchName pushes with the destination-less refspec an agent
// actually types -- `git push origin wb-alice`, NOT the "HEAD:<full ref>"
// shape the rest of this suite uses. That distinction is the entire point:
// only a destination-less refspec consults remote.origin.push.
func pushByBranchName(t *testing.T, clonePath, branch string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "push", "origin", branch)
	cmd.Dir = clonePath
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestCloneBootstrap_RefspecsMakePlainPushReachTheReservedNamespace is the
// happy path the reserved namespace depends on: with `loam clone`'s two
// config lines in place, an agent's ordinary `git push origin wb-alice`
// is accepted by the real hook and lands on
// refs/heads/loam-reserved/wb-alice.
//
// The `git fetch` half matters too: --single-branch (and, here, a plain
// clone that predates the work branch) leaves a clone that cannot see the
// work branch at all until ClientFetchRefspec is added, and a reviewer
// cloning to check a branch out needs exactly that.
func TestCloneBootstrap_RefspecsMakePlainPushReachTheReservedNamespace(t *testing.T) {
	t.Parallel()
	branches := map[string]workbranchstore.WorkBranch{
		"wb-alice": {Name: "wb-alice", Author: aliceIdentifier, State: workbranchstore.StateDraft},
	}
	env := newStack(t, branches, loamhookBinary, true)
	seededSHA := seedReservedWorkBranchRef(t, env.mirrorDir, "wb-alice")
	clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
	bootstrapRefspecs(t, clonePath)

	runGit(t, clonePath, "fetch", "--quiet", "origin")
	remotes := runGit(t, clonePath, "branch", "-r")
	assert.Contains(t, remotes, "origin/wb-alice", "the added fetch refspec must bring the work branch down under its BARE name")

	runGit(t, clonePath, "checkout", "--quiet", "-b", "wb-alice", "origin/wb-alice")
	commitFile(t, clonePath, "work.txt", "alice's work")
	out, err := pushByBranchName(t, clonePath, "wb-alice")
	require.NoErrorf(t, err, "a bootstrapped clone's plain `git push origin wb-alice` must be accepted: %s", out)

	landed := mirrorRefSHA(t, env.mirrorDir, refnames.WorkBranch("wb-alice"))
	require.NotEmpty(t, landed)
	assert.NotEqual(t, seededSHA, landed, "the reserved ref must have advanced to the pushed commit")
	assert.Empty(t, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-alice"), "nothing may be created at the UNRESERVED path")
	assert.NotEmpty(t, env.tracker.Calls(), "the hook must genuinely have been consulted")
}

// TestCloneBootstrap_HandRolledClone_PushIsRejectedWithAPointedReason is
// the same push from a clone that never got `loam clone`'s config. It must
// be REJECTED -- that is the cost the reserved namespace buys -- and the
// reason must name the ref that would have worked, not the generic
// "create one with 'work start'", which would send an agent who already
// ran work start round in a circle.
func TestCloneBootstrap_HandRolledClone_PushIsRejectedWithAPointedReason(t *testing.T) {
	t.Parallel()
	branches := map[string]workbranchstore.WorkBranch{
		"wb-alice": {Name: "wb-alice", Author: aliceIdentifier, State: workbranchstore.StateDraft},
	}
	env := newStack(t, branches, loamhookBinary, true)
	seededSHA := seedReservedWorkBranchRef(t, env.mirrorDir, "wb-alice")
	clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
	// Deliberately NO bootstrapRefspecs: this is a hand-rolled clone.
	runGit(t, clonePath, "checkout", "--quiet", "-b", "wb-alice")
	commitFile(t, clonePath, "work.txt", "alice's work")

	out, err := pushByBranchName(t, clonePath, "wb-alice")
	require.Error(t, err, "without the push refspec, plain git aims at refs/heads/wb-alice, which is not a registered ref")
	assert.Contains(t, out, "remote: loam: wb-alice must be pushed to refs/heads/loam-reserved/wb-alice; re-run 'loam clone' to configure the push refspec")
	assert.Equal(t, seededSHA, mirrorRefSHA(t, env.mirrorDir, refnames.WorkBranch("wb-alice")), "the reserved ref must not have moved")
	assert.Empty(t, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-alice"), "and nothing may have landed at the unreserved path either")
}

// TestCloneBootstrap_FetchRefspecIsAppendedNotReplaced pins the one thing
// that would break a `loam clone` that used SetConfig for BOTH refspecs:
// `git clone --single-branch` writes the target branch's own fetch refspec
// into remote.origin.fetch, and replacing it leaves the clone unable to
// fetch the branch it was cloned at. This exercises the real single-branch
// shape, which cloneWithIdentity (a full clone) does not.
func TestCloneBootstrap_FetchRefspecIsAppendedNotReplaced(t *testing.T) {
	t.Parallel()
	env := newStack(t, nil, loamhookBinary, true)
	seedReservedWorkBranchRef(t, env.mirrorDir, "wb-alice")
	workspace := shortTempDir(t)
	clonePath := filepath.Join(workspace, "clone")
	runGit(t, workspace, "clone", "--quiet", "--branch", "main", "--single-branch",
		"--config", "http.extraheader=Loam-Agent-Name: alice",
		"--config", "http.extraheader=Loam-Agent-Id: 1",
		"--config", "http.extraheader=Loam-Agent-Role: author",
		env.srv.URL+"/git/acme/widgets.git", clonePath)
	bootstrapRefspecs(t, clonePath)

	got := runGit(t, clonePath, "config", "--get-all", "remote.origin.fetch")
	assert.Contains(t, got, "refs/heads/main", "the single-branch clone's own refspec must survive")
	assert.Contains(t, got, refnames.ClientFetchRefspec)

	runGit(t, clonePath, "fetch", "--quiet", "origin")
	remotes := runGit(t, clonePath, "branch", "-r")
	assert.Contains(t, remotes, "origin/main")
	assert.Contains(t, remotes, "origin/wb-alice")
}
