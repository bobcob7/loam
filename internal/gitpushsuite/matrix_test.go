// TestPushMatrix is six of loam-li0.7's eight push-matrix cases (1-4, 7,
// 8; 5 and 6, force push and delete, are their own standalone tests in
// forcedelete_test.go, since they need a create-then-mutate sequence the
// rest of this table does not), driven by real `git` subprocesses against
// the EXACT production request chain (see fixture_test.go's newStack doc
// comment). Across all eight cases (this table plus forcedelete_test.go),
// four different mechanisms do the rejecting; only 1-4 are docs/git-
// spec.md's own four "Ref Policy (push)" reasons (docs/git-spec.md:148-
// 151) -- 7 and 8 are httpauth's and the role gate's own spec sections,
// not that table. Each row's own comment names which mechanism applies,
// plus exactly how this suite proves it is that mechanism and not some
// other "403 for some reason":
//
//   - Cases 1-4 (read-only ref, unknown ref, non-author, terminal state):
//     internal/refpolicy.EvaluatePush, reached through the real compiled
//     hook binary and a real hooksocket.Server -- proof: the byte-exact
//     "loam:"-prefixed reason from docs/git-spec.md:148-151's own pinned
//     table, AND callTracker recording that the branch WAS looked up (the
//     hook genuinely ran and made a decision). These four exact reasons,
//     over real git, are ALREADY proven by internal/hooksocket/e2e_test.go's
//     TestE2E_RejectedPushes_RealGitClientSeesRemotePrefixedLoamReason and,
//     at the EvaluatePush level, by internal/refpolicy/evaluate_test.go's
//     TestEvaluatePush_TableDriven -- this table does not re-derive them
//     from scratch; it re-asserts the same pinned strings through the ONE
//     layer neither of those suites adds: real Loam-Agent-* HTTP headers
//     (not context-injected identity) passing through a real
//     handler.GitRoleGate before ever reaching the hook.
//   - Cases 5-6 (force push, delete): git's own receive.denyNonFastForwards
//     / receive.denyDeletes, which internal/mirrorreconcile installs on
//     every mirror -- proof: the push is still rejected on a branch the
//     hook itself WOULD allow (registered, owned by the pusher, non-
//     terminal -- callTracker proves the hook was consulted and, per
//     crosscheck_test.go, genuinely permits deletes/non-fast-forwards on
//     an otherwise-valid branch), the rejection message carries NO
//     "loam:" prefix at all (git's own wording), and crosscheck_test.go's
//     TestCrossCheck_RemovingDenyConfig_OnlyThatCaseFlips independently
//     confirms flipping the specific git config flag is what changes the
//     outcome.
//   - Cases 7-8 (missing identity, wrong role): httpauth.GitIdentity and
//     handler.GitRoleGate respectively, BEFORE any git process runs at
//     all -- proof: callTracker.Calls() is empty, so the policy socket was
//     never even dialed, which is only possible if receive-pack itself
//     was never invoked.
package gitpushsuite

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

// registeredBranches is the fixed work-branch registry every case in this
// table (except the unknown-ref and read-only-ref cases, which
// deliberately push somewhere absent from it) shares.
func registeredBranches() map[string]workbranchstore.WorkBranch {
	return map[string]workbranchstore.WorkBranch{
		"wb-owned-by-bob": {Name: "wb-owned-by-bob", Author: bobIdentifier, State: workbranchstore.StateDraft},
		"wb-closed":       {Name: "wb-closed", Author: aliceIdentifier, State: workbranchstore.StateClosed},
		"wb-alice-draft":  {Name: "wb-alice-draft", Author: aliceIdentifier, State: workbranchstore.StateDraft},
	}
}

func TestPushMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		agentName      string
		agentID        string
		agentRole      string
		stripIdentity  bool
		ref            string
		wantReason     string // exact substring of the "remote: ..." line git relays
		wantLoamPrefix bool   // whether the rejection is "loam:"-prefixed -- Loam wrote it (hook, httpauth, or the role gate) -- or git's own wording (force push, delete)
		wantHookRan    bool   // whether callTracker must show the branch was looked up at all
	}{
		{
			// Rejection 1: read-only ref -- pushing straight to the
			// mirrored target branch "main". docs/git-spec.md:148:
			// "loam: refs/heads/main is read-only (target branch)".
			name:      "1 read-only ref: mirrored target branch main",
			agentName: "alice", agentID: "1", agentRole: "author",
			ref:            "refs/heads/main",
			wantReason:     "loam: refs/heads/main is read-only (target branch)",
			wantLoamPrefix: true,
			// "main" is still a refs/heads/ ref, so evaluateOne looks it up
			// (GetWorkBranch("main")) before discovering it is not a
			// registered work branch at all -- the hook DID run, it just
			// classified the existing, unregistered ref as read-only
			// rather than a create (refpolicy.unknownOrReadOnlyVerdict).
			wantHookRan: true,
		},
		{
			// Rejection 2: unknown ref -- creating a brand-new,
			// unregistered branch name. docs/git-spec.md:149:
			// "loam: refs/heads/foo is not a work branch; create one with 'work start'".
			name:      "2 unknown ref: never-registered branch name",
			agentName: "alice", agentID: "1", agentRole: "author",
			ref:            "refs/heads/wb-never-registered",
			wantReason:     "loam: refs/heads/wb-never-registered is not a work branch; create one with 'work start'",
			wantLoamPrefix: true,
			wantHookRan:    true,
		},
		{
			// Rejection 3: not the author -- alice pushing bob's branch.
			// docs/git-spec.md:150 template: "loam: <name> belongs to <author>".
			name:      "3 non-author: alice pushes bob's work branch",
			agentName: "alice", agentID: "1", agentRole: "author",
			ref:            "refs/heads/wb-owned-by-bob",
			wantReason:     "loam: wb-owned-by-bob belongs to bob",
			wantLoamPrefix: true,
			wantHookRan:    true,
		},
		{
			// Rejection 4: terminal state -- alice pushes her own CLOSED
			// branch. docs/git-spec.md:151 template: "loam: <name> is <state>".
			name:      "4 terminal state: alice's own closed branch",
			agentName: "alice", agentID: "1", agentRole: "author",
			ref:            "refs/heads/wb-closed",
			wantReason:     "loam: wb-closed is closed",
			wantLoamPrefix: true,
			wantHookRan:    true,
		},
		{
			// Rejection 7: missing identity headers -- a plain,
			// never-`loam clone`-configured git client. The CLONE step
			// itself still needs a valid identity (git.clone is gated
			// too), exactly as a real agent would first `loam clone`
			// successfully; clearIdentity then strips every extraheader
			// entry before the push, reproducing a client that lost or
			// never wrote its identity config. httpauth.GitIdentity 403s
			// before receive-pack even runs, so the hook process never
			// spawns and the policy socket is never dialed at all.
			name:      "7 missing identity: no Loam-Agent-* headers at all",
			agentName: "dave", agentID: "3", agentRole: "author",
			stripIdentity:  true,
			ref:            "refs/heads/wb-anything",
			wantReason:     "loam: forbidden: missing agent identity",
			wantLoamPrefix: true, // this IS loam:-prefixed (loam-j33), even though it never reaches the hook -- httpauth.GitIdentity writes it, not refpolicy
			wantHookRan:    false,
		},
		{
			// Rejection 8: wrong role -- a reviewer (git.clone only, no
			// git.push) attempting to push. handler.GitRoleGate 403s
			// before the git-receive-pack process even spawns.
			name:      "8 wrong role: reviewer lacks git.push",
			agentName: "carol", agentID: "2", agentRole: "reviewer",
			ref:            "refs/heads/wb-anything",
			wantReason:     `loam: role "reviewer" may not push (missing git.push capability)`,
			wantLoamPrefix: true,
			wantHookRan:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newStack(t, registeredBranches(), loamhookBinary, true)
			clonePath := cloneWithIdentity(t, env, tt.agentName, tt.agentID, tt.agentRole)
			if tt.stripIdentity {
				clearIdentity(t, clonePath)
			}
			commitFile(t, clonePath, "matrix.txt", "matrix push")
			before := mirrorRefSHA(t, env.mirrorDir, tt.ref)
			out, err := pushRef(t, clonePath, tt.ref)
			require.Error(t, err, "this push must be rejected: %s", out)
			assert.Contains(t, out, "remote: "+tt.wantReason, "git's own client must relay the exact reason")
			if tt.wantLoamPrefix {
				assert.Contains(t, out, "remote: loam:", "a hook/gate-driven rejection must be loam:-prefixed")
			}
			if tt.wantHookRan {
				assert.NotEmpty(t, env.tracker.Calls(), "the policy socket must have been consulted for this case")
			} else {
				assert.Empty(t, env.tracker.Calls(), "the policy socket must NEVER be dialed: this case is rejected before any git process runs")
			}
			assert.Equal(t, before, mirrorRefSHA(t, env.mirrorDir, tt.ref), "a rejected push must never move the target ref on the mirror, whether it pre-existed (main) or not (every other ref in this table)")
		})
	}
}
