package gitpushsuite

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

// TestRejection5_ForcePush_RejectedByGitOwnDenyNonFastForwards_NotTheHook
// proves docs/git-spec.md -> "Ref Policy (push)"'s force-push case: git
// itself, via receive.denyNonFastForwards (installed on every mirror by
// internal/mirrorreconcile), rejects a non-fast-forward update -- NOT
// internal/refpolicy.EvaluatePush, which has no fast-forward check at all
// (see evaluateOne's own source: it only ever inspects Author and State,
// never OldSHA/NewSHA ancestry) and would happily ALLOW this exact update
// on its own merits (alice's own registered, draft branch). The proof
// this test needs, per this bead's own instructions, is that the
// rejection happens even with a VALID identity, role, and registered
// branch -- so the failure can only be attributed to git's config layer,
// not the hook silently doing its own thing: the callTracker shows the
// hook WAS consulted (twice: once for the initial create, once for the
// force-push attempt) and the rejection message carries no "loam:" prefix
// at all, unlike every hook-driven rejection in matrix_test.go.
func TestRejection5_ForcePush_RejectedByGitOwnDenyNonFastForwards_NotTheHook(t *testing.T) {
	t.Parallel()
	branches := map[string]workbranchstore.WorkBranch{
		"wb-good": {Name: "wb-good", Author: "alice", State: workbranchstore.StateDraft},
	}
	env := newStack(t, branches, loamhookBinary, true)
	clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
	commitFile(t, clonePath, "a.txt", "first commit")
	out, err := pushRef(t, clonePath, "refs/heads/wb-good")
	require.NoError(t, err, "the initial create must succeed: %s", out)
	firstSHA := mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-good")
	require.NotEmpty(t, firstSHA)

	runGit(t, clonePath, "commit", "--quiet", "--amend", "-m", "diverged, rewriting history")
	out, err = pushRefs(t, clonePath, "--force", "HEAD:refs/heads/wb-good")

	require.Error(t, err, "a force push must still be rejected: %s", out)
	assert.NotContains(t, out, "loam:", "this rejection must be git's OWN wording, never the hook's -- the hook has no opinion on fast-forward-ness at all")
	assert.Contains(t, out, "non-fast-forward", "git's own receive.denyNonFastForwards rejection names itself")
	assert.Equal(t, firstSHA, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-good"), "the mirror's ref must not have moved to the diverged commit")
	assert.Equal(t, []string{"wb-good", "wb-good"}, env.tracker.Calls(), "the hook ran for BOTH the initial create and the force-push attempt -- it had every opportunity to reject this on policy grounds and did not, because policy has no fast-forward rule")
}

// TestRejection6_Delete_RejectedByGitOwnDenyDeletes_NotTheHook mirrors the
// force-push case for deletion: internal/refpolicy.EvaluatePush never
// distinguishes "delete" (NewSHA all-zero) from any other update -- it
// checks the SAME author/state rules regardless -- so a delete of alice's
// own registered, draft branch is a case the hook itself would ALLOW.
// git's own receive.denyDeletes (also installed by
// internal/mirrorreconcile on every mirror) is what actually blocks it.
func TestRejection6_Delete_RejectedByGitOwnDenyDeletes_NotTheHook(t *testing.T) {
	t.Parallel()
	branches := map[string]workbranchstore.WorkBranch{
		"wb-good2": {Name: "wb-good2", Author: "alice", State: workbranchstore.StateDraft},
	}
	env := newStack(t, branches, loamhookBinary, true)
	clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
	commitFile(t, clonePath, "b.txt", "first commit")
	out, err := pushRef(t, clonePath, "refs/heads/wb-good2")
	require.NoError(t, err, "the initial create must succeed: %s", out)
	firstSHA := mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-good2")
	require.NotEmpty(t, firstSHA)

	out, err = pushRefs(t, clonePath, ":refs/heads/wb-good2")

	require.Error(t, err, "a delete push must still be rejected: %s", out)
	assert.NotContains(t, out, "loam:", "this rejection must be git's OWN wording, never the hook's -- the hook does not special-case a delete at all")
	assert.Contains(t, out, "denying ref deletion", "git's own receive.denyDeletes rejection names itself")
	assert.Equal(t, firstSHA, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-good2"), "the branch must still exist on the mirror, at its original commit -- never deleted")
	assert.Equal(t, []string{"wb-good2", "wb-good2"}, env.tracker.Calls(), "the hook ran for both the initial create and the delete attempt")
}
