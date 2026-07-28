package gitpushsuite

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

// TestAtomicity_OneBadRefRejectsTheWholePush_GoodRefNeverLands is
// docs/git-spec.md -> "Ref Policy (push)"'s atomicity clause, proven
// through a REAL single `git push` invocation carrying TWO refspecs --
// one perfectly good (alice's own registered, draft work branch), one bad
// (the read-only mirrored target branch "main") -- so both ref updates
// travel to the server in ONE receive-pack session and ONE pre-receive
// hook invocation, exactly as a real client would send them together.
// internal/refpolicy/evaluate_test.go's own TestEvaluatePush_AtomicRejection
// already proves this at the EvaluatePush function level, in both ref
// orderings; what THIS test adds, and what that one structurally cannot,
// is the actual observable git-level consequence the bead's own
// instructions call out: "assert the good ref did NOT move on the mirror
// afterwards -- that is the assertion that actually proves it." A
// mutation that let EvaluatePush's caller apply the individually-allowed
// ref anyway (ignoring allAllowed) would satisfy the reason-string
// assertions alone but would still leave a real commit sitting on
// wb-good's mirror ref -- which is exactly what mirrorRefSHA below
// catches.
func TestAtomicity_OneBadRefRejectsTheWholePush_GoodRefNeverLands(t *testing.T) {
	t.Parallel()
	branches := map[string]workbranchstore.WorkBranch{
		"wb-good": {Name: "wb-good", Author: aliceIdentifier, State: workbranchstore.StateDraft},
	}
	env := newStack(t, branches, loamhookBinary, true)
	clonePath := cloneWithIdentity(t, env, "alice", "1", "author")
	commitFile(t, clonePath, "mixed.txt", "one good ref, one bad ref")
	require.Empty(t, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-good"), "wb-good must not exist on the mirror before this push")
	beforeMain := mirrorRefSHA(t, env.mirrorDir, "refs/heads/main")
	require.NotEmpty(t, beforeMain, "main must already exist (seeded)")
	out, err := pushRefs(t, clonePath, "HEAD:refs/heads/wb-good", "HEAD:refs/heads/main")
	require.Error(t, err, "a push with even one bad ref must be rejected as a whole: %s", out)
	assert.Contains(t, out, "remote: loam: refs/heads/main is read-only (target branch)", "the bad ref's own reason must still be relayed")
	assert.Empty(t, mirrorRefSHA(t, env.mirrorDir, "refs/heads/wb-good"), "the individually-good ref must NOT have landed on the mirror: pre-receive semantics reject the WHOLE push")
	assert.Equal(t, beforeMain, mirrorRefSHA(t, env.mirrorDir, "refs/heads/main"), "main's own tip must be completely unchanged")
	assert.ElementsMatch(t, []string{"wb-good", "main"}, env.tracker.Calls(), "the hook evaluated BOTH refs in one invocation before rejecting")
}
